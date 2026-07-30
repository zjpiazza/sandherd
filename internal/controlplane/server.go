package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	internalauth "github.com/zjpiazza/sandherd/internal/auth"
	"github.com/zjpiazza/sandherd/internal/kubernetes"
	"github.com/zjpiazza/sandherd/internal/lifecycle"
	"github.com/zjpiazza/sandherd/internal/runtimeadapter"
	"k8s.io/apimachinery/pkg/api/resource"
)

type Enqueuer interface {
	Enqueue(string)
}

type TerminalGateway interface {
	ServeTerminal(http.ResponseWriter, *http.Request, string, string, string, bool) error
	Metrics(http.ResponseWriter, *http.Request)
}

type principalContextKey struct{}

type Server struct {
	repository    *kubernetes.Repository
	controller    Enqueuer
	events        *lifecycle.EventBus
	logger        *slog.Logger
	authenticator internalauth.Authenticator
	ready         func() bool
	terminal      TerminalGateway
	adapters      *runtimeadapter.Registry
}

func NewServer(repository *kubernetes.Repository, controller Enqueuer, events *lifecycle.EventBus, logger *slog.Logger, authenticator internalauth.Authenticator, ready func() bool, terminal TerminalGateway, adapters *runtimeadapter.Registry) *Server {
	return &Server{repository: repository, controller: controller, events: events, logger: logger, authenticator: authenticator, ready: ready, terminal: terminal, adapters: adapters}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(response http.ResponseWriter, _ *http.Request) {
		writeJSON(response, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", s.handleReady)
	if s.terminal != nil {
		mux.HandleFunc("GET /metrics", s.terminal.Metrics)
	}
	mux.HandleFunc("/v1alpha1/agents", s.authenticate(s.handleAgents))
	mux.HandleFunc("/v1alpha1/agents/", s.authenticate(s.handleAgent))
	mux.HandleFunc("GET /v1alpha1/adapters", s.authenticate(s.handleAdapters))
	mux.HandleFunc("GET /v1alpha1/events", s.authenticate(s.handleEvents))
	return mux
}

type authenticatedHandler func(http.ResponseWriter, *http.Request, string, string)

func (s *Server) authenticate(next authenticatedHandler) http.HandlerFunc {
	return func(response http.ResponseWriter, request *http.Request) {
		requestID := request.Header.Get("X-Request-ID")
		if requestID == "" || len(requestID) > 128 {
			requestID = lifecycle.NewID()
		}
		response.Header().Set("X-Request-ID", requestID)
		value := request.Header.Get("Authorization")
		provided, validBearer := strings.CutPrefix(value, "Bearer ")
		if !validBearer || provided == "" || strings.ContainsAny(provided, " \t\r\n") {
			response.Header().Set("WWW-Authenticate", "Bearer")
			s.audit(request, requestID, "authentication_failed", "", "", "invalid_credential")
			writeError(response, requestID, lifecycle.NewError(http.StatusUnauthorized, "unauthenticated", "a valid bearer token is required"))
			return
		}
		principal, err := s.authenticator.Authenticate(request.Context(), provided)
		if errors.Is(err, internalauth.ErrUnavailable) {
			s.audit(request, requestID, "authentication_unavailable", "", "", "credential_backend_unavailable")
			result := lifecycle.NewError(http.StatusServiceUnavailable, "authentication_unavailable", "authentication is temporarily unavailable")
			result.Retryable = true
			writeError(response, requestID, result)
			return
		}
		if err != nil || principal.ID == "" || !principal.CanObserve() {
			response.Header().Set("WWW-Authenticate", "Bearer")
			s.audit(request, requestID, "authentication_failed", "", "", "invalid_credential")
			writeError(response, requestID, lifecycle.NewError(http.StatusUnauthorized, "unauthenticated", "a valid bearer token is required"))
			return
		}
		request = request.WithContext(context.WithValue(request.Context(), principalContextKey{}, principal))
		next(response, request, principal.ID, requestID)
	}
}

func (s *Server) handleReady(response http.ResponseWriter, _ *http.Request) {
	if s.ready != nil && !s.ready() {
		writeJSON(response, http.StatusServiceUnavailable, map[string]string{"status": "not_ready"})
		return
	}
	writeJSON(response, http.StatusOK, map[string]string{"status": "ready"})
}

func (s *Server) handleAgents(response http.ResponseWriter, request *http.Request, owner, requestID string) {
	switch request.Method {
	case http.MethodPost:
		s.createAgent(response, request, owner, requestID)
	case http.MethodGet:
		s.listAgents(response, request, owner, requestID)
	default:
		response.Header().Set("Allow", "GET, POST")
		writeError(response, requestID, lifecycle.NewError(http.StatusMethodNotAllowed, "method_not_allowed", "method is not allowed"))
	}
}

func (s *Server) createAgent(response http.ResponseWriter, request *http.Request, owner, requestID string) {
	if !hasControlPermission(request) {
		s.audit(request, requestID, "authorization_denied", owner, "", "control_permission_required")
		writeError(response, requestID, lifecycle.NewError(http.StatusForbidden, "forbidden_role", "control permission is required"))
		return
	}
	key := request.Header.Get("Idempotency-Key")
	if key == "" || len(key) > 256 {
		writeError(response, requestID, lifecycle.NewError(http.StatusBadRequest, "invalid_idempotency_key", "Idempotency-Key is required and must not exceed 256 characters"))
		return
	}
	var create lifecycle.CreateRequest
	if err := decodeJSON(response, request, &create); err != nil {
		writeError(response, requestID, lifecycle.NewError(http.StatusBadRequest, "invalid_request", err.Error()))
		return
	}
	create.ApplyDefaults()
	if err := create.Validate(); err != nil {
		writeError(response, requestID, lifecycle.NewError(http.StatusUnprocessableEntity, "validation_failed", err.Error()))
		return
	}
	for field, quantity := range map[string]string{"cpu": create.Spec.Resources.CPU, "memory": create.Spec.Resources.Memory, "workspace.size": create.Spec.Workspace.Size} {
		parsed, err := resource.ParseQuantity(quantity)
		if err != nil || parsed.Sign() <= 0 {
			writeError(response, requestID, lifecycle.NewError(http.StatusUnprocessableEntity, "validation_failed", field+" is not a valid Kubernetes quantity"))
			return
		}
	}
	if !s.authorizeProfiles(response, request, owner, requestID, "", create.Spec.SecretProfile, create.Spec.CredentialProfile) {
		return
	}
	if _, err := s.adapters.Resolve(create.Spec.Kind, create.Spec.SandboxProfile, create.Spec.CredentialProfile); err != nil {
		writeAdapterError(response, requestID, err)
		return
	}
	agent, created, err := s.repository.Create(request.Context(), owner, key, create)
	if err != nil {
		writeAnyError(response, requestID, err)
		return
	}
	setAgentHeaders(response, agent)
	response.Header().Set("Location", "/v1alpha1/agents/"+url.PathEscape(agent.ID))
	status := http.StatusCreated
	if created {
		s.events.Publish(lifecycle.Event{Type: "agent.created", AgentID: agent.ID, State: agent.Status.State, OccurredAt: time.Now().UTC(), RequestID: requestID, Owner: owner})
		s.logger.Info("agent created", "agent_id", agent.ID, "owner", owner, "request_id", requestID)
	}
	s.controller.Enqueue(agent.ID)
	writeJSON(response, status, agent)
}

func (s *Server) listAgents(response http.ResponseWriter, request *http.Request, owner, requestID string) {
	limit := 50
	if raw := request.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 200 {
			writeError(response, requestID, lifecycle.NewError(http.StatusBadRequest, "invalid_limit", "limit must be between 1 and 200"))
			return
		}
		limit = parsed
	}
	state := lifecycle.State(request.URL.Query().Get("state"))
	if state != "" && !validState(state) {
		writeError(response, requestID, lifecycle.NewError(http.StatusBadRequest, "invalid_state", "state filter is invalid"))
		return
	}
	name := request.URL.Query().Get("name")
	if name != "" && !lifecycle.ValidName(name) {
		writeError(response, requestID, lifecycle.NewError(http.StatusBadRequest, "invalid_name", "name filter is invalid"))
		return
	}
	result, err := s.repository.List(request.Context(), owner, kubernetes.ListOptions{
		Limit: limit, Cursor: request.URL.Query().Get("cursor"), State: state, Name: name,
	})
	if err != nil {
		writeAnyError(response, requestID, err)
		return
	}
	writeJSON(response, http.StatusOK, result)
}

func (s *Server) handleAgent(response http.ResponseWriter, request *http.Request, owner, requestID string) {
	tail := strings.TrimPrefix(request.URL.Path, "/v1alpha1/agents/")
	operation := ""
	terminal := false
	if strings.HasSuffix(tail, "/terminal") {
		tail, terminal = strings.TrimSuffix(tail, "/terminal"), true
	} else if strings.HasSuffix(tail, ":stop") {
		tail, operation = strings.TrimSuffix(tail, ":stop"), "stop"
	} else if strings.HasSuffix(tail, ":resume") {
		tail, operation = strings.TrimSuffix(tail, ":resume"), "resume"
	} else if strings.HasSuffix(tail, ":change-adapter") {
		tail, operation = strings.TrimSuffix(tail, ":change-adapter"), "change-adapter"
	}
	if strings.Contains(tail, "/") || !looksLikeUUID(tail) {
		writeError(response, requestID, lifecycle.NewError(http.StatusNotFound, "agent_not_found", "agent was not found"))
		return
	}
	if terminal {
		if request.Method != http.MethodGet {
			writeError(response, requestID, lifecycle.NewError(http.StatusMethodNotAllowed, "method_not_allowed", "method is not allowed"))
			return
		}
		if s.terminal == nil {
			writeError(response, requestID, lifecycle.NewError(http.StatusServiceUnavailable, "gateway_unavailable", "terminal streaming is unavailable"))
			return
		}
		if err := s.terminal.ServeTerminal(response, request, owner, tail, requestID, hasControlPermission(request)); err != nil {
			if errorCode(err) == "agent_not_found" {
				s.audit(request, requestID, "agent_access_denied", owner, tail, "not_owned_or_missing")
			}
			writeAnyError(response, requestID, err)
		}
		return
	}
	if operation != "" {
		if request.Method != http.MethodPost {
			writeError(response, requestID, lifecycle.NewError(http.StatusMethodNotAllowed, "method_not_allowed", "method is not allowed"))
			return
		}
		if operation == "change-adapter" {
			s.changeAdapter(response, request, owner, requestID, tail)
		} else {
			s.mutateAgent(response, request, owner, requestID, tail, operation)
		}
		return
	}
	switch request.Method {
	case http.MethodGet:
		agent, err := s.repository.Get(request.Context(), owner, tail)
		if err != nil {
			if errorCode(err) == "agent_not_found" {
				s.audit(request, requestID, "agent_access_denied", owner, tail, "not_owned_or_missing")
			}
			writeAnyError(response, requestID, err)
			return
		}
		setAgentHeaders(response, agent)
		writeJSON(response, http.StatusOK, agent)
	case http.MethodDelete:
		s.mutateAgent(response, request, owner, requestID, tail, "delete")
	default:
		writeError(response, requestID, lifecycle.NewError(http.StatusMethodNotAllowed, "method_not_allowed", "method is not allowed"))
	}
}

func (s *Server) handleAdapters(response http.ResponseWriter, _ *http.Request, _, _ string) {
	writeJSON(response, http.StatusOK, s.adapters.List())
}

func (s *Server) changeAdapter(response http.ResponseWriter, request *http.Request, owner, requestID, id string) {
	if !hasControlPermission(request) {
		s.audit(request, requestID, "authorization_denied", owner, id, "control_permission_required")
		writeError(response, requestID, lifecycle.NewError(http.StatusForbidden, "forbidden_role", "control permission is required"))
		return
	}
	var change lifecycle.ChangeAdapterRequest
	if err := decodeJSON(response, request, &change); err != nil {
		writeError(response, requestID, lifecycle.NewError(http.StatusBadRequest, "invalid_request", err.Error()))
		return
	}
	if err := change.Validate(); err != nil {
		writeError(response, requestID, lifecycle.NewError(http.StatusUnprocessableEntity, "validation_failed", err.Error()))
		return
	}
	agent, err := s.repository.Get(request.Context(), owner, id)
	if err != nil {
		if errorCode(err) == "agent_not_found" {
			s.audit(request, requestID, "agent_access_denied", owner, id, "not_owned_or_missing")
		}
		writeAnyError(response, requestID, err)
		return
	}
	if ifMatch := request.Header.Get("If-Match"); ifMatch != "" && ifMatch != kubernetes.ETag(agent.ResourceVersion) {
		writeError(response, requestID, lifecycle.NewError(http.StatusPreconditionFailed, "precondition_failed", "the agent changed since it was read"))
		return
	}
	if !s.authorizeProfiles(response, request, owner, requestID, id, agent.Spec.SecretProfile, change.CredentialProfile) {
		return
	}
	if _, err := s.adapters.Resolve(change.Kind, agent.Spec.SandboxProfile, change.CredentialProfile); err != nil {
		writeAdapterError(response, requestID, err)
		return
	}
	if agent.Spec.Kind != change.Kind || agent.Spec.CredentialProfile != change.CredentialProfile {
		if !lifecycle.CanChangeAdapter(agent.Status.State) {
			writeError(response, requestID, lifecycle.NewError(http.StatusConflict, "invalid_state_transition", "the agent adapter cannot be changed from its current state"))
			return
		}
	}
	transitional := lifecycle.StateReconfiguring
	if agent.Status.State == lifecycle.StateStopped {
		transitional = lifecycle.StateStopped
	}
	previousKind := agent.Spec.Kind
	agent, changed, err := s.repository.ChangeAdapter(request.Context(), owner, id, request.Header.Get("If-Match"), change, transitional)
	if err != nil {
		writeAnyError(response, requestID, err)
		return
	}
	if changed {
		s.events.Publish(lifecycle.Event{Type: "agent.adapter_change_requested", AgentID: id, State: transitional, OccurredAt: time.Now().UTC(), RequestID: requestID, Owner: owner})
		s.logger.Info("agent adapter change requested", "agent_id", id, "previous_adapter", previousKind, "adapter", change.Kind, "runtime_generation", agent.RuntimeGeneration, "request_id", requestID)
		s.controller.Enqueue(id)
	}
	setAgentHeaders(response, agent)
	writeJSON(response, http.StatusAccepted, agent)
}

func (s *Server) authorizeProfiles(response http.ResponseWriter, request *http.Request, owner, requestID, agentID, secretProfile, credentialProfile string) bool {
	principal := principalFromRequest(request)
	if secretProfile != "" && !principal.AllowsSecretProfile(secretProfile) {
		s.audit(request, requestID, "authorization_denied", owner, agentID, "secret_profile_not_allowed")
		writeError(response, requestID, lifecycle.NewError(http.StatusForbidden, "forbidden_secret_profile", "the requested repository secret profile is not permitted"))
		return false
	}
	if credentialProfile != "" && !principal.AllowsCredentialProfile(credentialProfile) {
		s.audit(request, requestID, "authorization_denied", owner, agentID, "credential_profile_not_allowed")
		writeError(response, requestID, lifecycle.NewError(http.StatusForbidden, "forbidden_credential_profile", "the requested agent credential profile is not permitted"))
		return false
	}
	return true
}

func (s *Server) mutateAgent(response http.ResponseWriter, request *http.Request, owner, requestID, id, operation string) {
	if !hasControlPermission(request) {
		s.audit(request, requestID, "authorization_denied", owner, id, "control_permission_required")
		writeError(response, requestID, lifecycle.NewError(http.StatusForbidden, "forbidden_role", "control permission is required"))
		return
	}
	agent, err := s.repository.Get(request.Context(), owner, id)
	if err != nil {
		if errorCode(err) == "agent_not_found" {
			s.audit(request, requestID, "agent_access_denied", owner, id, "not_owned_or_missing")
		}
		writeAnyError(response, requestID, err)
		return
	}
	if ifMatch := request.Header.Get("If-Match"); ifMatch != "" && ifMatch != kubernetes.ETag(agent.ResourceVersion) {
		writeError(response, requestID, lifecycle.NewError(http.StatusPreconditionFailed, "precondition_failed", "the agent changed since it was read"))
		return
	}
	var desired lifecycle.DesiredState
	var transitional lifecycle.State
	switch operation {
	case "stop":
		if !lifecycle.CanStop(agent.Status.State) {
			writeError(response, requestID, lifecycle.NewError(http.StatusConflict, "invalid_state_transition", "the agent cannot be stopped from its current state"))
			return
		}
		if agent.Status.State == lifecycle.StateStopping || agent.Status.State == lifecycle.StateStopped {
			setAgentHeaders(response, agent)
			writeJSON(response, http.StatusAccepted, agent)
			return
		}
		desired, transitional = lifecycle.DesiredStopped, lifecycle.StateStopping
	case "resume":
		if !lifecycle.CanResume(agent.Status.State) {
			writeError(response, requestID, lifecycle.NewError(http.StatusConflict, "invalid_state_transition", "the agent cannot be resumed from its current state"))
			return
		}
		if agent.Status.State != lifecycle.StateStopped {
			setAgentHeaders(response, agent)
			writeJSON(response, http.StatusAccepted, agent)
			return
		}
		desired, transitional = lifecycle.DesiredRunning, lifecycle.StateProvisioning
	case "delete":
		if agent.Status.State == lifecycle.StateDeleting {
			setAgentHeaders(response, agent)
			writeJSON(response, http.StatusAccepted, agent)
			return
		}
		desired, transitional = lifecycle.DesiredDeleted, lifecycle.StateDeleting
	}
	previous := agent.Status.State
	agent, err = s.repository.SetDesired(request.Context(), owner, id, request.Header.Get("If-Match"), desired, transitional)
	if err != nil {
		writeAnyError(response, requestID, err)
		return
	}
	s.events.Publish(lifecycle.Event{Type: "agent.state_changed", AgentID: id, PreviousState: previous, State: transitional, OccurredAt: time.Now().UTC(), RequestID: requestID, Owner: owner})
	s.logger.Info("agent lifecycle requested", "agent_id", id, "operation", operation, "state", transitional, "request_id", requestID)
	s.controller.Enqueue(id)
	setAgentHeaders(response, agent)
	writeJSON(response, http.StatusAccepted, agent)
}

func hasControlPermission(request *http.Request) bool {
	return principalFromRequest(request).CanControl()
}

func principalFromRequest(request *http.Request) internalauth.Principal {
	value, _ := request.Context().Value(principalContextKey{}).(internalauth.Principal)
	return value
}

func (s *Server) handleEvents(response http.ResponseWriter, request *http.Request, owner, requestID string) {
	filterAgent := request.URL.Query().Get("agentId")
	if filterAgent != "" {
		if !looksLikeUUID(filterAgent) {
			writeError(response, requestID, lifecycle.NewError(http.StatusBadRequest, "invalid_agent_id", "agentId filter is invalid"))
			return
		}
		if _, err := s.repository.Get(request.Context(), owner, filterAgent); err != nil {
			writeAnyError(response, requestID, err)
			return
		}
	}
	replay, events, valid, cancel := s.events.Subscribe(owner, request.Header.Get("Last-Event-ID"))
	if !valid {
		writeError(response, requestID, lifecycle.NewError(http.StatusGone, "event_cursor_expired", "the event cursor is no longer available"))
		return
	}
	defer cancel()
	flusher, ok := response.(http.Flusher)
	if !ok {
		writeError(response, requestID, lifecycle.NewError(http.StatusInternalServerError, "internal_error", "streaming is unavailable"))
		return
	}
	response.Header().Set("Content-Type", "text/event-stream")
	response.Header().Set("Cache-Control", "no-cache")
	response.Header().Set("Connection", "keep-alive")
	response.WriteHeader(http.StatusOK)
	for _, event := range replay {
		if filterAgent == "" || event.AgentID == filterAgent {
			writeSSE(response, event)
		}
	}
	flusher.Flush()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case event, open := <-events:
			if !open {
				return
			}
			if event.Owner == owner && (filterAgent == "" || event.AgentID == filterAgent) {
				writeSSE(response, event)
				flusher.Flush()
			}
		case <-heartbeat.C:
			_, _ = io.WriteString(response, ": keepalive\n\n")
			flusher.Flush()
		case <-request.Context().Done():
			return
		}
	}
}

func writeSSE(response io.Writer, event lifecycle.Event) {
	payload, _ := json.Marshal(event)
	_, _ = fmt.Fprintf(response, "id: %s\nevent: %s\ndata: %s\n\n", event.ID, event.Type, payload)
}

func decodeJSON(response http.ResponseWriter, request *http.Request, target any) error {
	request.Body = http.MaxBytesReader(response, request.Body, 1024*1024)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("request body is invalid: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return fmt.Errorf("request body must contain one JSON value")
	}
	return nil
}

func setAgentHeaders(response http.ResponseWriter, agent lifecycle.Agent) {
	response.Header().Set("ETag", kubernetes.ETag(agent.ResourceVersion))
}

func writeAnyError(response http.ResponseWriter, requestID string, err error) {
	var typed *lifecycle.Error
	if errors.As(err, &typed) {
		writeError(response, requestID, typed)
		return
	}
	writeError(response, requestID, lifecycle.NewError(http.StatusInternalServerError, "internal_error", "an internal error occurred"))
}

func errorCode(err error) string {
	var typed *lifecycle.Error
	if errors.As(err, &typed) {
		return typed.Code
	}
	return ""
}

func (s *Server) audit(request *http.Request, requestID, event, principalID, agentID, reason string) {
	attributes := []any{
		"audit_event", event,
		"outcome", "denied",
		"request_id", requestID,
		"method", request.Method,
		"path", request.URL.Path,
		"reason", reason,
	}
	if principalID != "" {
		attributes = append(attributes, "principal_id", principalID)
	}
	if agentID != "" {
		attributes = append(attributes, "agent_id", agentID)
	}
	s.logger.Warn("security audit", attributes...)
}

func writeError(response http.ResponseWriter, requestID string, err *lifecycle.Error) {
	writeJSON(response, err.Status, map[string]any{"error": map[string]any{
		"code": err.Code, "message": err.Message, "requestId": requestID, "retryable": err.Retryable, "details": err.Details,
	}})
}

func writeJSON(response http.ResponseWriter, status int, value any) {
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(value)
}

func looksLikeUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for index, character := range value {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			if character != '-' {
				return false
			}
			continue
		}
		if !strings.ContainsRune("0123456789abcdefABCDEF", character) {
			return false
		}
	}
	return true
}

func validState(state lifecycle.State) bool {
	switch state {
	case lifecycle.StateRequested, lifecycle.StateProvisioning, lifecycle.StateStarting, lifecycle.StateRunning,
		lifecycle.StateReconfiguring, lifecycle.StateStopping, lifecycle.StateStopped, lifecycle.StateFailed, lifecycle.StateDeleting:
		return true
	default:
		return false
	}
}

func writeAdapterError(response http.ResponseWriter, requestID string, err error) {
	switch {
	case errors.Is(err, runtimeadapter.ErrAdapterNotFound):
		writeError(response, requestID, lifecycle.NewError(http.StatusUnprocessableEntity, "adapter_not_found", "the requested agent adapter is not installed"))
	case errors.Is(err, runtimeadapter.ErrProfileNotFound):
		writeError(response, requestID, lifecycle.NewError(http.StatusUnprocessableEntity, "adapter_profile_not_found", "the requested adapter, sandbox, and credential profile combination is not installed"))
	default:
		writeAnyError(response, requestID, err)
	}
}
