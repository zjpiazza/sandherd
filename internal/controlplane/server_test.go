package controlplane

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	internalauth "github.com/zjpiazza/sandherd/internal/auth"
	cluster "github.com/zjpiazza/sandherd/internal/kubernetes"
	"github.com/zjpiazza/sandherd/internal/lifecycle"
	"github.com/zjpiazza/sandherd/internal/runtimeadapter"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

const (
	apiToken        = "control-plane-test-token-value-long-enough"
	observeAPIToken = "observe-api-token-for-tests-value-long-enough"
	otherAPIToken   = "other-control-token-for-tests-value-long-enough"
)

type testAuthenticator map[string]internalauth.Principal

type authenticatorFunc func(context.Context, string) (internalauth.Principal, error)

func (f authenticatorFunc) Authenticate(ctx context.Context, token string) (internalauth.Principal, error) {
	return f(ctx, token)
}

func (a testAuthenticator) Authenticate(_ context.Context, token string) (internalauth.Principal, error) {
	principal, ok := a[token]
	if !ok {
		return internalauth.Principal{}, internalauth.ErrUnauthenticated
	}
	return principal, nil
}

func newTestAuthenticator(t *testing.T) testAuthenticator {
	t.Helper()
	control, err := internalauth.NewPrincipalWithCredentialProfiles("owner", []internalauth.Permission{internalauth.PermissionControl}, []string{"personal"}, []string{"personal"})
	if err != nil {
		t.Fatal(err)
	}
	observer, err := internalauth.NewPrincipal("owner", []internalauth.Permission{internalauth.PermissionObserve}, nil)
	if err != nil {
		t.Fatal(err)
	}
	other, err := internalauth.NewPrincipal("other-owner", []internalauth.Permission{internalauth.PermissionControl}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return testAuthenticator{apiToken: control, observeAPIToken: observer, otherAPIToken: other}
}

func testAdapterRegistry(t *testing.T) *runtimeadapter.Registry {
	t.Helper()
	registry, err := runtimeadapter.New(runtimeadapter.Config{Version: 1, Adapters: []runtimeadapter.Definition{
		{
			ID: "codex", DisplayName: "Codex fixture", Version: "test",
			Capabilities: []runtimeadapter.Capability{runtimeadapter.CapabilityInteractive},
			Profiles: []runtimeadapter.Profile{
				{SandboxProfile: "standard", CredentialMode: runtimeadapter.CredentialNone, WarmPool: "pool", Command: []string{"/bin/bash"}, HealthCheck: []string{"/bin/bash", "--version"}},
				{SandboxProfile: "standard", CredentialProfile: "personal", CredentialMode: runtimeadapter.CredentialMutable, WarmPool: "codex-personal", Command: []string{"/bin/bash"}, HealthCheck: []string{"/bin/bash", "--version"}},
			},
		},
		{
			ID: "shell-minimal", DisplayName: "Shell fixture", Version: "test",
			Capabilities: []runtimeadapter.Capability{runtimeadapter.CapabilityInteractive, runtimeadapter.CapabilityHeadless},
			Profiles:     []runtimeadapter.Profile{{SandboxProfile: "standard", CredentialMode: runtimeadapter.CredentialNone, WarmPool: "shell-pool", Command: []string{"/bin/sh"}, HealthCheck: []string{"/bin/sh", "-c", "exit 0"}}},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

type recordingEnqueuer struct {
	mu  sync.Mutex
	ids []string
}

type recordingTerminalGateway struct {
	called     bool
	canControl bool
}

type ownershipTerminalGateway struct {
	repository *cluster.Repository
}

func (g ownershipTerminalGateway) ServeTerminal(response http.ResponseWriter, request *http.Request, owner, agentID, _ string, _ bool) error {
	if _, err := g.repository.Get(request.Context(), owner, agentID); err != nil {
		return err
	}
	response.WriteHeader(http.StatusNoContent)
	return nil
}

func (ownershipTerminalGateway) Metrics(response http.ResponseWriter, _ *http.Request) {
	response.WriteHeader(http.StatusOK)
}

func (g *recordingTerminalGateway) ServeTerminal(response http.ResponseWriter, _ *http.Request, _, _, _ string, canControl bool) error {
	g.called = true
	g.canControl = canControl
	response.WriteHeader(http.StatusNoContent)
	return nil
}

func (g *recordingTerminalGateway) Metrics(response http.ResponseWriter, _ *http.Request) {
	response.WriteHeader(http.StatusOK)
}

func (e *recordingEnqueuer) Enqueue(id string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ids = append(e.ids, id)
}

func testAPI(t *testing.T) (*cluster.Repository, *recordingEnqueuer, *lifecycle.EventBus, *httptest.Server) {
	t.Helper()
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		cluster.AgentGVR: "AgentList", cluster.SandboxClaimGVR: "SandboxClaimList", cluster.SandboxGVR: "SandboxList",
		cluster.PodGVR: "PodList", cluster.PVCGVR: "PersistentVolumeClaimList",
	})
	repository := cluster.NewRepository(client, "sandherd-system")
	enqueuer := &recordingEnqueuer{}
	events := lifecycle.NewEventBus(32)
	server := NewServer(repository, enqueuer, events, slog.New(slog.NewTextHandler(io.Discard, nil)), newTestAuthenticator(t), func() bool { return true }, nil, testAdapterRegistry(t))
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	return repository, enqueuer, events, httpServer
}

func createPayload(name string) string {
	return `{"name":"` + name + `","spec":{"kind":"codex","sandboxProfile":"standard","resources":{"cpu":"1","memory":"1Gi"},"workspace":{"size":"10Gi","storageProfile":"default","retentionPolicy":"retain"},"lifecycle":{"idleTimeoutSeconds":0}}}`
}

func apiRequest(t *testing.T, method, url, body, idempotency, ifMatch string) *http.Response {
	return apiRequestAs(t, apiToken, method, url, body, idempotency, ifMatch)
}

func apiRequestAs(t *testing.T, token, method, url, body, idempotency, ifMatch string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	if idempotency != "" {
		request.Header.Set("Idempotency-Key", idempotency)
	}
	if ifMatch != "" {
		request.Header.Set("If-Match", ifMatch)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func decodeAgent(t *testing.T, response *http.Response) lifecycle.Agent {
	t.Helper()
	defer response.Body.Close()
	var agent lifecycle.Agent
	if err := json.NewDecoder(response.Body).Decode(&agent); err != nil {
		t.Fatalf("decode agent: %v", err)
	}
	return agent
}

func TestLifecycleAPIIdempotencyETagsAndTransitions(t *testing.T) {
	repository, enqueuer, _, httpServer := testAPI(t)
	response := apiRequest(t, http.MethodPost, httpServer.URL+"/v1alpha1/agents", createPayload("alpha"), "create-1", "")
	if response.StatusCode != http.StatusCreated || response.Header.Get("ETag") == "" || response.Header.Get("Location") == "" {
		t.Fatalf("create response status=%d headers=%v", response.StatusCode, response.Header)
	}
	agent := decodeAgent(t, response)
	repeated := apiRequest(t, http.MethodPost, httpServer.URL+"/v1alpha1/agents", createPayload("alpha"), "create-1", "")
	if repeated.StatusCode != http.StatusCreated {
		t.Fatalf("repeat status = %d", repeated.StatusCode)
	}
	if duplicate := decodeAgent(t, repeated); duplicate.ID != agent.ID {
		t.Fatalf("repeat ID = %s, want %s", duplicate.ID, agent.ID)
	}
	conflict := apiRequest(t, http.MethodPost, httpServer.URL+"/v1alpha1/agents", createPayload("bravo"), "create-1", "")
	assertAPIError(t, conflict, http.StatusConflict, "idempotency_conflict")

	running := agent.Status
	running.State = lifecycle.StateRunning
	running.ObservedGeneration = agent.Generation
	now := time.Now().UTC()
	running.LastTransitionAt = &now
	if _, err := repository.SetStatus(context.Background(), agent.ID, running); err != nil {
		t.Fatal(err)
	}
	get := apiRequest(t, http.MethodGet, httpServer.URL+"/v1alpha1/agents/"+agent.ID, "", "", "")
	etag := get.Header.Get("ETag")
	_ = decodeAgent(t, get)
	stale := apiRequest(t, http.MethodPost, httpServer.URL+"/v1alpha1/agents/"+agent.ID+":stop", "", "", `"stale"`)
	assertAPIError(t, stale, http.StatusPreconditionFailed, "precondition_failed")
	stop := apiRequest(t, http.MethodPost, httpServer.URL+"/v1alpha1/agents/"+agent.ID+":stop", "", "", etag)
	if stop.StatusCode != http.StatusAccepted {
		t.Fatalf("stop status = %d", stop.StatusCode)
	}
	stopping := decodeAgent(t, stop)
	if stopping.Status.State != lifecycle.StateStopping {
		t.Fatalf("stop state = %s", stopping.Status.State)
	}
	repeatStop := apiRequest(t, http.MethodPost, httpServer.URL+"/v1alpha1/agents/"+agent.ID+":stop", "", "", "")
	if repeatStop.StatusCode != http.StatusAccepted {
		t.Fatalf("repeat stop status = %d", repeatStop.StatusCode)
	}
	_ = repeatStop.Body.Close()

	enqueuer.mu.Lock()
	defer enqueuer.mu.Unlock()
	if len(enqueuer.ids) < 2 || enqueuer.ids[0] != agent.ID {
		t.Fatalf("enqueued IDs = %#v", enqueuer.ids)
	}
}

func TestLifecycleAPIValidationFilteringAndErrors(t *testing.T) {
	_, _, _, httpServer := testAPI(t)
	for _, path := range []string{"/v1alpha1/agents", "/v1alpha1/events", "/v1alpha1/agents/019c09f2-34c1-7ee0-9c66-d52919d67380/terminal"} {
		unauthorized, err := http.Get(httpServer.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		if unauthorized.Header.Get("WWW-Authenticate") != "Bearer" {
			t.Fatalf("%s WWW-Authenticate = %q", path, unauthorized.Header.Get("WWW-Authenticate"))
		}
		assertAPIError(t, unauthorized, http.StatusUnauthorized, "unauthenticated")
	}
	missingKey := apiRequest(t, http.MethodPost, httpServer.URL+"/v1alpha1/agents", createPayload("alpha"), "", "")
	assertAPIError(t, missingKey, http.StatusBadRequest, "invalid_idempotency_key")
	invalid := apiRequest(t, http.MethodPost, httpServer.URL+"/v1alpha1/agents", strings.Replace(createPayload("alpha"), `"cpu":"1"`, `"cpu":"not-a-quantity"`, 1), "key", "")
	assertAPIError(t, invalid, http.StatusUnprocessableEntity, "validation_failed")
	created := apiRequest(t, http.MethodPost, httpServer.URL+"/v1alpha1/agents", createPayload("alpha"), "key", "")
	agent := decodeAgent(t, created)
	invalidStop := apiRequest(t, http.MethodPost, httpServer.URL+"/v1alpha1/agents/"+agent.ID+":stop", "", "", "")
	assertAPIError(t, invalidStop, http.StatusConflict, "invalid_state_transition")
	filtered := apiRequest(t, http.MethodGet, httpServer.URL+"/v1alpha1/agents?name=alpha&state=requested&limit=1", "", "", "")
	defer filtered.Body.Close()
	var list lifecycle.AgentList
	if err := json.NewDecoder(filtered.Body).Decode(&list); err != nil || len(list.Items) != 1 {
		t.Fatalf("filtered list = %#v, error %v", list, err)
	}
	badLimit := apiRequest(t, http.MethodGet, httpServer.URL+"/v1alpha1/agents?limit=999", "", "", "")
	assertAPIError(t, badLimit, http.StatusBadRequest, "invalid_limit")
}

func TestAdapterDiscoveryValidationAndChange(t *testing.T) {
	_, enqueuer, _, httpServer := testAPI(t)
	listed := apiRequest(t, http.MethodGet, httpServer.URL+"/v1alpha1/adapters", "", "", "")
	defer listed.Body.Close()
	var adapters runtimeadapter.List
	if listed.StatusCode != http.StatusOK || json.NewDecoder(listed.Body).Decode(&adapters) != nil || len(adapters.Items) != 2 {
		t.Fatalf("adapter list status=%d value=%#v", listed.StatusCode, adapters)
	}
	for _, adapter := range adapters.Items {
		encoded, _ := json.Marshal(adapter)
		if strings.Contains(string(encoded), "warmPool") || strings.Contains(string(encoded), "command") || strings.Contains(string(encoded), "credentialProfile") {
			t.Fatalf("adapter discovery leaked internal configuration: %s", encoded)
		}
	}

	created := apiRequest(t, http.MethodPost, httpServer.URL+"/v1alpha1/agents", createPayload("rebind"), "rebind-create", "")
	agent := decodeAgent(t, created)
	originalWorkspace := agent.Spec.Workspace
	changed := apiRequest(t, http.MethodPost, httpServer.URL+"/v1alpha1/agents/"+agent.ID+":change-adapter", `{"kind":"shell-minimal"}`, "", "")
	if changed.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(changed.Body)
		_ = changed.Body.Close()
		t.Fatalf("change adapter status=%d body=%s", changed.StatusCode, body)
	}
	updated := decodeAgent(t, changed)
	if updated.ID != agent.ID || updated.Spec.Kind != "shell-minimal" || updated.Spec.Workspace != originalWorkspace || updated.RuntimeGeneration != 2 || updated.Status.State != lifecycle.StateReconfiguring {
		t.Fatalf("changed agent = %#v", updated)
	}
	repeated := apiRequest(t, http.MethodPost, httpServer.URL+"/v1alpha1/agents/"+agent.ID+":change-adapter", `{"kind":"shell-minimal"}`, "", "")
	if duplicate := decodeAgent(t, repeated); duplicate.RuntimeGeneration != 2 {
		t.Fatalf("idempotent adapter generation = %d", duplicate.RuntimeGeneration)
	}
	unknown := apiRequest(t, http.MethodPost, httpServer.URL+"/v1alpha1/agents/"+agent.ID+":change-adapter", `{"kind":"missing"}`, "", "")
	assertAPIError(t, unknown, http.StatusUnprocessableEntity, "adapter_not_found")

	enqueuer.mu.Lock()
	defer enqueuer.mu.Unlock()
	if len(enqueuer.ids) < 2 || enqueuer.ids[len(enqueuer.ids)-1] != agent.ID {
		t.Fatalf("adapter change enqueue IDs = %#v", enqueuer.ids)
	}
}

func TestAgentCredentialProfileRequiresPrincipalAndInstalledBinding(t *testing.T) {
	_, _, _, httpServer := testAPI(t)
	credentialPayload := strings.Replace(createPayload("credentialed"), `"sandboxProfile":"standard"`, `"sandboxProfile":"standard","credentialProfile":"personal"`, 1)
	allowed := apiRequest(t, http.MethodPost, httpServer.URL+"/v1alpha1/agents", credentialPayload, "credential-allowed", "")
	if allowed.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(allowed.Body)
		_ = allowed.Body.Close()
		t.Fatalf("credential profile create status=%d body=%s", allowed.StatusCode, body)
	}
	_ = allowed.Body.Close()
	deniedPayload := strings.Replace(credentialPayload, `"name":"credentialed"`, `"name":"credentialed-other"`, 1)
	denied := apiRequestAs(t, otherAPIToken, http.MethodPost, httpServer.URL+"/v1alpha1/agents", deniedPayload, "credential-denied", "")
	assertAPIError(t, denied, http.StatusForbidden, "forbidden_credential_profile")
	unsupportedPayload := strings.Replace(createPayload("unsupported"), `"kind":"codex","sandboxProfile":"standard"`, `"kind":"shell-minimal","sandboxProfile":"standard","credentialProfile":"personal"`, 1)
	unsupported := apiRequest(t, http.MethodPost, httpServer.URL+"/v1alpha1/agents", unsupportedPayload, "credential-missing", "")
	assertAPIError(t, unsupported, http.StatusUnprocessableEntity, "adapter_profile_not_found")
}

func TestAuthenticationBackendUnavailableIsRetryable(t *testing.T) {
	client := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	authenticator := authenticatorFunc(func(context.Context, string) (internalauth.Principal, error) {
		return internalauth.Principal{}, internalauth.ErrUnavailable
	})
	server := NewServer(cluster.NewRepository(client, "sandherd-system"), &recordingEnqueuer{}, lifecycle.NewEventBus(1), slog.New(slog.NewTextHandler(io.Discard, nil)), authenticator, func() bool { return true }, nil, testAdapterRegistry(t))
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	response := apiRequestAs(t, apiToken, http.MethodGet, httpServer.URL+"/v1alpha1/agents", "", "", "")
	assertAPIError(t, response, http.StatusServiceUnavailable, "authentication_unavailable")
}

func TestObserveCredentialIsReadOnlyAndReachesTerminalGateway(t *testing.T) {
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		cluster.AgentGVR: "AgentList", cluster.SandboxClaimGVR: "SandboxClaimList", cluster.SandboxGVR: "SandboxList",
		cluster.PodGVR: "PodList", cluster.PVCGVR: "PersistentVolumeClaimList",
	})
	repository := cluster.NewRepository(client, "sandherd-system")
	terminal := &recordingTerminalGateway{}
	server := NewServer(repository, &recordingEnqueuer{}, lifecycle.NewEventBus(16), slog.New(slog.NewTextHandler(io.Discard, nil)), newTestAuthenticator(t), func() bool { return true }, terminal, testAdapterRegistry(t))
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()

	request, _ := http.NewRequest(http.MethodGet, httpServer.URL+"/v1alpha1/agents/019c09f2-34c1-7ee0-9c66-d52919d67380/terminal", nil)
	request.Header.Set("Authorization", "Bearer "+observeAPIToken)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNoContent || !terminal.called || terminal.canControl {
		t.Fatalf("terminal status=%d called=%v control=%v", response.StatusCode, terminal.called, terminal.canControl)
	}
	create, _ := http.NewRequest(http.MethodPost, httpServer.URL+"/v1alpha1/agents", strings.NewReader(createPayload("read-only")))
	create.Header.Set("Authorization", "Bearer "+observeAPIToken)
	create.Header.Set("Idempotency-Key", "read-only")
	response, err = http.DefaultClient.Do(create)
	if err != nil {
		t.Fatal(err)
	}
	assertAPIError(t, response, http.StatusForbidden, "forbidden_role")
}

func TestPrincipalsCannotAccessAnotherOwnersAgents(t *testing.T) {
	repository, _, events, initialServer := testAPI(t)
	created := apiRequest(t, http.MethodPost, initialServer.URL+"/v1alpha1/agents", createPayload("shared-name"), "owner-create", "")
	agent := decodeAgent(t, created)

	server := NewServer(repository, &recordingEnqueuer{}, events, slog.New(slog.NewTextHandler(io.Discard, nil)), newTestAuthenticator(t), func() bool { return true }, ownershipTerminalGateway{repository: repository}, testAdapterRegistry(t))
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()

	listResponse := apiRequestAs(t, otherAPIToken, http.MethodGet, httpServer.URL+"/v1alpha1/agents", "", "", "")
	defer listResponse.Body.Close()
	var list lifecycle.AgentList
	if err := json.NewDecoder(listResponse.Body).Decode(&list); err != nil || len(list.Items) != 0 {
		t.Fatalf("other owner's list = %#v, error %v", list, err)
	}
	for _, path := range []string{"/v1alpha1/agents/" + agent.ID, "/v1alpha1/agents/" + agent.ID + "/terminal"} {
		response := apiRequestAs(t, otherAPIToken, http.MethodGet, httpServer.URL+path, "", "", "")
		assertAPIError(t, response, http.StatusNotFound, "agent_not_found")
	}
	deleted := apiRequestAs(t, otherAPIToken, http.MethodDelete, httpServer.URL+"/v1alpha1/agents/"+agent.ID, "", "", "")
	assertAPIError(t, deleted, http.StatusNotFound, "agent_not_found")

	otherCreated := apiRequestAs(t, otherAPIToken, http.MethodPost, httpServer.URL+"/v1alpha1/agents", createPayload("shared-name"), "other-create", "")
	if otherCreated.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(otherCreated.Body)
		_ = otherCreated.Body.Close()
		t.Fatalf("other owner create status=%d body=%s", otherCreated.StatusCode, body)
	}
	if otherAgent := decodeAgent(t, otherCreated); otherAgent.Owner != "other-owner" || otherAgent.ID == agent.ID {
		t.Fatalf("other owner's agent = %#v", otherAgent)
	}
}

func TestPrincipalSecretProfileAuthorization(t *testing.T) {
	_, _, _, httpServer := testAPI(t)
	payload := strings.Replace(createPayload("profiled"), `"sandboxProfile":"standard"`, `"sandboxProfile":"standard","repository":{"url":"https://example.invalid/repository.git"},"secretProfile":"personal"`, 1)
	allowed := apiRequest(t, http.MethodPost, httpServer.URL+"/v1alpha1/agents", payload, "profile-allowed", "")
	if allowed.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(allowed.Body)
		_ = allowed.Body.Close()
		t.Fatalf("allowed profile status=%d body=%s", allowed.StatusCode, body)
	}
	_ = allowed.Body.Close()
	denied := apiRequestAs(t, otherAPIToken, http.MethodPost, httpServer.URL+"/v1alpha1/agents", strings.Replace(payload, `"name":"profiled"`, `"name":"profiled-other"`, 1), "profile-denied", "")
	assertAPIError(t, denied, http.StatusForbidden, "forbidden_secret_profile")
}

func TestAuthenticationAuditDoesNotLeakCredential(t *testing.T) {
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		cluster.AgentGVR: "AgentList", cluster.SandboxClaimGVR: "SandboxClaimList", cluster.SandboxGVR: "SandboxList",
		cluster.PodGVR: "PodList", cluster.PVCGVR: "PersistentVolumeClaimList",
	})
	var logs bytes.Buffer
	server := NewServer(cluster.NewRepository(client, "sandherd-system"), &recordingEnqueuer{}, lifecycle.NewEventBus(16), slog.New(slog.NewJSONHandler(&logs, nil)), newTestAuthenticator(t), func() bool { return true }, nil, testAdapterRegistry(t))
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	canary := "credential-canary-value-that-must-never-be-logged"
	response := apiRequestAs(t, canary, http.MethodGet, httpServer.URL+"/v1alpha1/agents", "", "", "")
	assertAPIError(t, response, http.StatusUnauthorized, "unauthenticated")
	if strings.Contains(logs.String(), canary) || !strings.Contains(logs.String(), `"audit_event":"authentication_failed"`) {
		t.Fatalf("unsafe or missing security audit log: %s", logs.String())
	}
}

func TestLifecycleEventSSEReplayAndExpiredCursor(t *testing.T) {
	_, _, events, httpServer := testAPI(t)
	event := lifecycle.Event{ID: lifecycle.NewID(), Type: "agent.created", AgentID: lifecycle.NewID(), State: lifecycle.StateRequested, OccurredAt: time.Now().UTC(), Owner: "owner"}
	events.Publish(event)
	request, _ := http.NewRequest(http.MethodGet, httpServer.URL+"/v1alpha1/events", nil)
	request.Header.Set("Authorization", "Bearer "+apiToken)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(response.Body)
	var buffer bytes.Buffer
	for range 3 {
		line, readErr := reader.ReadString('\n')
		if readErr != nil {
			t.Fatal(readErr)
		}
		buffer.WriteString(line)
	}
	_ = response.Body.Close()
	if !strings.Contains(buffer.String(), "id: "+event.ID) || !strings.Contains(buffer.String(), "event: agent.created") {
		t.Fatalf("SSE replay = %q", buffer.String())
	}
	expired := apiRequest(t, http.MethodGet, httpServer.URL+"/v1alpha1/events", "", "", "")
	// The generic helper cannot set Last-Event-ID, so close this open stream and
	// issue the cursor request explicitly.
	_ = expired.Body.Close()
	cursorRequest, _ := http.NewRequest(http.MethodGet, httpServer.URL+"/v1alpha1/events", nil)
	cursorRequest.Header.Set("Authorization", "Bearer "+apiToken)
	cursorRequest.Header.Set("Last-Event-ID", lifecycle.NewID())
	cursorResponse, err := http.DefaultClient.Do(cursorRequest)
	if err != nil {
		t.Fatal(err)
	}
	assertAPIError(t, cursorResponse, http.StatusGone, "event_cursor_expired")
}

func assertAPIError(t *testing.T, response *http.Response, status int, code string) {
	t.Helper()
	defer response.Body.Close()
	if response.StatusCode != status {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("status = %d, want %d; body=%s", response.StatusCode, status, body)
	}
	var envelope struct {
		Error struct {
			Code      string         `json:"code"`
			RequestID string         `json:"requestId"`
			Details   map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Error.Code != code || envelope.Error.RequestID == "" || envelope.Error.Details == nil {
		t.Fatalf("error envelope = %#v", envelope)
	}
}
