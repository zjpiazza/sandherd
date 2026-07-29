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

	cluster "github.com/zjpiazza/sandherd/internal/kubernetes"
	"github.com/zjpiazza/sandherd/internal/lifecycle"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

const apiToken = "control-plane-test-token"

type recordingEnqueuer struct {
	mu  sync.Mutex
	ids []string
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
	server := NewServer(repository, enqueuer, events, slog.New(slog.NewTextHandler(io.Discard, nil)), []byte(apiToken), "owner", func() bool { return true })
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	return repository, enqueuer, events, httpServer
}

func createPayload(name string) string {
	return `{"name":"` + name + `","spec":{"kind":"codex","sandboxProfile":"standard","resources":{"cpu":"1","memory":"1Gi"},"workspace":{"size":"10Gi","storageProfile":"default","retentionPolicy":"retain"},"lifecycle":{"idleTimeoutSeconds":0}}}`
}

func apiRequest(t *testing.T, method, url, body, idempotency, ifMatch string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+apiToken)
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
	unauthorized, err := http.Get(httpServer.URL + "/v1alpha1/agents")
	if err != nil {
		t.Fatal(err)
	}
	assertAPIError(t, unauthorized, http.StatusUnauthorized, "unauthenticated")
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
