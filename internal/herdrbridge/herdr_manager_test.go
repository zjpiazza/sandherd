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
	"testing"

	"github.com/zjpiazza/sandherd/internal/lifecycle"
)

type recordedCommand struct {
	arguments []string
	env       map[string]string
}

type recordingCommandRunner struct {
	mu        sync.Mutex
	commands  []recordedCommand
	failFocus bool
}

func (r *recordingCommandRunner) Run(_ context.Context, arguments []string, environment map[string]string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.commands = append(r.commands, recordedCommand{arguments: append([]string(nil), arguments...), env: environment})
	if r.failFocus && len(arguments) >= 4 && strings.Join(arguments[:4], " ") == "plugin pane focus w1:p2" {
		return nil, context.DeadlineExceeded
	}
	return []byte(`{"result":{"type":"ok"}}`), nil
}

func (r *recordingCommandRunner) joined() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var values []string
	for _, command := range r.commands {
		values = append(values, strings.Join(command.arguments, " "))
	}
	return strings.Join(values, "\n")
}

func TestHerdrClientUsesPluginPaneAndLifecycleAPIs(t *testing.T) {
	runner := &recordingCommandRunner{}
	client := NewHerdrClient(runner)
	ctx := context.Background()
	state := AgentState{AgentID: bridgeTestAgentID, Name: "alpha"}
	if err := client.ReportAgent(ctx, "w1:p2", "working", "starting"); err != nil {
		t.Fatal(err)
	}
	if err := client.ReportMetadata(ctx, "w1:p2", state); err != nil {
		t.Fatal(err)
	}
	if err := client.OpenManager(ctx, "create", "w1:p1"); err != nil {
		t.Fatal(err)
	}
	if err := client.OpenAgent(ctx, bridgeTestAgentID, "w1:p1", true); err != nil {
		t.Fatal(err)
	}
	if err := client.Notify(ctx, "attention", "body", "request"); err != nil {
		t.Fatal(err)
	}
	commands := runner.joined()
	for _, expected := range []string{
		"pane report-agent w1:p2 --source plugin:dev.sandherd --agent sandherd --state working",
		"--token sandherd_agent_id=" + bridgeTestAgentID,
		"plugin pane open --plugin dev.sandherd --entrypoint manager --placement popup",
		"--env SANDHERD_AGENT_ID=" + bridgeTestAgentID,
		"--env SANDHERD_TAKEOVER=1",
		"notification show attention --body body --sound request",
	} {
		if !strings.Contains(commands, expected) {
			t.Fatalf("commands missing %q:\n%s", expected, commands)
		}
	}
}

func TestOpenAgentUsesTabOnNarrowTerminal(t *testing.T) {
	t.Setenv("COLUMNS", "80")
	t.Setenv("HERDR_WORKSPACE_ID", "w1")
	runner := &recordingCommandRunner{}
	client := NewHerdrClient(runner)
	if err := client.OpenAgent(context.Background(), bridgeTestAgentID, "w1:p1", false); err != nil {
		t.Fatal(err)
	}
	command := runner.joined()
	for _, expected := range []string{"--placement tab", "--workspace w1"} {
		if !strings.Contains(command, expected) {
			t.Fatalf("command missing %q: %s", expected, command)
		}
	}
	if strings.Contains(command, "--target-pane") || strings.Contains(command, "--direction") {
		t.Fatalf("tab command contains split-only arguments: %s", command)
	}
}

func TestManagerCreatesAndOpensAgentWithoutKubernetesDetails(t *testing.T) {
	var created lifecycle.CreateRequest
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/v1alpha1/agents" {
			t.Errorf("request = %s %s", request.Method, request.URL.Path)
		}
		_ = json.NewDecoder(request.Body).Decode(&created)
		_ = json.NewEncoder(response).Encode(lifecycle.Agent{ID: bridgeTestAgentID, Name: created.Name, Status: lifecycle.AgentStatus{State: lifecycle.StateRequested}})
	}))
	defer server.Close()
	configuration := testConfig(t, server.URL)
	api, _ := NewClient(configuration, server.Client())
	store, _ := NewStateStore(t.TempDir())
	commandRunner := &recordingCommandRunner{}
	herdr := NewHerdrClient(commandRunner)
	t.Setenv("SANDHERD_TARGET_PANE_ID", "w1:p1")
	var output bytes.Buffer
	manager := NewManager(configuration, api, store, herdr, strings.NewReader("alpha\n"), &output)
	if err := manager.Run(context.Background(), "create"); err != nil {
		t.Fatal(err)
	}
	if created.Name != "alpha" || created.Spec.Kind != "codex" {
		t.Fatalf("created request = %#v", created)
	}
	if _, err := store.Load(bridgeTestAgentID); err != nil {
		t.Fatal(err)
	}
	commands := commandRunner.joined()
	if !strings.Contains(commands, "plugin pane open --plugin dev.sandherd --entrypoint agent") || !strings.Contains(commands, "--target-pane w1:p1") {
		t.Fatalf("open command = %s", commands)
	}
	if strings.Contains(commands, "sandbox") || strings.Contains(commands, "kube") {
		t.Fatalf("Herdr command leaked infrastructure details: %s", commands)
	}
}

func TestManagerLifecycleActionsUseSandherdAPI(t *testing.T) {
	tests := []struct {
		name          string
		action        string
		state         lifecycle.State
		input         string
		wantRequest   string
		wantCommand   string
		wantStateGone bool
	}{
		{name: "attach resumes a stopped agent", action: "attach", state: lifecycle.StateStopped, input: "1\n", wantRequest: "POST /v1alpha1/agents/" + bridgeTestAgentID + ":resume", wantCommand: "--entrypoint agent"},
		{name: "stop", action: "stop", state: lifecycle.StateRunning, input: "1\n", wantRequest: "POST /v1alpha1/agents/" + bridgeTestAgentID + ":stop"},
		{name: "resume", action: "resume", state: lifecycle.StateStopped, input: "1\n", wantRequest: "POST /v1alpha1/agents/" + bridgeTestAgentID + ":resume"},
		{name: "delete", action: "delete", state: lifecycle.StateStopped, input: "1\nalpha\n", wantRequest: "DELETE /v1alpha1/agents/" + bridgeTestAgentID, wantCommand: "plugin pane close w1:p2", wantStateGone: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var requests []string
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				requests = append(requests, request.Method+" "+request.URL.Path)
				agent := lifecycle.Agent{ID: bridgeTestAgentID, Name: "alpha", Status: lifecycle.AgentStatus{State: test.state}}
				if request.Method == http.MethodGet {
					_ = json.NewEncoder(response).Encode(lifecycle.AgentList{Items: []lifecycle.Agent{agent}})
					return
				}
				if test.action == "stop" {
					agent.Status.State = lifecycle.StateStopped
				} else if test.action == "resume" || test.action == "attach" {
					agent.Status.State = lifecycle.StateStarting
				} else {
					agent.Status.State = lifecycle.StateDeleting
				}
				_ = json.NewEncoder(response).Encode(agent)
			}))
			defer server.Close()

			configuration := testConfig(t, server.URL)
			api, err := NewClient(configuration, server.Client())
			if err != nil {
				t.Fatal(err)
			}
			store, err := NewStateStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Save(AgentState{AgentID: bridgeTestAgentID, Name: "alpha", BaseURL: server.URL, PaneID: "w1:p2"}); err != nil {
				t.Fatal(err)
			}
			runner := &recordingCommandRunner{failFocus: test.action == "attach"}
			manager := NewManager(configuration, api, store, NewHerdrClient(runner), strings.NewReader(test.input), &bytes.Buffer{})
			if err := manager.Run(context.Background(), test.action); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(strings.Join(requests, "\n"), test.wantRequest) {
				t.Fatalf("requests missing %q: %#v", test.wantRequest, requests)
			}
			if test.wantCommand != "" && !strings.Contains(runner.joined(), test.wantCommand) {
				t.Fatalf("commands missing %q: %s", test.wantCommand, runner.joined())
			}
			_, loadErr := store.Load(bridgeTestAgentID)
			if test.wantStateGone && !os.IsNotExist(loadErr) {
				t.Fatalf("deleted agent state still exists: %v", loadErr)
			}
		})
	}
}
