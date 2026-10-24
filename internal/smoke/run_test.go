package smoke

import (
	"context"
	"testing"

	cluster "github.com/zjpiazza/sandherd/internal/kubernetes"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func TestRecoveryInjectionRequiresTheStoppedOwnedTestSandbox(t *testing.T) {
	for _, test := range []struct {
		name, mode, owner, agentLabel string
		wantError                     bool
	}{
		{name: "valid claim ownership without sandbox agent label", mode: "Suspended", owner: "claim-uid", agentLabel: "test-id"},
		{name: "running sandbox", mode: "Running", owner: "claim-uid", agentLabel: "test-id", wantError: true},
		{name: "different owner", mode: "Suspended", owner: "another-claim", agentLabel: "test-id", wantError: true},
		{name: "different agent", mode: "Suspended", owner: "claim-uid", agentLabel: "other-id", wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			claim := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "extensions.agents.x-k8s.io/v1beta1", "kind": "SandboxClaim",
				"metadata": map[string]any{"name": "sandbox-testid", "namespace": "test", "uid": "claim-uid", "labels": map[string]any{cluster.AgentIDLabel: test.agentLabel}},
				"status":   map[string]any{"sandbox": map[string]any{"name": "assigned-sandbox"}},
			}}
			sandbox := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "agents.x-k8s.io/v1beta1", "kind": "Sandbox",
				"metadata": map[string]any{"name": "assigned-sandbox", "namespace": "test", "ownerReferences": []any{map[string]any{
					"apiVersion": "extensions.agents.x-k8s.io/v1beta1", "kind": "SandboxClaim", "name": "sandbox-testid", "uid": test.owner, "controller": true,
				}}},
				"spec": map[string]any{"operatingMode": test.mode, "podTemplate": map[string]any{"spec": map[string]any{
					"restartPolicy": "Always", "containers": []any{map[string]any{
						"name": "runner", "command": []any{"/usr/local/bin/sandherd-runner"}, "args": []any{"--agent-id=$(SANDHERD_AGENT_ID)"},
					}},
				}}},
			}}
			client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
				cluster.SandboxClaimGVR: "SandboxClaimList", cluster.SandboxGVR: "SandboxList",
			})
			for resource, object := range map[schema.GroupVersionResource]*unstructured.Unstructured{cluster.SandboxClaimGVR: claim, cluster.SandboxGVR: sandbox} {
				if _, err := client.Resource(resource).Namespace("test").Create(context.Background(), object, metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			err := injectCompletion(context.Background(), client, "test", "test-id")
			if (err != nil) != test.wantError {
				t.Fatalf("error = %v, wantError = %v", err, test.wantError)
			}
			if test.wantError {
				for _, action := range client.Actions() {
					if action.GetVerb() == "patch" || action.GetVerb() == "update" || action.GetVerb() == "delete" {
						t.Fatalf("unsafe fixture performed mutation: %#v", action)
					}
				}
				return
			}
			updated, err := client.Resource(cluster.SandboxGVR).Namespace("test").Get(context.Background(), "assigned-sandbox", metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			policy, _, _ := unstructured.NestedString(updated.Object, "spec", "podTemplate", "spec", "restartPolicy")
			if policy != "Never" {
				t.Fatalf("fault injection did not configure a completed Pod: %s", policy)
			}
		})
	}
}
