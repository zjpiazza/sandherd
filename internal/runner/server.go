package runner

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	internalauth "github.com/zjpiazza/sandherd/internal/auth"
)

type Metadata struct {
	AgentID          string     `json:"agentId"`
	RunnerGeneration string     `json:"runnerGeneration"`
	State            string     `json:"state"`
	PID              int        `json:"pid"`
	StartedAt        time.Time  `json:"startedAt"`
	EarliestSequence uint64     `json:"earliestSequence"`
	LatestSequence   uint64     `json:"latestSequence"`
	ExitCode         *int       `json:"exitCode,omitempty"`
	Signal           string     `json:"signal,omitempty"`
	FinishedAt       *time.Time `json:"finishedAt,omitempty"`
}

type staticAuthenticator struct {
	controlToken []byte
	observeToken []byte
}

type authenticator interface {
	authenticate(*http.Request) (Permissions, bool)
}

type capabilityAuthenticator struct {
	verifier *internalauth.Verifier
	agentID  string
}

func (a capabilityAuthenticator) authenticate(request *http.Request) (Permissions, bool) {
	if request.Method != http.MethodGet || request.URL.Path != "/v1alpha1/terminal" {
		return Permissions{}, false
	}
	claims, err := a.verifier.VerifyFor(request.Header.Get(internalauth.CapabilityHeader), a.agentID, "terminal")
	if err != nil {
		return Permissions{}, false
	}
	permissions := Permissions{Observe: true, TerminalOnly: true, RequestID: claims.RequestID}
	permissions.Control = claims.Role == "control"
	return permissions, true
}

type multipleAuthenticators []authenticator

func (a multipleAuthenticators) authenticate(request *http.Request) (Permissions, bool) {
	for _, candidate := range a {
		if permissions, ok := candidate.authenticate(request); ok {
			return permissions, true
		}
	}
	return Permissions{}, false
}

func (a staticAuthenticator) authenticate(request *http.Request) (Permissions, bool) {
	value := request.Header.Get("Authorization")
	if !strings.HasPrefix(value, "Bearer ") {
		return Permissions{}, false
	}
	token := []byte(strings.TrimPrefix(value, "Bearer "))
	if constantTimeEqual(token, a.controlToken) {
		return Permissions{Observe: true, Control: true}, true
	}
	if len(a.observeToken) > 0 && constantTimeEqual(token, a.observeToken) {
		return Permissions{Observe: true}, true
	}
	return Permissions{}, false
}

func constantTimeEqual(left, right []byte) bool {
	return len(left) == len(right) && subtle.ConstantTimeCompare(left, right) == 1
}

type server struct {
	hub           *hub
	process       *agentProcess
	authenticator authenticator
	logger        *slog.Logger
	stopGrace     time.Duration
	attachTimeout time.Duration
	writeTimeout  time.Duration
}

func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /readyz", s.handleReadiness)
	mux.HandleFunc("GET /v1alpha1/metadata", s.authenticated(s.handleMetadata))
	mux.HandleFunc("GET /v1alpha1/terminal", s.authenticated(s.handleTerminal))
	mux.HandleFunc("POST /v1alpha1/signal", s.authenticated(s.handleSignal))
	mux.HandleFunc("POST /v1alpha1/stop", s.authenticated(s.handleStop))
	return mux
}

type authenticatedHandler func(http.ResponseWriter, *http.Request, Permissions)

func (s *server) authenticated(next authenticatedHandler) http.HandlerFunc {
	return func(response http.ResponseWriter, request *http.Request) {
		permissions, ok := s.authenticator.authenticate(request)
		if !ok {
			writeHTTPError(response, http.StatusUnauthorized, "unauthenticated", "a valid bearer token is required")
			return
		}
		next(response, request, permissions)
	}
}

func (s *server) handleHealth(response http.ResponseWriter, _ *http.Request) {
	writeJSON(response, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *server) handleReadiness(response http.ResponseWriter, _ *http.Request) {
	metadata := s.hub.metadata()
	if metadata.State == "starting" {
		writeJSON(response, http.StatusServiceUnavailable, map[string]string{"status": "starting"})
		return
	}
	writeJSON(response, http.StatusOK, map[string]string{"status": "ready"})
}

func (s *server) handleMetadata(response http.ResponseWriter, _ *http.Request, permissions Permissions) {
	if permissions.TerminalOnly {
		writeHTTPError(response, http.StatusForbidden, "forbidden_scope", "the credential is scoped to terminal streaming")
		return
	}
	writeJSON(response, http.StatusOK, s.hub.metadata())
}

func (s *server) handleStop(response http.ResponseWriter, _ *http.Request, permissions Permissions) {
	if !permissions.Control || permissions.TerminalOnly {
		writeHTTPError(response, http.StatusForbidden, "forbidden_role", "control permission is required")
		return
	}
	go s.process.terminate(s.stopGrace)
	writeJSON(response, http.StatusAccepted, map[string]string{"status": "stopping"})
}

func (s *server) handleSignal(response http.ResponseWriter, request *http.Request, permissions Permissions) {
	if !permissions.Control || permissions.TerminalOnly {
		writeHTTPError(response, http.StatusForbidden, "forbidden_role", "control permission is required")
		return
	}
	request.Body = http.MaxBytesReader(response, request.Body, 4096)
	var body struct {
		Signal string `json:"signal"`
	}
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		writeHTTPError(response, http.StatusBadRequest, "invalid_signal", "request body must contain a supported signal")
		return
	}
	signal, err := parseSignal(body.Signal)
	if err != nil {
		writeHTTPError(response, http.StatusBadRequest, "invalid_signal", err.Error())
		return
	}
	if err := s.process.signal(signal); err != nil {
		writeHTTPError(response, http.StatusConflict, "agent_not_running", err.Error())
		return
	}
	writeJSON(response, http.StatusAccepted, map[string]string{"status": "signaled", "signal": signalName(signal)})
}

func (s *server) handleTerminal(response http.ResponseWriter, request *http.Request, permissions Permissions) {
	if !offersProtocol(request.Header.Get("Sec-WebSocket-Protocol"), Protocol) {
		writeHTTPError(response, http.StatusBadRequest, "unsupported_protocol", "the Sandherd terminal subprotocol is required")
		return
	}
	connection, err := websocket.Accept(response, request, &websocket.AcceptOptions{Subprotocols: []string{Protocol}})
	if err != nil {
		s.logger.Warn("WebSocket upgrade failed", "error", err)
		return
	}
	defer connection.CloseNow()
	connection.SetReadLimit(1024 * 1024)

	requestID := permissions.RequestID
	if requestID == "" {
		requestID = newID()
	}
	attachContext, cancelAttach := context.WithTimeout(request.Context(), s.attachTimeout)
	messageType, payload, err := connection.Read(attachContext)
	cancelAttach()
	if err != nil {
		return
	}
	if messageType != websocket.MessageText {
		s.writeAndClose(connection, protocolError("unsupported_protocol", "terminal frames must be JSON text messages", requestID, false))
		return
	}
	attachFrame, err := decodeFrame(payload)
	if err != nil || attachFrame.Type != "attach" || attachFrame.ProtocolVersion != ProtocolVersion || attachFrame.TerminalSize == nil {
		s.writeAndClose(connection, protocolError("unsupported_protocol", "the first frame must be a valid v1alpha1 attach", requestID, false))
		return
	}
	subscription, err := s.hub.attach(attachRequest{
		Role:          attachFrame.Role,
		AfterSequence: attachFrame.AfterSequence,
		Takeover:      attachFrame.Takeover,
		TerminalSize:  *attachFrame.TerminalSize,
		Permissions:   permissions,
	})
	if err != nil {
		code := errorCode(err)
		s.writeAndClose(connection, protocolError(code, err.Error(), requestID, code == "controller_busy"))
		return
	}
	defer s.hub.detach(subscription)
	s.logger.Info("terminal attached", "attachment_id", subscription.id, "role", subscription.role, "request_id", requestID, "traceparent", request.Header.Get("traceparent"))

	connectionContext, cancelConnection := context.WithCancel(request.Context())
	defer cancelConnection()
	writerDone := make(chan error, 1)
	go func() {
		writerDone <- s.writeFrames(connectionContext, connection, subscription)
	}()

	for {
		select {
		case <-subscription.done:
			cancelConnection()
			<-writerDone
			return
		case <-writerDone:
			return
		default:
		}

		messageType, payload, readErr := connection.Read(connectionContext)
		if readErr != nil {
			return
		}
		if messageType != websocket.MessageText {
			subscription.close(protocolError("unsupported_protocol", "terminal frames must be JSON text messages", newID(), false))
			<-writerDone
			return
		}
		frame, decodeErr := decodeFrame(payload)
		if decodeErr != nil {
			subscription.close(protocolError("unsupported_protocol", "terminal frame is invalid", newID(), false))
			<-writerDone
			return
		}
		if !s.handleClientFrame(subscription, frame) {
			<-writerDone
			return
		}
	}
}

func (s *server) handleClientFrame(sub *subscription, frame Frame) bool {
	switch frame.Type {
	case "input":
		if frame.LeaseID == "" {
			s.sendProtocolError(sub, "invalid_controller_lease", "input requires the current lease", false)
			return true
		}
		if err := s.hub.writeInput(sub, frame.LeaseID, frame.Data); err != nil {
			s.sendProtocolError(sub, errorCode(err), err.Error(), false)
		}
	case "resize":
		if frame.LeaseID == "" || frame.TerminalSize == nil {
			s.sendProtocolError(sub, "invalid_controller_lease", "resize requires the current lease and terminal size", false)
			return true
		}
		if err := s.hub.resize(sub, frame.LeaseID, *frame.TerminalSize); err != nil {
			s.sendProtocolError(sub, errorCode(err), err.Error(), false)
		}
	case "takeover":
		attached, err := s.hub.takeover(sub)
		if err != nil {
			s.sendProtocolError(sub, errorCode(err), err.Error(), false)
			return true
		}
		s.hub.send(sub, attached)
	case "ack":
		if frame.Sequence == nil {
			s.sendProtocolError(sub, "unsupported_protocol", "ack requires a sequence", false)
			return true
		}
		if err := s.hub.acknowledge(sub, *frame.Sequence); err != nil {
			s.sendProtocolError(sub, errorCode(err), err.Error(), false)
		}
	case "pong":
		s.hub.touch(sub)
	case "ping":
		if frame.Nonce == "" || len(frame.Nonce) > 128 {
			s.sendProtocolError(sub, "unsupported_protocol", "ping nonce is invalid", false)
			return true
		}
		s.hub.touch(sub)
		s.hub.send(sub, Frame{Type: "pong", Nonce: frame.Nonce})
	default:
		sub.close(protocolError("unsupported_protocol", "client frame type is not supported", newID(), false))
		return false
	}
	return true
}

func (s *server) writeFrames(ctx context.Context, connection *websocket.Conn, sub *subscription) error {
	for _, frame := range sub.initial {
		if err := s.writeFrame(ctx, connection, frame); err != nil {
			return err
		}
	}
	for {
		select {
		case frame := <-sub.queue:
			if err := s.writeFrame(ctx, connection, frame); err != nil {
				return err
			}
		case <-sub.done:
			reason := sub.closeReason()
			if reason.Type != "" {
				_ = s.writeFrame(context.Background(), connection, reason)
			}
			return connection.Close(websocket.StatusPolicyViolation, reason.Code)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (s *server) writeFrame(ctx context.Context, connection *websocket.Conn, frame Frame) error {
	writeContext, cancel := context.WithTimeout(ctx, s.writeTimeout)
	defer cancel()
	return wsjson.Write(writeContext, connection, frame)
}

func (s *server) writeAndClose(connection *websocket.Conn, frame Frame) {
	_ = s.writeFrame(context.Background(), connection, frame)
	_ = connection.Close(websocket.StatusPolicyViolation, frame.Code)
}

func (s *server) sendProtocolError(sub *subscription, code, message string, retryable bool) {
	s.hub.send(sub, protocolError(code, message, newID(), retryable))
}

func (h *hub) send(sub *subscription, frame Frame) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if current := h.subscribers[sub.id]; current == sub {
		h.sendLocked(sub, frame)
	}
}

func offersProtocol(header, protocol string) bool {
	for _, offered := range strings.Split(header, ",") {
		if strings.TrimSpace(offered) == protocol {
			return true
		}
	}
	return false
}

func errorCode(err error) string {
	message := err.Error()
	if before, _, ok := strings.Cut(message, ":"); ok && before != "" {
		return before
	}
	return "internal_error"
}

func writeJSON(response http.ResponseWriter, status int, value any) {
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(value)
}

func writeHTTPError(response http.ResponseWriter, status int, code, message string) {
	writeJSON(response, status, map[string]any{
		"error": map[string]any{
			"code":      code,
			"message":   message,
			"requestId": newID(),
			"retryable": false,
			"details":   map[string]any{},
		},
	})
}
