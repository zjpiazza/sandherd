package runner

import (
	"fmt"
	"sync"
	"time"
)

type Permissions struct {
	Observe bool
	Control bool
}

type attachRequest struct {
	Role          string
	AfterSequence *uint64
	Takeover      bool
	TerminalSize  TerminalSize
	Permissions   Permissions
}

type subscription struct {
	id          string
	role        string
	leaseID     string
	permissions Permissions
	initial     []Frame
	queue       chan Frame
	done        chan struct{}

	closeOnce sync.Once
	reasonMu  sync.Mutex
	reason    Frame
}

func (s *subscription) close(reason Frame) {
	s.closeOnce.Do(func() {
		s.reasonMu.Lock()
		s.reason = reason
		s.reasonMu.Unlock()
		close(s.done)
	})
}

func (s *subscription) closeReason() Frame {
	s.reasonMu.Lock()
	defer s.reasonMu.Unlock()
	return s.reason
}

type controllerLease struct {
	attachmentID string
	leaseID      string
	expiresAt    time.Time
}

type controlledProcess interface {
	write([]byte) error
	resize(TerminalSize) error
	snapshot() (int, time.Time, *processExit)
}

type hub struct {
	mu               sync.Mutex
	controlMu        sync.Mutex
	agentID          string
	generation       string
	replay           *replayBuffer
	subscribers      map[string]*subscription
	controller       *controllerLease
	controllerTTL    time.Duration
	subscriberBuffer int
	state            string
	exit             *Frame
	process          controlledProcess
	closed           chan struct{}
}

func newHub(agentID string, replayBytes, replayFrames, subscriberBuffer int, controllerTTL time.Duration) *hub {
	return &hub{
		agentID:          agentID,
		generation:       newID(),
		replay:           newReplayBuffer(replayBytes, replayFrames),
		subscribers:      make(map[string]*subscription),
		controllerTTL:    controllerTTL,
		subscriberBuffer: subscriberBuffer,
		state:            "starting",
		closed:           make(chan struct{}),
	}
}

func (h *hub) setProcess(process controlledProcess) {
	h.mu.Lock()
	h.process = process
	if h.state == "starting" {
		h.state = "running"
	}
	h.mu.Unlock()
}

func (h *hub) publishOutput(data []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	frame := h.replay.append(data)
	h.broadcastLocked(frame)
}

func (h *hub) publishExit(exit processExit) {
	h.controlMu.Lock()
	defer h.controlMu.Unlock()
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.exit != nil {
		return
	}
	h.state = "exited"
	frame := Frame{
		Type:             "exit",
		RunnerGeneration: h.generation,
		ExitCode:         exit.ExitCode,
		Signal:           exit.Signal,
		FinishedAt:       exit.FinishedAt.Format(time.RFC3339Nano),
	}
	h.exit = &frame
	h.revokeControllerLocked("agent_stopped")
	h.broadcastLocked(frame)
}

func (h *hub) attach(request attachRequest) (*subscription, error) {
	if request.Role != "control" && request.Role != "observe" {
		return nil, fmt.Errorf("forbidden_role: role must be control or observe")
	}
	if err := request.TerminalSize.Validate(); err != nil {
		return nil, fmt.Errorf("invalid_terminal_size: %w", err)
	}
	if !request.Permissions.Observe {
		return nil, fmt.Errorf("forbidden_role: observe permission is required")
	}
	if request.Role == "control" && !request.Permissions.Control {
		return nil, fmt.Errorf("forbidden_role: control permission is required")
	}

	h.controlMu.Lock()
	defer h.controlMu.Unlock()
	h.mu.Lock()
	defer h.mu.Unlock()
	h.expireControllerLocked(time.Now())
	if request.AfterSequence != nil {
		_, _, valid := h.replay.after(*request.AfterSequence)
		if !valid {
			return nil, fmt.Errorf("replay_cursor_invalid: cursor is ahead of the runner")
		}
	}
	if request.Role == "control" && h.controller != nil && !request.Takeover {
		return nil, fmt.Errorf("controller_busy: another controller holds the lease")
	}
	if request.Role == "control" && h.state == "running" {
		if err := h.process.resize(request.TerminalSize); err != nil {
			return nil, fmt.Errorf("invalid_resize: %w", err)
		}
	}

	sub := &subscription{
		id:          newID(),
		role:        request.Role,
		permissions: request.Permissions,
		queue:       make(chan Frame, h.subscriberBuffer),
		done:        make(chan struct{}),
	}
	if request.Role == "control" {
		if request.Takeover {
			h.revokeControllerLocked("takeover")
		}
		h.grantControllerLocked(sub)
	}

	earliest, latest := h.replay.bounds()
	attached := Frame{
		Type:             "attached",
		AttachmentID:     sub.id,
		AgentID:          h.agentID,
		Role:             sub.role,
		LeaseID:          sub.leaseID,
		RunnerGeneration: h.generation,
		EarliestSequence: &earliest,
		LatestSequence:   &latest,
		ProcessState:     h.state,
	}
	sub.initial = append(sub.initial, attached)
	if request.AfterSequence != nil {
		frames, gap, _ := h.replay.after(*request.AfterSequence)
		if gap {
			requested := *request.AfterSequence
			sub.initial = append(sub.initial, Frame{
				Type:                   "replay_gap",
				RequestedAfterSequence: &requested,
				EarliestSequence:       &earliest,
				LatestSequence:         &latest,
			})
		}
		sub.initial = append(sub.initial, frames...)
	}
	if h.exit != nil {
		sub.initial = append(sub.initial, *h.exit)
	}
	h.subscribers[sub.id] = sub
	return sub, nil
}

func (h *hub) detach(sub *subscription) {
	h.controlMu.Lock()
	defer h.controlMu.Unlock()
	h.mu.Lock()
	defer h.mu.Unlock()
	current, exists := h.subscribers[sub.id]
	if !exists || current != sub {
		return
	}
	delete(h.subscribers, sub.id)
	if h.controller != nil && h.controller.attachmentID == sub.id {
		h.controller = nil
	}
}

func (h *hub) takeover(sub *subscription) (Frame, error) {
	h.controlMu.Lock()
	defer h.controlMu.Unlock()
	h.mu.Lock()
	defer h.mu.Unlock()
	if !sub.permissions.Control {
		return Frame{}, fmt.Errorf("forbidden_role: control permission is required")
	}
	if _, exists := h.subscribers[sub.id]; !exists {
		return Frame{}, fmt.Errorf("attachment_closed: attachment is closed")
	}
	h.expireControllerLocked(time.Now())
	if h.controller == nil || h.controller.attachmentID != sub.id {
		h.revokeControllerLocked("takeover")
	}
	h.grantControllerLocked(sub)
	earliest, latest := h.replay.bounds()
	return Frame{
		Type:             "attached",
		AttachmentID:     sub.id,
		AgentID:          h.agentID,
		Role:             "control",
		LeaseID:          sub.leaseID,
		RunnerGeneration: h.generation,
		EarliestSequence: &earliest,
		LatestSequence:   &latest,
		ProcessState:     h.state,
	}, nil
}

func (h *hub) grantControllerLocked(sub *subscription) {
	leaseID := newID()
	sub.role = "control"
	sub.leaseID = leaseID
	h.controller = &controllerLease{
		attachmentID: sub.id,
		leaseID:      leaseID,
		expiresAt:    time.Now().Add(h.controllerTTL),
	}
}

func (h *hub) revokeControllerLocked(reason string) {
	if h.controller == nil {
		return
	}
	if sub := h.subscribers[h.controller.attachmentID]; sub != nil {
		sub.role = "observe"
		sub.leaseID = ""
		h.sendLocked(sub, Frame{Type: "controller_revoked", Reason: reason})
	}
	h.controller = nil
}

func (h *hub) expireControllerLocked(now time.Time) {
	if h.controller != nil && !now.Before(h.controller.expiresAt) {
		h.revokeControllerLocked("lease_expired")
	}
}

func (h *hub) touch(sub *subscription) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.controller != nil && h.controller.attachmentID == sub.id {
		h.controller.expiresAt = time.Now().Add(h.controllerTTL)
	}
}

func (h *hub) acknowledge(sub *subscription, sequence uint64) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, exists := h.subscribers[sub.id]; !exists {
		return fmt.Errorf("attachment_closed: attachment is closed")
	}
	if sequence > h.replay.latest {
		return fmt.Errorf("replay_cursor_invalid: acknowledgement is ahead of the runner")
	}
	if h.controller != nil && h.controller.attachmentID == sub.id {
		h.controller.expiresAt = time.Now().Add(h.controllerTTL)
	}
	return nil
}

func (h *hub) writeInput(sub *subscription, leaseID string, data []byte) error {
	if len(data) > 1024*1024 {
		return fmt.Errorf("invalid_input: input exceeds 1 MiB")
	}
	h.controlMu.Lock()
	defer h.controlMu.Unlock()
	h.mu.Lock()
	if err := h.authorizeControllerLocked(sub, leaseID); err != nil {
		h.mu.Unlock()
		return err
	}
	process := h.process
	h.mu.Unlock()
	if err := process.write(data); err != nil {
		return fmt.Errorf("agent_not_running: %w", err)
	}
	h.mu.Lock()
	if h.controller != nil && h.controller.attachmentID == sub.id && h.controller.leaseID == leaseID {
		h.controller.expiresAt = time.Now().Add(h.controllerTTL)
	}
	h.mu.Unlock()
	return nil
}

func (h *hub) resize(sub *subscription, leaseID string, size TerminalSize) error {
	h.controlMu.Lock()
	defer h.controlMu.Unlock()
	h.mu.Lock()
	if err := h.authorizeControllerLocked(sub, leaseID); err != nil {
		h.mu.Unlock()
		return err
	}
	process := h.process
	h.mu.Unlock()
	if err := process.resize(size); err != nil {
		return fmt.Errorf("invalid_resize: %w", err)
	}
	h.mu.Lock()
	if h.controller != nil && h.controller.attachmentID == sub.id && h.controller.leaseID == leaseID {
		h.controller.expiresAt = time.Now().Add(h.controllerTTL)
	}
	h.mu.Unlock()
	return nil
}

func (h *hub) authorizeControllerLocked(sub *subscription, leaseID string) error {
	h.expireControllerLocked(time.Now())
	if h.controller == nil || h.controller.attachmentID != sub.id || h.controller.leaseID != leaseID {
		return fmt.Errorf("invalid_controller_lease: current controller lease is required")
	}
	return nil
}

func (h *hub) broadcastLocked(frame Frame) {
	for _, sub := range h.subscribers {
		h.sendLocked(sub, frame)
	}
}

func (h *hub) sendLocked(sub *subscription, frame Frame) {
	select {
	case sub.queue <- frame:
	default:
		delete(h.subscribers, sub.id)
		if h.controller != nil && h.controller.attachmentID == sub.id {
			h.controller = nil
		}
		sub.close(protocolError("slow_consumer", "terminal output backlog exceeded", newID(), true))
	}
}

func (h *hub) runLeaseReaper() {
	interval := h.controllerTTL / 4
	if interval > time.Second {
		interval = time.Second
	}
	if interval < 10*time.Millisecond {
		interval = 10 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case now := <-ticker.C:
			h.controlMu.Lock()
			h.mu.Lock()
			h.expireControllerLocked(now)
			h.mu.Unlock()
			h.controlMu.Unlock()
		case <-h.closed:
			return
		}
	}
}

func (h *hub) close() {
	close(h.closed)
}

func (h *hub) metadata() Metadata {
	h.mu.Lock()
	defer h.mu.Unlock()
	earliest, latest := h.replay.bounds()
	var pid int
	var startedAt time.Time
	var exit *processExit
	if h.process != nil {
		pid, startedAt, exit = h.process.snapshot()
	}
	metadata := Metadata{
		AgentID:          h.agentID,
		RunnerGeneration: h.generation,
		State:            h.state,
		PID:              pid,
		StartedAt:        startedAt,
		EarliestSequence: earliest,
		LatestSequence:   latest,
	}
	if exit != nil {
		metadata.ExitCode = exit.ExitCode
		metadata.Signal = exit.Signal
		metadata.FinishedAt = &exit.FinishedAt
	}
	return metadata
}
