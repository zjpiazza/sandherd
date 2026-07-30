package herdrbridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zjpiazza/sandherd/internal/lifecycle"
)

const bridgeTestAgentID = "019c09f2-34c1-7ee0-9c66-d52919d67380"

func testConfig(t *testing.T, baseURL string) Config {
	t.Helper()
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("sandherd-test-token-value"), 0o600); err != nil {
		t.Fatal(err)
	}
	return Config{
		BaseURL: baseURL, TokenFile: tokenFile, PollInterval: 10 * time.Millisecond, ReconnectLimit: time.Second,
		Create: lifecycle.CreateRequest{Spec: lifecycle.AgentSpec{
			Kind: "codex", SandboxProfile: "standard",
			Resources: lifecycle.ResourceSpec{CPU: "1", Memory: "1Gi"},
			Workspace: lifecycle.WorkspaceSpec{Size: "10Gi", StorageProfile: "default", RetentionPolicy: "retain"},
			Lifecycle: lifecycle.LifecycleSpec{IdleTimeoutSeconds: 0},
		}},
	}
}

func TestLoadConfigUsesPluginDirectoryAndEnvironmentOverrides(t *testing.T) {
	directory := t.TempDir()
	configuration := testConfig(t, "https://configured.example/base")
	contents, _ := json.Marshal(struct {
		BaseURL   string                  `json:"baseUrl"`
		TokenFile string                  `json:"tokenFile"`
		Create    lifecycle.CreateRequest `json:"create"`
	}{configuration.BaseURL, configuration.TokenFile, configuration.Create})
	if err := os.WriteFile(filepath.Join(directory, "config.json"), contents, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERDR_PLUGIN_CONFIG_DIR", directory)
	t.Setenv("SANDHERD_CONFIG_FILE", "")
	t.Setenv("SANDHERD_BASE_URL", "https://override.example")
	loaded, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.BaseURL != "https://override.example" || loaded.Create.Spec.Kind != "codex" || loaded.PollInterval <= 0 {
		t.Fatalf("loaded configuration = %#v", loaded)
	}
}

func TestClientLifecycleAndCredentialBoundary(t *testing.T) {
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer sandherd-test-token-value" {
			t.Errorf("authorization = %q", request.Header.Get("Authorization"))
		}
		if strings.Contains(request.Header.Get("Authorization"), "kube") || request.Header.Get("X-Sandbox-ID") != "" {
			t.Error("client sent Kubernetes routing data")
		}
		methods = append(methods, request.Method+" "+request.URL.Path)
		agent := lifecycle.Agent{APIVersion: "sandherd.dev/v1alpha1", ID: bridgeTestAgentID, Name: "alpha", Status: lifecycle.AgentStatus{State: lifecycle.StateRunning}}
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/base/v1alpha1/agents":
			_ = json.NewEncoder(response).Encode(lifecycle.AgentList{Items: []lifecycle.Agent{agent}})
		default:
			_ = json.NewEncoder(response).Encode(agent)
		}
	}))
	defer server.Close()
	configuration := testConfig(t, server.URL+"/base")
	client, err := NewClient(configuration, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	request := configuration.CreateRequest("alpha")
	if _, err := client.Create(ctx, request); err != nil {
		t.Fatal(err)
	}
	if _, err := client.List(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Get(ctx, bridgeTestAgentID); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Stop(ctx, bridgeTestAgentID); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Resume(ctx, bridgeTestAgentID); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Delete(ctx, bridgeTestAgentID); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"POST /base/v1alpha1/agents", "GET /base/v1alpha1/agents", "GET /base/v1alpha1/agents/" + bridgeTestAgentID,
		"POST /base/v1alpha1/agents/" + bridgeTestAgentID + ":stop", "POST /base/v1alpha1/agents/" + bridgeTestAgentID + ":resume", "DELETE /base/v1alpha1/agents/" + bridgeTestAgentID,
	}
	if strings.Join(methods, "|") != strings.Join(want, "|") {
		t.Fatalf("requests = %#v, want %#v", methods, want)
	}
	if terminalURL := client.TerminalURL(bridgeTestAgentID); terminalURL != "ws"+strings.TrimPrefix(server.URL, "http")+"/base/v1alpha1/agents/"+bridgeTestAgentID+"/terminal" {
		t.Fatalf("terminal URL = %s", terminalURL)
	}
}

func TestStateStoreRoundTripContainsNoCredential(t *testing.T) {
	store, err := NewStateStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sequence := uint64(42)
	state := AgentState{AgentID: bridgeTestAgentID, Name: "alpha", BaseURL: "https://sandherd.example", PaneID: "w1:p2", AfterSequence: &sequence, RunnerGeneration: "generation"}
	if err := store.Save(state); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load(bridgeTestAgentID)
	if err != nil || loaded.PaneID != "w1:p2" || loaded.AfterSequence == nil || *loaded.AfterSequence != 42 {
		t.Fatalf("loaded state = %#v, error %v", loaded, err)
	}
	files, _ := filepath.Glob(filepath.Join(store.directory, "*.json"))
	contents, _ := os.ReadFile(files[0])
	if strings.Contains(string(contents), "token") || strings.Contains(string(contents), "credential") {
		t.Fatalf("state contains credential material: %s", contents)
	}
	if err := store.Remove(bridgeTestAgentID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(bridgeTestAgentID); !os.IsNotExist(err) {
		t.Fatalf("load removed state error = %v", err)
	}
}
