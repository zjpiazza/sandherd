package herdrbridge

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/zjpiazza/sandherd/internal/lifecycle"
	"github.com/zjpiazza/sandherd/internal/runner"
)

func TestBridgeReconnectsReplaysResizesAndForwardsArbitraryBytes(t *testing.T) {
	var connections atomic.Int32
	var getCalls atomic.Int32
	var stopCalls atomic.Int32
	var observedInput atomic.Bool
	var observedResize atomic.Bool
	var observedPong atomic.Bool
	secondAcknowledged := make(chan struct{}, 1)
	attachCursors := make(chan *uint64, 2)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/v1alpha1/agents/"+bridgeTestAgentID && request.Method == http.MethodGet {
			call := getCalls.Add(1)
			state := lifecycle.StateRunning
			if call == 1 {
				state = lifecycle.StateProvisioning
			}
			_ = json.NewEncoder(response).Encode(lifecycle.Agent{ID: bridgeTestAgentID, Name: "alpha", Status: lifecycle.AgentStatus{State: state}})
			return
		}
		if request.Method == http.MethodPost || request.Method == http.MethodDelete {
			stopCalls.Add(1)
			http.Error(response, "unexpected lifecycle mutation", http.StatusBadRequest)
			return
		}
		if request.URL.Path != "/v1alpha1/agents/"+bridgeTestAgentID+"/terminal" {
			http.NotFound(response, request)
			return
		}
		connectionNumber := connections.Add(1)
		connection, err := websocket.Accept(response, request, &websocket.AcceptOptions{Subprotocols: []string{runner.Protocol}})
		if err != nil {
			return
		}
		defer connection.CloseNow()
		attach, err := readTerminalFrame(request.Context(), connection)
		if err != nil {
			return
		}
		attachCursors <- attach.AfterSequence
		zero, latest := uint64(0), uint64(connectionNumber)
		attached := runner.Frame{
			Type: "attached", AttachmentID: "019c09f2-34c1-7ee0-9c66-d52919d67381", AgentID: bridgeTestAgentID,
			Role: "control", LeaseID: "019c09f2-34c1-7ee0-9c66-d52919d67382", RunnerGeneration: "019c09f2-34c1-7ee0-9c66-d52919d67383",
			EarliestSequence: &zero, LatestSequence: &latest, ProcessState: "running",
		}
		if err := writeTerminalFrame(request.Context(), connection, attached); err != nil {
			return
		}
		if connectionNumber == 1 {
			_ = writeTerminalFrame(request.Context(), connection, runner.Frame{Type: "ping", Nonce: "server-ping"})
		}
		sequence := uint64(connectionNumber)
		data := []byte{0x00, 0xff, 0x1b, byte('0' + connectionNumber)}
		if err := writeTerminalFrame(request.Context(), connection, runner.Frame{Type: "output", Sequence: &sequence, Data: data}); err != nil {
			return
		}
		for {
			frame, err := readTerminalFrame(request.Context(), connection)
			if err != nil {
				return
			}
			switch frame.Type {
			case "input":
				if bytes.Equal(frame.Data, []byte{0x00, 0xfe, 'x'}) {
					observedInput.Store(true)
				}
			case "resize":
				if frame.TerminalSize != nil && frame.TerminalSize.Columns > 0 && frame.TerminalSize.Rows > 0 {
					observedResize.Store(true)
				}
			case "pong":
				if frame.Nonce == "server-ping" {
					observedPong.Store(true)
				}
			case "ack":
				if frame.Sequence != nil && *frame.Sequence == sequence {
					if connectionNumber == 1 {
						_ = connection.CloseNow()
						return
					}
					select {
					case secondAcknowledged <- struct{}{}:
					default:
					}
					<-request.Context().Done()
					return
				}
			}
		}
	}))
	defer server.Close()

	configuration := testConfig(t, server.URL)
	api, _ := NewClient(configuration, server.Client())
	store, _ := NewStateStore(t.TempDir())
	commandRunner := &recordingCommandRunner{}
	herdr := NewHerdrClient(commandRunner)
	input, inputWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	defer inputWriter.Close()
	var output bytes.Buffer
	var status bytes.Buffer
	bridge, err := NewBridge(BridgeOptions{
		Config: configuration, API: api, Store: store, Herdr: herdr,
		AgentID: bridgeTestAgentID, PaneID: "w1:p2", Input: input, Output: &output, Status: &status,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- bridge.Run(ctx) }()
	if _, err := inputWriter.Write([]byte{0x00, 0xfe, 'x'}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-secondAcknowledged:
		cancel()
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("bridge did not reconnect and acknowledge second output")
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("bridge result = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("bridge did not stop after context cancellation")
	}

	firstCursor := <-attachCursors
	secondCursor := <-attachCursors
	if firstCursor != nil || secondCursor == nil || *secondCursor != 1 {
		t.Fatalf("attach cursors = %#v then %#v", firstCursor, secondCursor)
	}
	wantOutput := []byte{0x00, 0xff, 0x1b, '1', 0x00, 0xff, 0x1b, '2'}
	if !bytes.Equal(output.Bytes(), wantOutput) {
		t.Fatalf("terminal output = %v, want %v", output.Bytes(), wantOutput)
	}
	state, err := store.Load(bridgeTestAgentID)
	if err != nil || state.AfterSequence == nil || *state.AfterSequence != 2 || state.PaneID != "w1:p2" {
		t.Fatalf("saved state = %#v, error %v", state, err)
	}
	if !observedInput.Load() || !observedResize.Load() || !observedPong.Load() {
		t.Fatalf("input=%v resize=%v pong=%v", observedInput.Load(), observedResize.Load(), observedPong.Load())
	}
	if stopCalls.Load() != 0 {
		t.Fatalf("terminal detach made %d lifecycle mutations", stopCalls.Load())
	}
	commands := commandRunner.joined()
	if !strings.Contains(commands, "--state working") || !strings.Contains(commands, "--state idle") || !strings.Contains(commands, "sandherd_agent_id="+bridgeTestAgentID) {
		t.Fatalf("Herdr state commands = %s", commands)
	}
	if !strings.Contains(status.String(), "provisioning") || !strings.Contains(status.String(), "reconnecting") {
		t.Fatalf("status output = %q", status.String())
	}
}

func TestBridgeSurfacesProtocolUpgradeAndTakeover(t *testing.T) {
	var sawTakeover atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/v1alpha1/agents/"+bridgeTestAgentID {
			_ = json.NewEncoder(response).Encode(lifecycle.Agent{ID: bridgeTestAgentID, Name: "alpha", Status: lifecycle.AgentStatus{State: lifecycle.StateRunning}})
			return
		}
		connection, err := websocket.Accept(response, request, &websocket.AcceptOptions{Subprotocols: []string{runner.Protocol}})
		if err != nil {
			return
		}
		defer connection.CloseNow()
		attach, err := readTerminalFrame(request.Context(), connection)
		if err != nil {
			return
		}
		sawTakeover.Store(attach.Takeover)
		_ = writeTerminalFrame(request.Context(), connection, runner.Frame{Type: "error", Code: "unsupported_protocol", Message: "upgrade", RequestID: "request", Retryable: false})
	}))
	defer server.Close()
	configuration := testConfig(t, server.URL)
	api, _ := NewClient(configuration, server.Client())
	store, _ := NewStateStore(t.TempDir())
	commands := &recordingCommandRunner{}
	input, writer, _ := os.Pipe()
	defer input.Close()
	defer writer.Close()
	bridge, _ := NewBridge(BridgeOptions{
		Config: configuration, API: api, Store: store, Herdr: NewHerdrClient(commands), AgentID: bridgeTestAgentID,
		PaneID: "w1:p2", Takeover: true, Input: input, Output: &bytes.Buffer{}, Status: &bytes.Buffer{},
	})
	err := bridge.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "upgrade") || !sawTakeover.Load() {
		t.Fatalf("bridge error = %v, takeover=%v", err, sawTakeover.Load())
	}
	if !strings.Contains(commands.joined(), "notification show Sandherd needs attention") || !strings.Contains(commands.joined(), "--state blocked") {
		t.Fatalf("attention commands = %s", commands.joined())
	}
}

func TestBridgeResetsStaleCursorRejectedDuringAttachment(t *testing.T) {
	configuration := testConfig(t, "")
	target, err := url.Parse(startBridgeTestRunner(t, configuration.TokenFile))
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		request.URL.Path = "/v1alpha1/terminal"
		proxy.ServeHTTP(response, request)
	}))
	defer server.Close()
	configuration.BaseURL = server.URL
	api, _ := NewClient(configuration, server.Client())
	store, _ := NewStateStore(t.TempDir())
	cursor := uint64(1_000_000)
	state := AgentState{AgentID: bridgeTestAgentID, Name: "resumed", BaseURL: server.URL,
		PaneID: "w1:p2", RunnerGeneration: "previous-runner", AfterSequence: &cursor}
	if err := store.Save(state); err != nil {
		t.Fatal(err)
	}
	input, writer, _ := os.Pipe()
	defer input.Close()
	defer writer.Close()
	outputReader, outputWriter := io.Pipe()
	defer outputReader.Close()
	defer outputWriter.Close()
	bridge, err := NewBridge(BridgeOptions{
		Config: configuration, API: api, Store: store, Herdr: NewHerdrClient(&recordingCommandRunner{}),
		AgentID: bridgeTestAgentID, PaneID: state.PaneID, Input: input, Output: outputWriter, Status: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// A fresh real runner rejects a cursor from the previous process before it
	// sends "attached", so the bridge cannot first compare runner generations.
	if err := bridge.runConnection(ctx, &state); !errors.Is(err, errReplayReset) {
		t.Fatalf("stale attach returned %v, want cursor reset", err)
	}
	saved, err := store.Load(bridgeTestAgentID)
	if err != nil || saved.AfterSequence != nil || state.AfterSequence != nil {
		t.Fatalf("stale cursor was not cleared durably: %#v, %v", saved, err)
	}
	handled := make(chan struct{})
	go func() {
		scanner := bufio.NewScanner(outputReader)
		for scanner.Scan() {
			if strings.TrimSpace(scanner.Text()) == "handled:after-resume" {
				close(handled)
				return
			}
		}
	}()
	bridge.inputBytes <- []byte("after-resume\n")
	result := make(chan error, 1)
	go func() { result <- bridge.runConnection(ctx, &state) }()
	select {
	case <-handled:
	case err := <-result:
		t.Fatalf("reattachment exited before input was handled: %v", err)
	case <-ctx.Done():
		t.Fatal("replacement runner did not handle input")
	}
	cancel()
	select {
	case <-result:
	case <-time.After(time.Second):
		t.Fatal("reattachment did not stop")
	}
	if state.RunnerGeneration == "previous-runner" || state.RunnerGeneration == "" {
		t.Fatalf("replacement runner generation was not saved: %q", state.RunnerGeneration)
	}
}

func TestBridgeIdleLeaseSurvivesAndHonorsTakeoverAndCancellation(t *testing.T) {
	configuration := testConfig(t, "")
	runnerURL := startBridgeTestRunner(t, configuration.TokenFile)
	target, err := url.Parse(runnerURL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	var connections atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v1alpha1/agents/" + bridgeTestAgentID:
			_ = json.NewEncoder(response).Encode(lifecycle.Agent{ID: bridgeTestAgentID, Name: "idle", Status: lifecycle.AgentStatus{State: lifecycle.StateRunning}})
		case "/v1alpha1/agents/" + bridgeTestAgentID + "/terminal":
			connections.Add(1)
			request.URL.Path = "/v1alpha1/terminal"
			proxy.ServeHTTP(response, request)
		default:
			http.NotFound(response, request)
		}
	}))
	t.Cleanup(server.Close)
	configuration.BaseURL = server.URL

	startBridge := func(takeover bool) (*os.File, <-chan string, <-chan error, context.CancelFunc) {
		t.Helper()
		api, err := NewClient(configuration, server.Client())
		if err != nil {
			t.Fatal(err)
		}
		store, err := NewStateStore(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		input, inputWriter, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		outputReader, outputWriter := io.Pipe()
		lines := make(chan string, 32)
		go func() {
			scanner := bufio.NewScanner(outputReader)
			for scanner.Scan() {
				lines <- scanner.Text()
			}
		}()
		bridge, err := NewBridge(BridgeOptions{
			Config: configuration, API: api, Store: store, Herdr: NewHerdrClient(&recordingCommandRunner{}),
			AgentID: bridgeTestAgentID, PaneID: "w1:p2", Takeover: takeover,
			Input: input, Output: outputWriter, Status: io.Discard,
		})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan error, 1)
		done := make(chan struct{})
		go func() {
			defer close(done)
			defer outputWriter.Close()
			result <- bridge.Run(ctx)
		}()
		t.Cleanup(func() {
			cancel()
			_ = inputWriter.Close()
			_ = input.Close()
			_ = outputReader.Close()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("bridge did not stop during cleanup")
			}
		})
		return inputWriter, lines, result, cancel
	}
	awaitHandledInput := func(input *os.File, lines <-chan string, result <-chan error, text string) {
		t.Helper()
		if _, err := io.WriteString(input, text+"\n"); err != nil {
			t.Fatal(err)
		}
		timeout := time.NewTimer(5 * time.Second)
		defer timeout.Stop()
		for {
			select {
			case line := <-lines:
				if strings.TrimSpace(line) == "handled:"+text {
					return
				}
			case err := <-result:
				t.Fatalf("bridge exited before agent handled input: %v", err)
			case <-timeout.C:
				t.Fatalf("agent did not handle %q", text)
			}
		}
	}

	input, lines, result, _ := startBridge(false)
	awaitHandledInput(input, lines, result, "before-idle")
	// Use the real runner's default 30-second lease. No input, output, resize,
	// or server-initiated ping can keep this attachment alive during the wait.
	idle := time.NewTimer(32 * time.Second)
	defer idle.Stop()
	select {
	case err := <-result:
		t.Fatalf("idle bridge lost its controller lease: %v", err)
	case <-idle.C:
	}
	awaitHandledInput(input, lines, result, "after-idle")
	if got := connections.Load(); got != 1 {
		t.Fatalf("idle retention reattached %d times instead of renewing its lease", got)
	}

	secondInput, secondLines, secondResult, cancelSecond := startBridge(true)
	awaitHandledInput(secondInput, secondLines, secondResult, "after-takeover")
	select {
	case err := <-result:
		if !errors.Is(err, errControllerRevoked) || !strings.Contains(err.Error(), "takeover") {
			t.Fatalf("replaced controller result = %v, want takeover revocation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("keepalive prevented intentional takeover")
	}
	cancelSecond()
	select {
	case err := <-secondResult:
		if err != nil {
			t.Fatalf("cancelled bridge result = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("keepalive prevented bridge cancellation")
	}
	if got := connections.Load(); got != 2 {
		t.Fatalf("revocation or cancellation unexpectedly reconnected: %d connections", got)
	}
}

// Run the actual runner and PTY in a subprocess so its signal handling and
// shutdown cannot interfere with the test process.
func TestBridgeRunnerHelper(t *testing.T) {
	if os.Getenv("SANDHERD_BRIDGE_RUNNER_HELPER") != "1" {
		return
	}
	for index, argument := range os.Args {
		if argument == "--" {
			os.Exit(runner.Run(os.Args[index+1:], os.Stdout, os.Stderr))
		}
	}
	t.Fatal("runner helper arguments are missing")
}

func startBridgeTestRunner(t *testing.T, tokenFile string) string {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable, "-test.run=^TestBridgeRunnerHelper$", "--",
		"--agent-id", bridgeTestAgentID, "--listen", "127.0.0.1:0", "--auth-token-file", tokenFile,
		"--working-directory", t.TempDir(), "--stop-grace", "100ms",
		"--", "/bin/sh", "-c", `while IFS= read -r line; do printf 'handled:%s\n' "$line"; done`)
	command.Env = append(os.Environ(), "SANDHERD_BRIDGE_RUNNER_HELPER=1", `SANDHERD_AGENT_HEALTH_CHECK_JSON=["/bin/true"]`)
	logs, err := command.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	addresses := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(logs)
		for scanner.Scan() {
			var entry struct {
				Address string `json:"address"`
			}
			if json.Unmarshal(scanner.Bytes(), &entry) == nil && entry.Address != "" {
				select {
				case addresses <- entry.Address:
				default:
				}
			}
		}
	}()
	exited := make(chan struct{})
	var exitErr error
	go func() {
		exitErr = command.Wait()
		close(exited)
	}()
	t.Cleanup(func() {
		_ = command.Process.Signal(syscall.SIGTERM)
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
			_ = command.Process.Kill()
			<-exited
			t.Error("runner did not shut down")
		}
	})
	select {
	case address := <-addresses:
		return "http://" + address
	case <-exited:
		t.Fatalf("runner exited before listening: %v", exitErr)
	case <-time.After(10 * time.Second):
		t.Fatal("runner did not publish its listening address")
	}
	return ""
}

func TestRecordingCommandRunnerIsRaceSafe(t *testing.T) {
	runner := &recordingCommandRunner{}
	client := NewHerdrClient(runner)
	var wait sync.WaitGroup
	for range 4 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_ = client.ReportAgent(context.Background(), "w1:p1", "working", "test")
		}()
	}
	wait.Wait()
}
