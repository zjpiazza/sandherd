package kubernetes

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/zjpiazza/sandherd/internal/lifecycle"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

const testNamespace = "sandherd-system"

func fakeClient(objects ...runtime.Object) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		AgentGVR: "AgentList", SandboxClaimGVR: "SandboxClaimList", SandboxGVR: "SandboxList", PodGVR: "PodList", PVCGVR: "PersistentVolumeClaimList",
	}, objects...)
}

func TestRepositoryResolveRunnerKeepsKubernetesIdentityInternal(t *testing.T) {
	ctx := context.Background()
	repository := NewRepository(fakeClient(), testNamespace)
	agent, _, err := repository.Create(ctx, "owner", "key", validCreateRequest("alpha"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.ResolveRunner(ctx, "owner", agent.ID); errorCode(err) != "agent_not_running" {
		t.Fatalf("resolve non-running agent error = %v", err)
	}
	status := agent.Status
	status.State = lifecycle.StateRunning
	status.ObservedGeneration = agent.Generation
	now := time.Now().UTC()
	status.ReadyAt = &now
	if _, err := repository.SetStatus(ctx, agent.ID, status); err != nil {
		t.Fatal(err)
	}
	claim := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "extensions.agents.x-k8s.io/v1beta1", "kind": "SandboxClaim",
		"metadata": map[string]any{"name": claimName(agent.ID), "namespace": testNamespace},
		"status":   map[string]any{"sandbox": map[string]any{"name": "sandbox-internal-1"}},
	}}
	if _, err := repository.client.Resource(SandboxClaimGVR).Namespace(testNamespace).Create(ctx, claim, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	target, err := repository.ResolveRunner(ctx, "owner", agent.ID)
	if err != nil || target.SandboxName != "sandbox-internal-1" || target.AgentID != agent.ID {
		t.Fatalf("target = %#v, error %v", target, err)
	}
	if _, err := repository.ResolveRunner(ctx, "another-owner", agent.ID); errorStatus(err) != http.StatusNotFound {
		t.Fatalf("cross-owner resolve error = %v", err)
	}
}

func errorStatus(err error) int {
	var typed *lifecycle.Error
	if errors.As(err, &typed) {
		return typed.Status
	}
	return 0
}

func validCreateRequest(name string) lifecycle.CreateRequest {
	return lifecycle.CreateRequest{Name: name, Spec: lifecycle.AgentSpec{
		Kind: "codex", SandboxProfile: "standard",
		Resources: lifecycle.ResourceSpec{CPU: "1", Memory: "1Gi"},
		Workspace: lifecycle.WorkspaceSpec{Size: "10Gi", StorageProfile: "default", RetentionPolicy: "retain"},
		Lifecycle: lifecycle.LifecycleSpec{IdleTimeoutSeconds: 0},
	}}
}

func TestRepositoryCreateIdempotencyAndNameUniqueness(t *testing.T) {
	ctx := context.Background()
	repository := NewRepository(fakeClient(), testNamespace)
	first, created, err := repository.Create(ctx, "owner-a", "request-1", validCreateRequest("agent-one"))
	if err != nil || !created {
		t.Fatalf("first create = created %v, error %v", created, err)
	}
	repeated, created, err := repository.Create(ctx, "owner-a", "request-1", validCreateRequest("agent-one"))
	if err != nil || created || repeated.ID != first.ID {
		t.Fatalf("repeated create = %#v, created %v, error %v", repeated, created, err)
	}
	changed := validCreateRequest("agent-two")
	if _, _, err := repository.Create(ctx, "owner-a", "request-1", changed); errorCode(err) != "idempotency_conflict" {
		t.Fatalf("changed idempotency error = %v", err)
	}
	if _, _, err := repository.Create(ctx, "owner-a", "request-2", validCreateRequest("agent-one")); errorCode(err) != "name_conflict" {
		t.Fatalf("duplicate name error = %v", err)
	}
	otherOwner, created, err := repository.Create(ctx, "owner-b", "request-1", validCreateRequest("agent-one"))
	if err != nil || !created || otherOwner.ID == first.ID {
		t.Fatalf("other owner create = %#v, created %v, error %v", otherOwner, created, err)
	}
}

func TestRepositoryListFiltersAndPaginates(t *testing.T) {
	ctx := context.Background()
	repository := NewRepository(fakeClient(), testNamespace)
	for index, name := range []string{"alpha", "bravo", "charlie"} {
		if _, _, err := repository.Create(ctx, "owner", "key-"+name, validCreateRequest(name)); err != nil {
			t.Fatalf("create %d: %v", index, err)
		}
	}
	first, err := repository.List(ctx, "owner", ListOptions{Limit: 2})
	if err != nil || len(first.Items) != 2 || first.NextCursor == "" {
		t.Fatalf("first page = %#v, error %v", first, err)
	}
	second, err := repository.List(ctx, "owner", ListOptions{Limit: 2, Cursor: first.NextCursor})
	if err != nil || len(second.Items) != 1 || second.Items[0].ID <= first.Items[1].ID {
		t.Fatalf("second page = %#v, error %v", second, err)
	}
	filtered, err := repository.List(ctx, "owner", ListOptions{Name: "bravo"})
	if err != nil || len(filtered.Items) != 1 || filtered.Items[0].Name != "bravo" {
		t.Fatalf("filtered list = %#v, error %v", filtered, err)
	}
}

func TestRepositoryETagPrecondition(t *testing.T) {
	ctx := context.Background()
	repository := NewRepository(fakeClient(), testNamespace)
	agent, _, err := repository.Create(ctx, "owner", "key", validCreateRequest("alpha"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.SetDesired(ctx, "owner", agent.ID, `"stale"`, lifecycle.DesiredStopped, lifecycle.StateStopping); errorCode(err) != "precondition_failed" {
		t.Fatalf("stale mutation error = %v", err)
	}
}

func errorCode(err error) string {
	var typed *lifecycle.Error
	if errors.As(err, &typed) {
		return typed.Code
	}
	return ""
}
