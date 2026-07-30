package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/zjpiazza/sandherd/internal/auth"
	"github.com/zjpiazza/sandherd/internal/kubernetes"
	"github.com/zjpiazza/sandherd/internal/lifecycle"
	"github.com/zjpiazza/sandherd/internal/runner"
)

type Resolver interface {
	ResolveRunner(context.Context, string, string) (kubernetes.RunnerTarget, error)
}

type Limits struct {
	MaxConnections         int
	MaxConnectionsPerAgent int
	MaxMessageBytes        int64
	AttachTimeout          time.Duration
	WriteTimeout           time.Duration
	IdleTimeout            time.Duration
	MaxLifetime            time.Duration
}

func DefaultLimits() Limits {
	return Limits{
		MaxConnections: 512, MaxConnectionsPerAgent: 16, MaxMessageBytes: 1024 * 1024,
		AttachTimeout: 10 * time.Second, WriteTimeout: 5 * time.Second,
		IdleTimeout: 15 * time.Minute, MaxLifetime: 12 * time.Hour,
	}
}

type Config struct {
	Resolver        Resolver
	Signer          *auth.Signer
	Events          *lifecycle.EventBus
	Logger          *slog.Logger
	RouterURL       string
	RouterTokenFile string
	RunnerPort      int
	Limits          Limits
}

type Gateway struct {
	resolver        Resolver
	signer          *auth.Signer
	events          *lifecycle.EventBus
	logger          *slog.Logger
	routerURL       *url.URL
	routerTokenFile string
	runnerPort      int
	limits          Limits
	metrics         Metrics
	limiter         connectionLimiter
}

func New(configuration Config) (*Gateway, error) {
	if configuration.Resolver == nil || configuration.Signer == nil || configuration.Events == nil || configuration.Logger == nil {
		return nil, fmt.Errorf("resolver, signer, events, and logger are required")
	}
	routerURL, err := url.Parse(configuration.RouterURL)
	if err != nil || (routerURL.Scheme != "http" && routerURL.Scheme != "https") || routerURL.Host == "" {
		return nil, fmt.Errorf("router URL must be an absolute HTTP URL")
	}
	if configuration.RouterTokenFile == "" || configuration.RunnerPort < 1 || configuration.RunnerPort > 65535 {
		return nil, fmt.Errorf("router token file and a valid runner port are required")
	}
	limits := configuration.Limits
	if limits.MaxConnections < 1 || limits.MaxConnectionsPerAgent < 1 || limits.MaxMessageBytes < 1 || limits.AttachTimeout <= 0 || limits.WriteTimeout <= 0 || limits.IdleTimeout <= 0 || limits.MaxLifetime <= 0 {
		return nil, fmt.Errorf("all gateway limits must be positive")
	}
	return &Gateway{
		resolver: configuration.Resolver, signer: configuration.Signer, events: configuration.Events,
		logger: configuration.Logger, routerURL: routerURL, routerTokenFile: configuration.RouterTokenFile,
		runnerPort: configuration.RunnerPort, limits: limits,
		limiter: connectionLimiter{maximum: limits.MaxConnections, perAgentMaximum: limits.MaxConnectionsPerAgent, perAgent: make(map[string]int)},
	}, nil
}

func (g *Gateway) Metrics(response http.ResponseWriter, request *http.Request) {
	g.metrics.ServeHTTP(response, request)
}

// ServeTerminal returns only errors that occur before the WebSocket upgrade.
// After upgrade it emits protocol errors and owns connection closure.
func (g *Gateway) ServeTerminal(response http.ResponseWriter, request *http.Request, owner, agentID, requestID string, canControl bool) error {
	if !offersProtocol(request.Header.Get("Sec-WebSocket-Protocol"), runner.Protocol) {
		return lifecycle.NewError(http.StatusBadRequest, "unsupported_protocol", "the Sandherd terminal subprotocol is required")
	}
	target, err := g.resolver.ResolveRunner(request.Context(), owner, agentID)
	if err != nil {
		return err
	}
	if !g.limiter.acquire(agentID) {
		result := lifecycle.NewError(http.StatusTooManyRequests, "connection_limit_exceeded", "the terminal connection limit was reached")
		result.Retryable = true
		return result
	}
	defer g.limiter.release(agentID)

	client, err := websocket.Accept(response, request, &websocket.AcceptOptions{Subprotocols: []string{runner.Protocol}})
	if err != nil {
		return nil
	}
	defer client.CloseNow()
	client.SetReadLimit(g.limits.MaxMessageBytes)
	started := time.Now()
	g.metrics.active.Add(1)
	g.metrics.connections.Add(1)
	defer func() {
		g.metrics.active.Add(-1)
		g.metrics.durationNanos.Add(uint64(time.Since(started)))
		g.metrics.durationCount.Add(1)
	}()

	attachContext, cancelAttach := context.WithTimeout(request.Context(), g.limits.AttachTimeout)
	messageType, attach, err := client.Read(attachContext)
	cancelAttach()
	if err != nil {
		g.metrics.failures.Add(1)
		return nil
	}
	frame, valid := inspectAttach(messageType, attach)
	if !valid {
		g.writeProtocolError(client, "unsupported_protocol", "the first frame must be a valid v1alpha1 attach", requestID, false)
		return nil
	}
	if frame.Role == "control" && !canControl {
		g.logger.Warn("security audit", "audit_event", "authorization_denied", "outcome", "denied", "principal_id", owner, "agent_id", agentID, "request_id", requestID, "operation", "terminal_control_attach", "reason", "control_permission_required")
		g.writeProtocolError(client, "forbidden_role", "control permission is required", requestID, false)
		return nil
	}
	permissionRole := "observe"
	if canControl {
		permissionRole = "control"
	}
	capability, err := g.signer.Mint(agentID, permissionRole, requestID)
	if err != nil {
		g.writeProtocolError(client, "internal_error", "the runner connection could not be authorized", requestID, true)
		g.metrics.failures.Add(1)
		return nil
	}
	routerToken, err := readToken(g.routerTokenFile)
	if err != nil {
		g.writeProtocolError(client, "internal_error", "the runner connection could not be authorized", requestID, true)
		g.metrics.failures.Add(1)
		return nil
	}
	headers := http.Header{
		"Authorization":       []string{"Bearer " + routerToken},
		"X-Sandbox-ID":        []string{target.SandboxName},
		"X-Sandbox-Namespace": []string{target.Namespace},
		"X-Sandbox-Port":      []string{strconv.Itoa(g.runnerPort)},
		auth.CapabilityHeader: []string{capability},
		"X-Request-ID":        []string{requestID},
	}
	if traceparent := request.Header.Get("traceparent"); traceparent != "" {
		headers.Set("traceparent", traceparent)
	}
	router, dialResponse, err := websocket.Dial(request.Context(), g.terminalURL(), &websocket.DialOptions{HTTPHeader: headers, Subprotocols: []string{runner.Protocol}})
	if dialResponse != nil && dialResponse.Body != nil {
		_ = dialResponse.Body.Close()
	}
	if err != nil {
		g.logger.Error("runner WebSocket dial failed", "agent_id", agentID, "request_id", requestID, "error", err)
		g.writeProtocolError(client, "internal_error", "the runner connection is unavailable", requestID, true)
		g.metrics.failures.Add(1)
		return nil
	}
	defer router.CloseNow()
	router.SetReadLimit(g.limits.MaxMessageBytes)
	writeContext, cancelWrite := context.WithTimeout(request.Context(), g.limits.WriteTimeout)
	err = router.Write(writeContext, websocket.MessageText, attach)
	cancelWrite()
	if err != nil {
		g.writeProtocolError(client, "internal_error", "the runner connection is unavailable", requestID, true)
		g.metrics.failures.Add(1)
		return nil
	}
	g.metrics.bytesFromClient.Add(uint64(len(attach)))

	g.logger.Info("terminal gateway attached", "agent_id", agentID, "role", frame.Role, "request_id", requestID, "traceparent", request.Header.Get("traceparent"))
	g.proxy(request.Context(), client, router, owner, agentID, requestID, canControl, frame.Takeover)
	return nil
}

func (g *Gateway) proxy(parent context.Context, client, upstream *websocket.Conn, owner, agentID, requestID string, canControl, initialTakeover bool) {
	ctx, cancel := context.WithTimeout(parent, g.limits.MaxLifetime)
	defer cancel()
	var lastActivity atomic.Int64
	lastActivity.Store(time.Now().UnixNano())
	activity := func() { lastActivity.Store(time.Now().UnixNano()) }
	var takeoverPending atomic.Int64
	if initialTakeover && canControl {
		takeoverPending.Store(1)
	}
	errorsChannel := make(chan error, 3)

	go func() {
		for {
			messageType, payload, err := client.Read(ctx)
			if err != nil {
				errorsChannel <- err
				return
			}
			activity()
			if messageType != websocket.MessageText {
				errorsChannel <- errors.New("client sent a non-text terminal frame")
				return
			}
			frameType, _ := inspectType(payload)
			if frameType == "takeover" {
				if canControl {
					takeoverPending.Add(1)
				} else {
					g.logger.Warn("security audit", "audit_event", "authorization_denied", "outcome", "denied", "principal_id", owner, "agent_id", agentID, "request_id", requestID, "operation", "terminal_takeover", "reason", "control_permission_required")
				}
			}
			writeContext, cancelWrite := context.WithTimeout(ctx, g.limits.WriteTimeout)
			err = upstream.Write(writeContext, messageType, payload)
			cancelWrite()
			if err != nil {
				errorsChannel <- err
				return
			}
			g.metrics.bytesFromClient.Add(uint64(len(payload)))
		}
	}()

	go func() {
		for {
			messageType, payload, err := upstream.Read(ctx)
			if err != nil {
				errorsChannel <- err
				return
			}
			activity()
			frameType, role := inspectType(payload)
			if frameType == "replay_gap" {
				g.metrics.replayGaps.Add(1)
			}
			if frameType == "error" {
				g.metrics.failures.Add(1)
			}
			if frameType == "attached" && role == "control" {
				for pending := takeoverPending.Load(); pending > 0; pending = takeoverPending.Load() {
					if takeoverPending.CompareAndSwap(pending, pending-1) {
						g.metrics.takeovers.Add(1)
						g.events.Publish(lifecycle.Event{Type: "agent.controller_taken_over", AgentID: agentID, OccurredAt: time.Now().UTC(), RequestID: requestID, Owner: owner})
						g.logger.Info("terminal controller taken over", "agent_id", agentID, "request_id", requestID)
						break
					}
				}
			}
			writeContext, cancelWrite := context.WithTimeout(ctx, g.limits.WriteTimeout)
			err = client.Write(writeContext, messageType, payload)
			cancelWrite()
			if err != nil {
				errorsChannel <- err
				return
			}
			g.metrics.bytesToClient.Add(uint64(len(payload)))
		}
	}()

	go func() {
		interval := g.limits.IdleTimeout / 4
		if interval < 10*time.Millisecond {
			interval = 10 * time.Millisecond
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if time.Since(time.Unix(0, lastActivity.Load())) >= g.limits.IdleTimeout {
					errorsChannel <- errors.New("terminal connection idle timeout")
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	err := <-errorsChannel
	cancel()
	if websocket.CloseStatus(err) != websocket.StatusNormalClosure && !errors.Is(err, context.Canceled) {
		g.metrics.failures.Add(1)
		g.logger.Debug("terminal gateway detached", "agent_id", agentID, "request_id", requestID, "error", err)
	}
}

func (g *Gateway) terminalURL() string {
	result := *g.routerURL
	if result.Scheme == "https" {
		result.Scheme = "wss"
	} else {
		result.Scheme = "ws"
	}
	result.Path = strings.TrimSuffix(result.Path, "/") + "/v1alpha1/terminal"
	return result.String()
}

func (g *Gateway) writeProtocolError(connection *websocket.Conn, code, message, requestID string, retryable bool) {
	payload, _ := json.Marshal(map[string]any{"type": "error", "code": code, "message": message, "requestId": requestID, "retryable": retryable})
	ctx, cancel := context.WithTimeout(context.Background(), g.limits.WriteTimeout)
	defer cancel()
	_ = connection.Write(ctx, websocket.MessageText, payload)
	_ = connection.Close(websocket.StatusPolicyViolation, code)
}

type inspectedFrame struct {
	Type            string               `json:"type"`
	ProtocolVersion string               `json:"protocolVersion"`
	Role            string               `json:"role"`
	Takeover        bool                 `json:"takeover"`
	TerminalSize    *runner.TerminalSize `json:"terminalSize"`
}

func inspectAttach(messageType websocket.MessageType, payload []byte) (inspectedFrame, bool) {
	var frame inspectedFrame
	if messageType != websocket.MessageText || json.Unmarshal(payload, &frame) != nil || frame.Type != "attach" || frame.ProtocolVersion != runner.ProtocolVersion || (frame.Role != "control" && frame.Role != "observe") || frame.TerminalSize == nil || frame.TerminalSize.Validate() != nil {
		return inspectedFrame{}, false
	}
	return frame, true
}

func inspectType(payload []byte) (string, string) {
	var frame struct {
		Type string `json:"type"`
		Role string `json:"role"`
	}
	if json.Unmarshal(payload, &frame) != nil {
		return "", ""
	}
	return frame.Type, frame.Role
}

func offersProtocol(header, protocol string) bool {
	for _, offered := range strings.Split(header, ",") {
		if strings.TrimSpace(offered) == protocol {
			return true
		}
	}
	return false
}

func readToken(path string) (string, error) {
	contents, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(contents))
	if len(token) < 16 {
		return "", fmt.Errorf("router token must contain at least 16 non-whitespace bytes")
	}
	return token, nil
}

type connectionLimiter struct {
	mu              sync.Mutex
	maximum         int
	perAgentMaximum int
	total           int
	perAgent        map[string]int
}

func (l *connectionLimiter) acquire(agentID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.total >= l.maximum || l.perAgent[agentID] >= l.perAgentMaximum {
		return false
	}
	l.total++
	l.perAgent[agentID]++
	return true
}

func (l *connectionLimiter) release(agentID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.total--
	l.perAgent[agentID]--
	if l.perAgent[agentID] == 0 {
		delete(l.perAgent, agentID)
	}
}
