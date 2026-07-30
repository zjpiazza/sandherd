package gateway

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/zjpiazza/sandherd/internal/auth"
	"github.com/zjpiazza/sandherd/internal/kubernetes"
	"github.com/zjpiazza/sandherd/internal/lifecycle"
	"github.com/zjpiazza/sandherd/internal/runner"
)

const (
	testAgentID     = "019c09f2-34c1-7ee0-9c66-d52919d67380"
	testRouterToken = "router-service-account-token"
)

type fakeResolver struct {
	target kubernetes.RunnerTarget
	err    error
}

func (r fakeResolver) ResolveRunner(context.Context, string, string) (kubernetes.RunnerTarget, error) {
	return r.target, r.err
}

type backendConnection struct {
	connection *websocket.Conn
	writeMu    sync.Mutex
}

func (c *backendConnection) write(t *testing.T, payload []byte) {
	t.Helper()
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.connection.Write(ctx, websocket.MessageText, payload); err != nil {
		t.Fatalf("backend write: %v", err)
	}
}

type runnerBackend struct {
	t        *testing.T
	verifier *auth.Verifier
	received chan []byte
	headers  chan http.Header
	mu       sync.Mutex
	nextID   int
	clients  map[int]*backendConnection
	server   *httptest.Server
}

func newRunnerBackend(t *testing.T, verifier *auth.Verifier) *runnerBackend {
	t.Helper()
	b := &runnerBackend{t: t, verifier: verifier, received: make(chan []byte, 64), headers: make(chan http.Header, 16), clients: make(map[int]*backendConnection)}
	b.server = httptest.NewServer(http.HandlerFunc(b.serveHTTP))
	t.Cleanup(b.server.Close)
	return b
}

func (b *runnerBackend) serveHTTP(response http.ResponseWriter, request *http.Request) {
	if request.Header.Get("Authorization") != "" {
		http.Error(response, "router credential leaked", http.StatusBadRequest)
		return
	}
	if _, err := b.verifier.Verify(request.Header.Get(auth.CapabilityHeader), testAgentID); err != nil {
		http.Error(response, "invalid capability", http.StatusUnauthorized)
		return
	}
	b.headers <- request.Header.Clone()
	connection, err := websocket.Accept(response, request, &websocket.AcceptOptions{Subprotocols: []string{runner.Protocol}})
	if err != nil {
		return
	}
	defer connection.CloseNow()
	client := &backendConnection{connection: connection}
	b.mu.Lock()
	id := b.nextID
	b.nextID++
	b.clients[id] = client
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.clients, id)
		b.mu.Unlock()
	}()
	ctx := request.Context()
	for {
		messageType, payload, err := connection.Read(ctx)
		if err != nil {
			return
		}
		if messageType != websocket.MessageText {
			return
		}
		copied := append([]byte(nil), payload...)
		b.received <- copied
		frameType, role := inspectType(payload)
		if frameType == "attach" {
			attachedRole := role
			lease := ""
			if attachedRole == "control" {
				lease = `,"leaseId":"019c09f2-34c1-7ee0-9c66-d52919d67381"`
			}
			client.write(b.t, []byte(`{"type":"attached","attachmentId":"019c09f2-34c1-7ee0-9c66-d52919d67382","agentId":"`+testAgentID+`","role":"`+attachedRole+`"`+lease+`,"runnerGeneration":"019c09f2-34c1-7ee0-9c66-d52919d67383","earliestSequence":0,"latestSequence":0,"processState":"running"}`))
		}
		if frameType == "takeover" {
			client.write(b.t, []byte(`{"type":"attached","attachmentId":"019c09f2-34c1-7ee0-9c66-d52919d67382","agentId":"`+testAgentID+`","role":"control","leaseId":"019c09f2-34c1-7ee0-9c66-d52919d67384","runnerGeneration":"019c09f2-34c1-7ee0-9c66-d52919d67383","earliestSequence":0,"latestSequence":0,"processState":"running"}`))
		}
	}
}

func (b *runnerBackend) broadcast(payload []byte) {
	b.mu.Lock()
	clients := make([]*backendConnection, 0, len(b.clients))
	for _, client := range b.clients {
		clients = append(clients, client)
	}
	b.mu.Unlock()
	for _, client := range clients {
		client.write(b.t, payload)
	}
}

func newRouter(t *testing.T, backendURL string) *httptest.Server {
	t.Helper()
	target, err := url.Parse(backendURL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	originalDirector := proxy.Director
	proxy.Director = func(request *http.Request) {
		originalDirector(request)
		request.Header.Del("Authorization")
	}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer "+testRouterToken || request.Header.Get("X-Sandbox-ID") != "sandbox-internal" || request.Header.Get("X-Sandbox-Namespace") != "sandherd-system" || request.Header.Get("X-Sandbox-Port") != "8080" {
			http.Error(response, "invalid router contract", http.StatusForbidden)
			return
		}
		proxy.ServeHTTP(response, request)
	}))
	t.Cleanup(server.Close)
	return server
}

type gatewayHarness struct {
	gateway *Gateway
	events  *lifecycle.EventBus
	server  *httptest.Server
	logs    *bytes.Buffer
}

func newGatewayHarness(t *testing.T, backend *runnerBackend, signer *auth.Signer, limits Limits, canControl bool) *gatewayHarness {
	t.Helper()
	routerServer := newRouter(t, backend.server.URL)
	tokenFile := t.TempDir() + "/router-token"
	if err := os.WriteFile(tokenFile, []byte(testRouterToken), 0o600); err != nil {
		t.Fatal(err)
	}
	events := lifecycle.NewEventBus(64)
	logs := &bytes.Buffer{}
	gateway, err := New(Config{
		Resolver: fakeResolver{target: kubernetes.RunnerTarget{AgentID: testAgentID, SandboxName: "sandbox-internal", Namespace: "sandherd-system"}},
		Signer:   signer, Events: events, Logger: slog.New(slog.NewJSONHandler(logs, nil)),
		RouterURL: routerServer.URL, RouterTokenFile: tokenFile, RunnerPort: 8080, Limits: limits,
	})
	if err != nil {
		t.Fatal(err)
	}
	harness := &gatewayHarness{gateway: gateway, events: events, logs: logs}
	harness.server = httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if err := gateway.ServeTerminal(response, request, "owner", testAgentID, "request-test", canControl); err != nil {
			typed, ok := err.(*lifecycle.Error)
			if !ok {
				http.Error(response, "internal", http.StatusInternalServerError)
				return
			}
			response.Header().Set("Content-Type", "application/json")
			response.WriteHeader(typed.Status)
			_ = json.NewEncoder(response).Encode(map[string]any{"error": map[string]any{"code": typed.Code, "message": typed.Message}})
		}
	}))
	t.Cleanup(harness.server.Close)
	return harness
}

func testSigner(t *testing.T) (*auth.Signer, *auth.Verifier) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, _ := auth.NewSigner(privateKey, time.Minute)
	verifier, _ := auth.NewVerifier(publicKey)
	return signer, verifier
}

func dialGateway(t *testing.T, serverURL string, attach string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	connection, response, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(serverURL, "http"), &websocket.DialOptions{Subprotocols: []string{runner.Protocol}})
	if err != nil {
		if response != nil {
			t.Fatalf("gateway dial: %v (HTTP %d)", err, response.StatusCode)
		}
		t.Fatal(err)
	}
	if err := connection.Write(ctx, websocket.MessageText, []byte(attach)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.CloseNow() })
	return connection
}

func readRaw(t *testing.T, connection *websocket.Conn) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	messageType, payload, err := connection.Read(ctx)
	if err != nil || messageType != websocket.MessageText {
		t.Fatalf("read terminal frame: type=%v error=%v", messageType, err)
	}
	return payload
}

func receiveBackend(t *testing.T, backend *runnerBackend) []byte {
	t.Helper()
	select {
	case payload := <-backend.received:
		return payload
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for backend frame")
		return nil
	}
}

func TestGatewayPreservesFramesAndInternalizesRouterDetails(t *testing.T) {
	signer, verifier := testSigner(t)
	backend := newRunnerBackend(t, verifier)
	harness := newGatewayHarness(t, backend, signer, DefaultLimits(), true)
	attach := `{"type":"attach","protocolVersion":"v1alpha1","role":"control","terminalSize":{"columns":80,"rows":24}}`
	connection := dialGateway(t, harness.server.URL, attach)
	if string(receiveBackend(t, backend)) != attach {
		t.Fatal("attach frame changed in transit")
	}
	attachedPayload := readRaw(t, connection)
	if strings.Contains(string(attachedPayload), "sandbox-internal") {
		t.Fatalf("public frame leaked Kubernetes identity: %s", attachedPayload)
	}
	if frameType, role := inspectType(attachedPayload); frameType != "attached" || role != "control" {
		t.Fatalf("attached frame = %s/%s", frameType, role)
	}
	select {
	case headers := <-backend.headers:
		if headers.Get("Authorization") != "" || headers.Get(auth.CapabilityHeader) == "" || headers.Get("X-Request-ID") != "request-test" {
			t.Fatalf("runner headers = %v", headers)
		}
	case <-time.After(time.Second):
		t.Fatal("runner did not receive headers")
	}

	clientFrames := []string{
		`{"type":"input","leaseId":"019c09f2-34c1-7ee0-9c66-d52919d67381","data":"AP8="}`,
		`{"type":"resize","leaseId":"019c09f2-34c1-7ee0-9c66-d52919d67381","terminalSize":{"columns":100,"rows":30}}`,
		`{"type":"ack","sequence":1}`,
		`{"type":"ping","nonce":"p"}`,
		`{"type":"pong","nonce":"p"}`,
	}
	for _, frame := range clientFrames {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		if err := connection.Write(ctx, websocket.MessageText, []byte(frame)); err != nil {
			cancel()
			t.Fatal(err)
		}
		cancel()
		if got := string(receiveBackend(t, backend)); got != frame {
			t.Fatalf("client frame changed: got %s, want %s", got, frame)
		}
	}

	serverFrames := []string{
		`{"type":"output","sequence":1,"data":"AP8="}`,
		`{"type":"replay_gap","requestedAfterSequence":0,"earliestSequence":1,"latestSequence":2}`,
		`{"type":"controller_revoked","reason":"lease_expired"}`,
		`{"type":"exit","runnerGeneration":"019c09f2-34c1-7ee0-9c66-d52919d67383","exitCode":0,"finishedAt":"2026-07-29T12:00:00Z"}`,
		`{"type":"error","code":"slow_consumer","message":"slow","requestId":"r","retryable":true}`,
		`{"type":"ping","nonce":"s"}`,
		`{"type":"pong","nonce":"s"}`,
	}
	for _, frame := range serverFrames {
		backend.broadcast([]byte(frame))
		if got := string(readRaw(t, connection)); got != frame {
			t.Fatalf("server frame changed: got %s, want %s", got, frame)
		}
	}
	recorder := httptest.NewRecorder()
	harness.gateway.Metrics(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	metrics := recorder.Body.String()
	for _, name := range []string{"sandherd_gateway_bytes_from_client_total", "sandherd_gateway_bytes_to_client_total", "sandherd_gateway_replay_gaps_total", "sandherd_gateway_failures_total"} {
		if !strings.Contains(metrics, name) {
			t.Fatalf("metrics missing %s: %s", name, metrics)
		}
	}
	for _, canary := range []string{"AP8=", `"nonce":"p"`} {
		if strings.Contains(metrics, canary) || strings.Contains(harness.logs.String(), canary) {
			t.Fatalf("terminal content canary leaked into telemetry: %q", canary)
		}
	}
}

func TestGatewayReconnectTakeoverAndObserveAuthorization(t *testing.T) {
	signer, verifier := testSigner(t)
	backend := newRunnerBackend(t, verifier)
	harness := newGatewayHarness(t, backend, signer, DefaultLimits(), true)
	first := dialGateway(t, harness.server.URL, `{"type":"attach","protocolVersion":"v1alpha1","role":"observe","terminalSize":{"columns":80,"rows":24}}`)
	_ = receiveBackend(t, backend)
	_ = readRaw(t, first)
	backend.broadcast([]byte(`{"type":"output","sequence":1,"data":"b25l"}`))
	_ = readRaw(t, first)
	_ = first.Close(websocket.StatusNormalClosure, "reconnect")

	second := dialGateway(t, harness.server.URL, `{"type":"attach","protocolVersion":"v1alpha1","role":"observe","afterSequence":1,"terminalSize":{"columns":80,"rows":24}}`)
	reattach := string(receiveBackend(t, backend))
	if !strings.Contains(reattach, `"afterSequence":1`) {
		t.Fatalf("reconnect attach = %s", reattach)
	}
	_ = readRaw(t, second)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	if err := second.Write(ctx, websocket.MessageText, []byte(`{"type":"takeover"}`)); err != nil {
		cancel()
		t.Fatal(err)
	}
	cancel()
	_ = receiveBackend(t, backend)
	if frameType, role := inspectType(readRaw(t, second)); frameType != "attached" || role != "control" {
		t.Fatalf("takeover response = %s/%s", frameType, role)
	}
	replay, _, _, cancelEvents := harness.events.Subscribe("owner", "")
	defer cancelEvents()
	if len(replay) != 1 || replay[0].Type != "agent.controller_taken_over" || replay[0].RequestID != "request-test" {
		t.Fatalf("takeover events = %#v", replay)
	}

	observerHarness := newGatewayHarness(t, backend, signer, DefaultLimits(), false)
	observer := dialGateway(t, observerHarness.server.URL, `{"type":"attach","protocolVersion":"v1alpha1","role":"control","terminalSize":{"columns":80,"rows":24}}`)
	var frame map[string]any
	if err := json.Unmarshal(readRaw(t, observer), &frame); err != nil || frame["code"] != "forbidden_role" {
		t.Fatalf("observer control response = %#v, error %v", frame, err)
	}
}

func TestGatewayLimitsAndReplacementDoNotStopRunner(t *testing.T) {
	signer, verifier := testSigner(t)
	backend := newRunnerBackend(t, verifier)
	limits := DefaultLimits()
	limits.MaxConnections = 1
	limits.MaxConnectionsPerAgent = 1
	limits.MaxMessageBytes = 512
	harness := newGatewayHarness(t, backend, signer, limits, true)
	attach := `{"type":"attach","protocolVersion":"v1alpha1","role":"observe","terminalSize":{"columns":80,"rows":24}}`
	first := dialGateway(t, harness.server.URL, attach)
	_ = receiveBackend(t, backend)
	_ = readRaw(t, first)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	_, response, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(harness.server.URL, "http"), &websocket.DialOptions{Subprotocols: []string{runner.Protocol}})
	cancel()
	if err == nil || response == nil || response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second connection response=%v error=%v", response, err)
	}
	_ = response.Body.Close()

	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	if err := first.Write(ctx, websocket.MessageText, []byte(`{"type":"ping","nonce":"`+strings.Repeat("x", 700)+`"}`)); err != nil {
		cancel()
		t.Fatal(err)
	}
	cancel()
	ctx, cancel = context.WithTimeout(context.Background(), 2*time.Second)
	_, _, err = first.Read(ctx)
	cancel()
	if err == nil {
		t.Fatal("oversized frame did not close connection")
	}

	replacement := newGatewayHarness(t, backend, signer, DefaultLimits(), true)
	reconnected := dialGateway(t, replacement.server.URL, `{"type":"attach","protocolVersion":"v1alpha1","role":"observe","afterSequence":1,"terminalSize":{"columns":80,"rows":24}}`)
	if got := string(receiveBackend(t, backend)); !strings.Contains(got, `"afterSequence":1`) {
		t.Fatalf("replacement gateway attach = %s", got)
	}
	if frameType, _ := inspectType(readRaw(t, reconnected)); frameType != "attached" {
		t.Fatalf("replacement gateway response = %s", frameType)
	}
}

func TestGatewayMultipleObserversReceiveOrderedOutput(t *testing.T) {
	signer, verifier := testSigner(t)
	backend := newRunnerBackend(t, verifier)
	harness := newGatewayHarness(t, backend, signer, DefaultLimits(), true)
	attach := `{"type":"attach","protocolVersion":"v1alpha1","role":"observe","terminalSize":{"columns":80,"rows":24}}`
	connections := make([]*websocket.Conn, 8)
	for index := range connections {
		connections[index] = dialGateway(t, harness.server.URL, attach)
		_ = receiveBackend(t, backend)
		_ = readRaw(t, connections[index])
	}
	frames := []string{
		`{"type":"output","sequence":1,"data":"b25l"}`,
		`{"type":"output","sequence":2,"data":"dHdv"}`,
	}
	for _, frame := range frames {
		backend.broadcast([]byte(frame))
		for index, connection := range connections {
			if got := string(readRaw(t, connection)); got != frame {
				t.Fatalf("observer %d received %s, want %s", index, got, frame)
			}
		}
	}
}

func TestGatewayEnforcesIdleAndLifetimeLimits(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		idle     time.Duration
		lifetime time.Duration
	}{
		{name: "idle", idle: 40 * time.Millisecond, lifetime: time.Second},
		{name: "lifetime", idle: time.Second, lifetime: 40 * time.Millisecond},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			signer, verifier := testSigner(t)
			backend := newRunnerBackend(t, verifier)
			limits := DefaultLimits()
			limits.IdleTimeout = testCase.idle
			limits.MaxLifetime = testCase.lifetime
			harness := newGatewayHarness(t, backend, signer, limits, true)
			connection := dialGateway(t, harness.server.URL, `{"type":"attach","protocolVersion":"v1alpha1","role":"observe","terminalSize":{"columns":80,"rows":24}}`)
			_ = receiveBackend(t, backend)
			_ = readRaw(t, connection)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			_, _, err := connection.Read(ctx)
			cancel()
			if err == nil {
				t.Fatalf("connection remained open beyond %s limit", testCase.name)
			}
		})
	}
}
