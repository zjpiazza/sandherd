package herdrbridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/zjpiazza/sandherd/internal/lifecycle"
	"github.com/zjpiazza/sandherd/internal/runner"
	"golang.org/x/term"
)

var (
	errReplayReset       = errors.New("terminal replay cursor reset")
	errControllerRevoked = errors.New("terminal controller was revoked")
	errAttention         = errors.New("terminal requires user attention")
)

type Bridge struct {
	config   Config
	api      *Client
	store    *StateStore
	herdr    *HerdrClient
	agentID  string
	paneID   string
	takeover bool
	input    *os.File
	output   io.Writer
	status   io.Writer

	inputBytes  chan []byte
	resize      chan runner.TerminalSize
	readOnce    sync.Once
	resizeDone  chan struct{}
	connectedAt time.Time
}

type BridgeOptions struct {
	Config   Config
	API      *Client
	Store    *StateStore
	Herdr    *HerdrClient
	AgentID  string
	PaneID   string
	Takeover bool
	Input    *os.File
	Output   io.Writer
	Status   io.Writer
}

func NewBridge(options BridgeOptions) (*Bridge, error) {
	if options.API == nil || options.Store == nil || options.Herdr == nil || !validAgentID(options.AgentID) || options.Input == nil || options.Output == nil || options.Status == nil {
		return nil, fmt.Errorf("API, state, Herdr, valid agent ID, and terminal streams are required")
	}
	return &Bridge{
		config: options.Config, api: options.API, store: options.Store, herdr: options.Herdr,
		agentID: options.AgentID, paneID: options.PaneID, takeover: options.Takeover,
		input: options.Input, output: options.Output, status: options.Status,
		inputBytes: make(chan []byte, 32), resize: make(chan runner.TerminalSize, 4), resizeDone: make(chan struct{}),
	}, nil
}

func (b *Bridge) Run(ctx context.Context) error {
	agent, err := b.waitUntilRunning(ctx)
	if err != nil {
		b.reportAttention(ctx, err)
		return err
	}
	state, loadErr := b.store.Load(b.agentID)
	if loadErr != nil {
		state = AgentState{AgentID: agent.ID, Name: agent.Name, BaseURL: b.api.BaseURL()}
	}
	state.Name = agent.Name
	state.BaseURL = b.api.BaseURL()
	state.PaneID = b.paneID
	if err := b.store.Save(state); err != nil {
		return err
	}
	_ = b.herdr.ReportMetadata(ctx, b.paneID, state)
	if term.IsTerminal(int(b.input.Fd())) {
		terminalState, rawErr := term.MakeRaw(int(b.input.Fd()))
		if rawErr != nil {
			return fmt.Errorf("put terminal in raw mode: %w", rawErr)
		}
		defer term.Restore(int(b.input.Fd()), terminalState)
	}
	readerContext, cancelReaders := context.WithCancel(ctx)
	b.startTerminalReaders(readerContext)
	defer func() {
		cancelReaders()
		<-b.resizeDone
	}()

	reconnectDeadline := time.Now().Add(b.config.ReconnectLimit)
	backoff := 250 * time.Millisecond
	for {
		attemptStarted := time.Now()
		err = b.runConnection(ctx, &state)
		if ctx.Err() != nil {
			return nil
		}
		if errors.Is(err, errControllerRevoked) || errors.Is(err, errAttention) {
			b.reportAttention(ctx, err)
			return err
		}
		if b.connectedAt.After(attemptStarted) {
			reconnectDeadline = time.Now().Add(b.config.ReconnectLimit)
			backoff = 250 * time.Millisecond
		}
		if errors.Is(err, errReplayReset) {
			backoff = 50 * time.Millisecond
		} else {
			if time.Now().After(reconnectDeadline) {
				wrapped := fmt.Errorf("terminal reconnect window expired: %w", err)
				b.reportAttention(ctx, wrapped)
				return wrapped
			}
			_ = b.herdr.ReportAgent(ctx, b.paneID, "working", "reconnecting")
			fmt.Fprintf(b.status, "\r\n[sandherd] connection lost; reconnecting: %v\r\n", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		if backoff < 5*time.Second {
			backoff *= 2
		}
	}
}

func (b *Bridge) waitUntilRunning(ctx context.Context) (lifecycle.Agent, error) {
	lastState := lifecycle.State("")
	for {
		agent, err := b.api.Get(ctx, b.agentID)
		if err != nil {
			return lifecycle.Agent{}, err
		}
		if agent.Status.State != lastState {
			fmt.Fprintf(b.status, "[sandherd] %s is %s\r\n", agent.Name, agent.Status.State)
			lastState = agent.Status.State
		}
		switch agent.Status.State {
		case lifecycle.StateRunning:
			return agent, nil
		case lifecycle.StateRequested, lifecycle.StateProvisioning, lifecycle.StateStarting:
			_ = b.herdr.ReportAgent(ctx, b.paneID, "working", string(agent.Status.State))
		case lifecycle.StateStopped:
			return lifecycle.Agent{}, fmt.Errorf("agent is stopped; use the Sandherd Resume action")
		case lifecycle.StateFailed:
			return lifecycle.Agent{}, fmt.Errorf("agent failed: %s", agent.Status.Message)
		case lifecycle.StateStopping:
			return lifecycle.Agent{}, fmt.Errorf("agent is stopping; resume it after it reaches stopped")
		case lifecycle.StateDeleting:
			return lifecycle.Agent{}, fmt.Errorf("agent is being deleted")
		}
		select {
		case <-ctx.Done():
			return lifecycle.Agent{}, ctx.Err()
		case <-time.After(b.config.PollInterval):
		}
	}
}

func (b *Bridge) startTerminalReaders(ctx context.Context) {
	b.readOnce.Do(func() {
		go func() {
			buffer := make([]byte, 32*1024)
			for {
				count, err := b.input.Read(buffer)
				if count > 0 {
					data := append([]byte(nil), buffer[:count]...)
					select {
					case b.inputBytes <- data:
					case <-ctx.Done():
						return
					}
				}
				if err != nil {
					return
				}
			}
		}()
		resizeSignals := make(chan os.Signal, 4)
		signal.Notify(resizeSignals, syscall.SIGWINCH)
		go func() {
			defer close(b.resizeDone)
			defer signal.Stop(resizeSignals)
			b.publishSize()
			for {
				select {
				case <-resizeSignals:
					b.publishSize()
				case <-ctx.Done():
					return
				}
			}
		}()
	})
}

func (b *Bridge) publishSize() {
	size := b.terminalSize()
	select {
	case b.resize <- size:
	default:
		select {
		case <-b.resize:
		default:
		}
		select {
		case b.resize <- size:
		default:
		}
	}
}

func (b *Bridge) terminalSize() runner.TerminalSize {
	columns, rows := 80, 24
	if term.IsTerminal(int(b.input.Fd())) {
		if width, height, err := term.GetSize(int(b.input.Fd())); err == nil && width > 0 && height > 0 {
			columns, rows = width, height
		}
	}
	if columns > 1000 {
		columns = 1000
	}
	if rows > 1000 {
		rows = 1000
	}
	return runner.TerminalSize{Columns: uint16(columns), Rows: uint16(rows)}
}

func (b *Bridge) runConnection(ctx context.Context, state *AgentState) error {
	authorization, err := b.api.AuthorizationHeader()
	if err != nil {
		return err
	}
	headers := http.Header{"Authorization": []string{authorization}, "X-Request-ID": []string{lifecycle.NewID()}}
	connection, response, err := websocket.Dial(ctx, b.api.TerminalURL(b.agentID), &websocket.DialOptions{HTTPHeader: headers, Subprotocols: []string{runner.Protocol}})
	if response != nil && response.Body != nil {
		defer response.Body.Close()
	}
	if err != nil {
		if response != nil {
			apiErr := decodeAPIError(response)
			if IsAPIError(apiErr, "unsupported_protocol") {
				return fmt.Errorf("%w: Sandherd terminal protocol is newer than this bridge; upgrade the plugin: %v", errAttention, apiErr)
			}
			if IsAPIError(apiErr, "unauthenticated") || IsAPIError(apiErr, "forbidden_role") {
				return fmt.Errorf("%w: %v", errAttention, apiErr)
			}
			return apiErr
		}
		return err
	}
	defer connection.CloseNow()
	connection.SetReadLimit(1024 * 1024)
	if connection.Subprotocol() != runner.Protocol {
		return fmt.Errorf("server did not negotiate %s; upgrade Sandherd and this plugin together", runner.Protocol)
	}
	attach := runner.Frame{Type: "attach", ProtocolVersion: runner.ProtocolVersion, Role: "control", AfterSequence: state.AfterSequence, Takeover: b.takeover, TerminalSize: sizePointer(b.terminalSize())}
	if err := writeTerminalFrame(ctx, connection, attach); err != nil {
		return err
	}
	first, err := readTerminalFrame(ctx, connection)
	if err != nil {
		return err
	}
	if first.Type == "error" {
		return bridgeProtocolError(first)
	}
	if first.Type != "attached" || first.Role != "control" || first.LeaseID == "" {
		return fmt.Errorf("runner returned an invalid attachment response")
	}
	if state.RunnerGeneration != "" && state.RunnerGeneration != first.RunnerGeneration && state.AfterSequence != nil {
		state.AfterSequence = nil
		state.RunnerGeneration = first.RunnerGeneration
		_ = b.store.Save(*state)
		return errReplayReset
	}
	state.RunnerGeneration = first.RunnerGeneration
	_ = b.store.Save(*state)
	b.connectedAt = time.Now()
	_ = b.herdr.ReportAgent(ctx, b.paneID, "idle", "connected")
	leaseID := first.LeaseID

	connectionContext, cancel := context.WithCancel(ctx)
	defer cancel()
	frames := make(chan runner.Frame, 16)
	readErrors := make(chan error, 1)
	go func() {
		for {
			frame, readErr := readTerminalFrame(connectionContext, connection)
			if readErr != nil {
				readErrors <- readErr
				return
			}
			select {
			case frames <- frame:
			case <-connectionContext.Done():
				return
			}
		}
	}()
	commands := make(chan runner.Frame, 32)
	writeErrors := make(chan error, 1)
	go b.writeTerminal(connectionContext, connection, leaseID, commands, writeErrors)
	idleTimer := time.NewTimer(time.Second)
	defer idleTimer.Stop()
	for {
		select {
		case err := <-readErrors:
			return err
		case err := <-writeErrors:
			return err
		case <-idleTimer.C:
			_ = b.herdr.ReportAgent(ctx, b.paneID, "idle", "ready")
		case frame := <-frames:
			switch frame.Type {
			case "attached":
				if frame.Role == "control" && frame.LeaseID != "" {
					leaseID = frame.LeaseID
				}
			case "output":
				if _, err := b.output.Write(frame.Data); err != nil {
					return err
				}
				if frame.Sequence != nil {
					sequence := *frame.Sequence
					state.AfterSequence = &sequence
					_ = b.store.Save(*state)
					queueFrame(commands, runner.Frame{Type: "ack", Sequence: &sequence})
				}
				resetTimer(idleTimer, time.Second)
			case "replay_gap":
				fmt.Fprintln(b.status, "\r\n[sandherd] replay buffer expired; output starts at the earliest available sequence\r")
			case "controller_revoked":
				return fmt.Errorf("%w: %s", errControllerRevoked, frame.Reason)
			case "exit":
				_ = b.herdr.ReportAgent(ctx, b.paneID, "idle", "agent exited")
				_ = b.herdr.Notify(ctx, state.Name+" finished", "The remote agent process exited.", "done")
			case "error":
				if frame.Code == "replay_cursor_invalid" {
					state.AfterSequence = nil
					_ = b.store.Save(*state)
					return errReplayReset
				}
				return bridgeProtocolError(frame)
			case "ping":
				queueFrame(commands, runner.Frame{Type: "pong", Nonce: frame.Nonce})
			case "pong":
			default:
				return fmt.Errorf("unsupported terminal frame %q; upgrade the plugin", frame.Type)
			}
		case <-ctx.Done():
			return nil
		}
	}
}

func (b *Bridge) writeTerminal(ctx context.Context, connection *websocket.Conn, leaseID string, commands <-chan runner.Frame, result chan<- error) {
	for {
		var frame runner.Frame
		select {
		case data := <-b.inputBytes:
			frame = runner.Frame{Type: "input", LeaseID: leaseID, Data: data}
			_ = b.herdr.ReportAgent(ctx, b.paneID, "working", "agent working")
		case size := <-b.resize:
			frame = runner.Frame{Type: "resize", LeaseID: leaseID, TerminalSize: &size}
		case frame = <-commands:
		case <-ctx.Done():
			return
		}
		if err := writeTerminalFrame(ctx, connection, frame); err != nil {
			select {
			case result <- err:
			default:
			}
			return
		}
	}
}

func (b *Bridge) reportAttention(ctx context.Context, err error) {
	_ = b.herdr.ReportAgent(ctx, b.paneID, "blocked", "attention required")
	_ = b.herdr.Notify(ctx, "Sandherd needs attention", err.Error(), "request")
}

func readTerminalFrame(ctx context.Context, connection *websocket.Conn) (runner.Frame, error) {
	messageType, payload, err := connection.Read(ctx)
	if err != nil {
		return runner.Frame{}, err
	}
	if messageType != websocket.MessageText {
		return runner.Frame{}, fmt.Errorf("terminal server sent a non-text frame")
	}
	var frame runner.Frame
	if err := json.Unmarshal(payload, &frame); err != nil || frame.Type == "" {
		return runner.Frame{}, fmt.Errorf("terminal server sent invalid JSON")
	}
	return frame, nil
}

func writeTerminalFrame(ctx context.Context, connection *websocket.Conn, frame runner.Frame) error {
	payload, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	writeContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return connection.Write(writeContext, websocket.MessageText, payload)
}

func bridgeProtocolError(frame runner.Frame) error {
	switch frame.Code {
	case "controller_busy":
		return fmt.Errorf("%w: another pane controls this agent; focus it or reopen with takeover", errAttention)
	case "forbidden_role":
		return fmt.Errorf("%w: this Sandherd credential cannot control the agent", errAttention)
	case "unsupported_protocol":
		return fmt.Errorf("%w: terminal protocol mismatch; upgrade the Sandherd Herdr plugin", errAttention)
	default:
		return fmt.Errorf("terminal error %s: %s (request %s)", frame.Code, frame.Message, frame.RequestID)
	}
}

func sizePointer(size runner.TerminalSize) *runner.TerminalSize { return &size }

func queueFrame(channel chan<- runner.Frame, frame runner.Frame) {
	select {
	case channel <- frame:
	default:
	}
}

func resetTimer(timer *time.Timer, duration time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(duration)
}
