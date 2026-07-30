package herdrbridge

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
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
