package kubernetes

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/zjpiazza/sandherd/internal/lifecycle"
	"github.com/zjpiazza/sandherd/internal/runtimeadapter"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	k8stesting "k8s.io/client-go/testing"
)

func testAdapterRegistry(t *testing.T, pool string) *runtimeadapter.Registry {
	t.Helper()
	registry, err := runtimeadapter.New(runtimeadapter.Config{Version: 1, Adapters: []runtimeadapter.Definition{
		{
			ID: "codex", DisplayName: "Codex test adapter", Version: "test",
			Capabilities: []runtimeadapter.Capability{runtimeadapter.CapabilityInteractive},
			Profiles:     []runtimeadapter.Profile{{SandboxProfile: "standard", CredentialMode: runtimeadapter.CredentialNone, WarmPool: pool, Command: []string{"/bin/bash"}, HealthCheck: []string{"/bin/bash", "--version"}}},
		},
		{
			ID: "shell-minimal", DisplayName: "Shell test adapter", Version: "test",
			Capabilities: []runtimeadapter.Capability{runtimeadapter.CapabilityInteractive},
			Profiles:     []runtimeadapter.Profile{{SandboxProfile: "standard", CredentialMode: runtimeadapter.CredentialNone, WarmPool: pool + "-replacement", Command: []string{"/bin/sh"}, HealthCheck: []string{"/bin/sh", "-c", "exit 0"}}},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func TestReconcilerLifecycleAndRestartRecovery(t *testing.T) {
	ctx := context.Background()
	client := fakeClient()
	repository := NewRepository(client, testNamespace)
	events := lifecycle.NewEventBus(32)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	reconciler := NewReconciler(client, repository, testNamespace, testAdapterRegistry(t, "approved-pool"), events, logger)
	agent, _, err := repository.Create(ctx, "owner", "key", validCreateRequest("alpha"))
	if err != nil {
		t.Fatal(err)
	}

	if err := reconciler.Reconcile(ctx, agent.ID); err != nil {
		t.Fatalf("initial reconcile: %v", err)
	}
	claim, err := client.Resource(SandboxClaimGVR).Namespace(testNamespace).Get(ctx, claimName(agent.ID), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get claim: %v", err)
	}
	pool, _, _ := unstructured.NestedString(claim.Object, "spec", "warmPoolRef", "name")
	if pool != "approved-pool" || claim.GetLabels()[AgentIDLabel] != agent.ID {
		t.Fatalf("claim = %#v", claim.Object)
	}
	agent, _ = repository.Get(ctx, "owner", agent.ID)
	if agent.Status.State != lifecycle.StateProvisioning {
		t.Fatalf("state = %s, want provisioning", agent.Status.State)
	}

	markClaimReady(t, ctx, client, claim, "sandbox-one")
	createReadySandbox(t, ctx, client, agent.ID, "sandbox-one")
	if err := reconciler.Reconcile(ctx, agent.ID); err != nil {
		t.Fatalf("ready reconcile: %v", err)
	}
	agent, _ = repository.Get(ctx, "owner", agent.ID)
	if agent.Status.State != lifecycle.StateRunning || agent.Status.ReadyAt == nil {
		t.Fatalf("ready agent = %#v", agent.Status)
	}

	// A new reconciler represents a control-plane restart. It observes and adopts
	// the existing deterministic claim rather than creating another sandbox.
	restarted := NewReconciler(client, repository, testNamespace, testAdapterRegistry(t, "approved-pool"), events, logger)
	if err := restarted.Reconcile(ctx, agent.ID); err != nil {
		t.Fatalf("restart reconcile: %v", err)
	}
	claims, _ := client.Resource(SandboxClaimGVR).Namespace(testNamespace).List(ctx, metav1.ListOptions{})
	if len(claims.Items) != 1 {
		t.Fatalf("claims after restart = %d, want 1", len(claims.Items))
	}

	agent, err = repository.SetDesired(ctx, "owner", agent.ID, "", lifecycle.DesiredStopped, lifecycle.StateStopping)
	if err != nil {
		t.Fatal(err)
	}
	if err := reconciler.Reconcile(ctx, agent.ID); err != nil {
		t.Fatalf("stop reconcile: %v", err)
	}
	sandbox, _ := client.Resource(SandboxGVR).Namespace(testNamespace).Get(ctx, "sandbox-one", metav1.GetOptions{})
	operatingMode, _, _ := unstructured.NestedString(sandbox.Object, "spec", "operatingMode")
	if operatingMode != "Suspended" {
		t.Fatalf("desired sandbox operating mode = %q, want Suspended", operatingMode)
	}
	_ = unstructured.SetNestedSlice(sandbox.Object, []any{map[string]any{"type": "Suspended", "status": "True"}}, "status", "conditions")
	_, _ = client.Resource(SandboxGVR).Namespace(testNamespace).UpdateStatus(ctx, sandbox, metav1.UpdateOptions{})
	if err := reconciler.Reconcile(ctx, agent.ID); err != nil {
		t.Fatalf("stopped reconcile: %v", err)
	}
	agent, _ = repository.Get(ctx, "owner", agent.ID)
	if agent.Status.State != lifecycle.StateStopped || agent.Status.StoppedAt == nil {
		t.Fatalf("stopped agent = %#v", agent.Status)
	}

	agent, err = repository.SetDesired(ctx, "owner", agent.ID, "", lifecycle.DesiredRunning, lifecycle.StateProvisioning)
	if err != nil {
		t.Fatal(err)
	}
	if err := reconciler.Reconcile(ctx, agent.ID); err != nil {
		t.Fatalf("resume reconcile: %v", err)
	}
	sandbox, _ = client.Resource(SandboxGVR).Namespace(testNamespace).Get(ctx, "sandbox-one", metav1.GetOptions{})
	operatingMode, _, _ = unstructured.NestedString(sandbox.Object, "spec", "operatingMode")
	if operatingMode != "Running" {
		t.Fatalf("resumed sandbox operating mode = %q, want Running", operatingMode)
	}
	pod, _ := client.Resource(PodGVR).Namespace(testNamespace).Get(ctx, "sandbox-one", metav1.GetOptions{})
	_ = unstructured.SetNestedField(pod.Object, "Failed", "status", "phase")
	_, _ = client.Resource(PodGVR).Namespace(testNamespace).UpdateStatus(ctx, pod, metav1.UpdateOptions{})
	if err := reconciler.Reconcile(ctx, agent.ID); err != nil {
		t.Fatalf("failed pod reconcile: %v", err)
	}
	agent, _ = repository.Get(ctx, "owner", agent.ID)
	if agent.Status.State != lifecycle.StateFailed || agent.Status.Reason != "runner_failed" {
		t.Fatalf("failed runner agent = %#v", agent.Status)
	}
}

func TestReconcilerInjectsRepositoryAndApprovedStorageWithoutCredentials(t *testing.T) {
	ctx := context.Background()
	client := fakeClient()
	repository := NewRepository(client, testNamespace)
	reconciler := NewReconciler(client, repository, testNamespace, testAdapterRegistry(t, "public-pool"), nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	reconciler.ConfigureWorkspaceProfiles(map[string]string{"fast": "rook-ceph-block"}, map[string]string{"private": "private-pool"})
	request := validCreateRequest("alpha")
	request.Spec.Repository = &lifecycle.RepositorySpec{URL: "ssh://git@github.com/example/repo.git", Revision: "main"}
	request.Spec.Workspace.StorageProfile = "fast"
	request.Spec.SecretProfile = "private"
	agent, _, err := repository.Create(ctx, "owner", "key", request)
	if err != nil {
		t.Fatal(err)
	}
	if err := reconciler.Reconcile(ctx, agent.ID); err != nil {
		t.Fatal(err)
	}
	claim, err := client.Resource(SandboxClaimGVR).Namespace(testNamespace).Get(ctx, claimName(agent.ID), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pool, _, _ := unstructured.NestedString(claim.Object, "spec", "warmPoolRef", "name")
	// NestedString cannot index slices; inspect the single volume template explicitly.
	volumes, _, _ := unstructured.NestedSlice(claim.Object, "spec", "volumeClaimTemplates")
	volume := volumes[0].(map[string]any)
	storageClass, _, _ := unstructured.NestedString(volume, "spec", "storageClassName")
	if pool != "private-pool" || storageClass != "rook-ceph-block" {
		t.Fatalf("claim pool=%q storageClass=%q: %#v", pool, storageClass, claim.Object)
	}
	environment, _, _ := unstructured.NestedSlice(claim.Object, "spec", "env")
	encoded := fmt.Sprint(environment)
	for _, expected := range []string{"SANDHERD_AGENT_ID", "SANDHERD_REPOSITORY_URL", "SANDHERD_REPOSITORY_REVISION", "workspace-bootstrap"} {
		if !strings.Contains(encoded, expected) {
			t.Fatalf("claim environment missing %q: %s", expected, encoded)
		}
	}
	if strings.Contains(strings.ToLower(encoded), "secret") || strings.Contains(strings.ToLower(encoded), "credential") {
		t.Fatalf("claim environment leaked a credential profile: %s", encoded)
	}
}

func TestReconcilerSurfacesStableBootstrapFailure(t *testing.T) {
	ctx := context.Background()
	client := fakeClient()
	repository := NewRepository(client, testNamespace)
	reconciler := NewReconciler(client, repository, testNamespace, testAdapterRegistry(t, "pool"), nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	agent, _, _ := repository.Create(ctx, "owner", "key", validCreateRequest("alpha"))
	if err := reconciler.Reconcile(ctx, agent.ID); err != nil {
		t.Fatal(err)
	}
	claim, _ := client.Resource(SandboxClaimGVR).Namespace(testNamespace).Get(ctx, claimName(agent.ID), metav1.GetOptions{})
	markClaimReady(t, ctx, client, claim, "sandbox-one")
	createReadySandbox(t, ctx, client, agent.ID, "sandbox-one")
	pod, _ := client.Resource(PodGVR).Namespace(testNamespace).Get(ctx, "sandbox-one", metav1.GetOptions{})
	_ = unstructured.SetNestedSlice(pod.Object, []any{map[string]any{
		"name": "workspace-bootstrap", "state": map[string]any{"terminated": map[string]any{"exitCode": int64(24)}},
	}}, "status", "initContainerStatuses")
	_, _ = client.Resource(PodGVR).Namespace(testNamespace).UpdateStatus(ctx, pod, metav1.UpdateOptions{})
	if err := reconciler.Reconcile(ctx, agent.ID); err != nil {
		t.Fatal(err)
	}
	agent, _ = repository.Get(ctx, "owner", agent.ID)
	if agent.Status.State != lifecycle.StateFailed || agent.Status.Reason != "workspace_full" {
		t.Fatalf("bootstrap failure status = %#v", agent.Status)
	}
}

func TestReconcilerRejectsUnapprovedWorkspaceProfilesBeforeCreatingClaim(t *testing.T) {
	tests := []struct {
		name       string
		configure  func(*lifecycle.CreateRequest)
		wantReason string
	}{
		{name: "storage", wantReason: "storage_profile_not_found", configure: func(request *lifecycle.CreateRequest) {
			request.Spec.Workspace.StorageProfile = "unapproved"
		}},
		{name: "secret", wantReason: "secret_profile_not_found", configure: func(request *lifecycle.CreateRequest) {
			request.Spec.SecretProfile = "unapproved"
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			client := fakeClient()
			repository := NewRepository(client, testNamespace)
			reconciler := NewReconciler(client, repository, testNamespace, testAdapterRegistry(t, "pool"), nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
			request := validCreateRequest("alpha")
			test.configure(&request)
			agent, _, err := repository.Create(ctx, "owner", "key", request)
			if err != nil {
				t.Fatal(err)
			}
			if err := reconciler.Reconcile(ctx, agent.ID); err != nil {
				t.Fatal(err)
			}
			agent, _ = repository.Get(ctx, "owner", agent.ID)
			if agent.Status.State != lifecycle.StateFailed || agent.Status.Reason != test.wantReason {
				t.Fatalf("profile failure status = %#v", agent.Status)
			}
			claims, _ := client.Resource(SandboxClaimGVR).Namespace(testNamespace).List(ctx, metav1.ListOptions{})
			if len(claims.Items) != 0 {
				t.Fatalf("unapproved profile created claims: %#v", claims.Items)
			}
		})
	}
}

func TestReconcilerRetainsWorkspaceAndFinalizesDeletion(t *testing.T) {
	ctx := context.Background()
	client := fakeClient()
	repository := NewRepository(client, testNamespace)
	reconciler := NewReconciler(client, repository, testNamespace, testAdapterRegistry(t, "pool"), lifecycle.NewEventBus(8), slog.New(slog.NewTextHandler(io.Discard, nil)))
	agent, _, err := repository.Create(ctx, "owner", "key", validCreateRequest("alpha"))
	if err != nil {
		t.Fatal(err)
	}
	if err := reconciler.Reconcile(ctx, agent.ID); err != nil {
		t.Fatal(err)
	}
	pvc := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "PersistentVolumeClaim",
		"metadata": map[string]any{
			"name": "workspace-one", "namespace": testNamespace,
			"labels":          map[string]any{AgentIDLabel: agent.ID},
			"ownerReferences": []any{map[string]any{"apiVersion": "v1", "kind": "Pod", "name": "owner", "uid": "uid"}},
		},
	}}
	if _, err := client.Resource(PVCGVR).Namespace(testNamespace).Create(ctx, pvc, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.SetDesired(ctx, "owner", agent.ID, "", lifecycle.DesiredDeleted, lifecycle.StateDeleting); err != nil {
		t.Fatal(err)
	}
	if err := reconciler.Reconcile(ctx, agent.ID); err != nil {
		t.Fatalf("delete claim reconcile: %v", err)
	}
	if err := reconciler.Reconcile(ctx, agent.ID); err != nil {
		t.Fatalf("finalize agent reconcile: %v", err)
	}
	if _, err := repository.Get(ctx, "owner", agent.ID); errorCode(err) != "agent_not_found" {
		t.Fatalf("deleted agent lookup = %v", err)
	}
	preserved, err := client.Resource(PVCGVR).Namespace(testNamespace).Get(ctx, "workspace-one", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("retained PVC: %v", err)
	}
	if len(preserved.GetOwnerReferences()) != 0 {
		t.Fatalf("retained PVC owner references = %#v", preserved.GetOwnerReferences())
	}
}

func TestReconcilerRecreatesMissingClaimWithoutDuplicates(t *testing.T) {
	ctx := context.Background()
	client := fakeClient()
	repository := NewRepository(client, testNamespace)
	reconciler := NewReconciler(client, repository, testNamespace, testAdapterRegistry(t, "pool"), nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	agent, _, _ := repository.Create(ctx, "owner", "key", validCreateRequest("alpha"))
	_ = reconciler.Reconcile(ctx, agent.ID)
	_ = client.Resource(SandboxClaimGVR).Namespace(testNamespace).Delete(ctx, claimName(agent.ID), metav1.DeleteOptions{})
	if err := reconciler.Reconcile(ctx, agent.ID); err != nil {
		t.Fatal(err)
	}
	claims, _ := client.Resource(SandboxClaimGVR).Namespace(testNamespace).List(ctx, metav1.ListOptions{})
	if len(claims.Items) != 1 || claims.Items[0].GetName() != claimName(agent.ID) {
		t.Fatalf("recreated claims = %#v", claims.Items)
	}
}

func TestReconcilerChangesAdapterWithoutReplacingWorkspace(t *testing.T) {
	ctx := context.Background()
	client := fakeClient()
	repository := NewRepository(client, testNamespace)
	reconciler := NewReconciler(client, repository, testNamespace, testAdapterRegistry(t, "pool"), nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	agent, _, err := repository.Create(ctx, "owner", "key", validCreateRequest("alpha"))
	if err != nil {
		t.Fatal(err)
	}
	if err := reconciler.Reconcile(ctx, agent.ID); err != nil {
		t.Fatal(err)
	}
	sandboxName := claimName(agent.ID)
	claim, _ := client.Resource(SandboxClaimGVR).Namespace(testNamespace).Get(ctx, sandboxName, metav1.GetOptions{})
	markClaimReady(t, ctx, client, claim, sandboxName)
	createReadySandbox(t, ctx, client, agent.ID, sandboxName)
	pvc := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "PersistentVolumeClaim",
		"metadata": map[string]any{
			"name": "workspace-" + sandboxName, "namespace": testNamespace,
			"labels":          map[string]any{AgentIDLabel: agent.ID, StorageScopeLabel: StorageScopeWorkspace},
			"annotations":     map[string]any{"test.sandherd.dev/workspace-canary": "preserved"},
			"ownerReferences": []any{map[string]any{"apiVersion": "agents.x-k8s.io/v1beta1", "kind": "Sandbox", "name": sandboxName, "uid": "old-runtime"}},
		},
	}}
	if _, err := client.Resource(PVCGVR).Namespace(testNamespace).Create(ctx, pvc, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := reconciler.Reconcile(ctx, agent.ID); err != nil {
		t.Fatal(err)
	}
	agent, _ = repository.Get(ctx, "owner", agent.ID)
	originalWorkspace := agent.Spec.Workspace
	changed, didChange, err := repository.ChangeAdapter(ctx, "owner", agent.ID, "", lifecycle.ChangeAdapterRequest{Kind: "shell-minimal"}, lifecycle.StateReconfiguring)
	if err != nil || !didChange || changed.RuntimeGeneration != 2 {
		t.Fatalf("change adapter agent=%#v changed=%v error=%v", changed, didChange, err)
	}

	// First drain the old runtime.
	if err := reconciler.Reconcile(ctx, agent.ID); err != nil {
		t.Fatal(err)
	}
	sandbox, _ := client.Resource(SandboxGVR).Namespace(testNamespace).Get(ctx, sandboxName, metav1.GetOptions{})
	operatingMode, _, _ := unstructured.NestedString(sandbox.Object, "spec", "operatingMode")
	if operatingMode != "Suspended" {
		t.Fatalf("old runtime operating mode = %q", operatingMode)
	}
	_ = unstructured.SetNestedSlice(sandbox.Object, []any{map[string]any{"type": "Suspended", "status": "True"}}, "status", "conditions")
	if _, err := client.Resource(SandboxGVR).Namespace(testNamespace).UpdateStatus(ctx, sandbox, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	// Then orphan-delete the old claim and deterministic Sandbox. Kubernetes
	// leaves the PVC available for the replacement Sandbox to adopt.
	if err := reconciler.Reconcile(ctx, agent.ID); err != nil {
		t.Fatal(err)
	}
	if err := reconciler.Reconcile(ctx, agent.ID); err != nil {
		t.Fatal(err)
	}
	if err := reconciler.Reconcile(ctx, agent.ID); err != nil {
		t.Fatal(err)
	}
	replacement, err := client.Resource(SandboxClaimGVR).Namespace(testNamespace).Get(ctx, sandboxName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("replacement claim: %v", err)
	}
	pool, _, _ := unstructured.NestedString(replacement.Object, "spec", "warmPoolRef", "name")
	if pool != "pool-replacement" || replacement.GetAnnotations()[RuntimeGenerationAnnotation] != "2" || replacement.GetAnnotations()[AdapterIDAnnotation] != "shell-minimal" {
		t.Fatalf("replacement runtime claim = %#v", replacement.Object)
	}
	preserved, err := client.Resource(PVCGVR).Namespace(testNamespace).Get(ctx, "workspace-"+sandboxName, metav1.GetOptions{})
	if err != nil || preserved.GetAnnotations()["test.sandherd.dev/workspace-canary"] != "preserved" {
		t.Fatalf("workspace was not preserved: pvc=%#v error=%v", preserved, err)
	}
	assertOrphanDelete(t, client.Actions(), SandboxClaimGVR.Resource)
	assertOrphanDelete(t, client.Actions(), SandboxGVR.Resource)

	markClaimReady(t, ctx, client, replacement, sandboxName)
	createReadySandbox(t, ctx, client, agent.ID, sandboxName)
	if err := reconciler.Reconcile(ctx, agent.ID); err != nil {
		t.Fatal(err)
	}
	agent, _ = repository.Get(ctx, "owner", agent.ID)
	if agent.Status.State != lifecycle.StateRunning || agent.Spec.Workspace != originalWorkspace || agent.Status.Runtime == nil || agent.Status.Runtime.Generation != 2 || agent.Status.Runtime.Kind != "shell-minimal" {
		t.Fatalf("rebound agent = %#v", agent)
	}
}

func assertOrphanDelete(t *testing.T, actions []k8stesting.Action, resource string) {
	t.Helper()
	for _, action := range actions {
		if action.GetVerb() != "delete" || action.GetResource().Resource != resource {
			continue
		}
		deletion, ok := action.(k8stesting.DeleteAction)
		if ok && deletion.GetDeleteOptions().PropagationPolicy != nil && *deletion.GetDeleteOptions().PropagationPolicy == metav1.DeletePropagationOrphan {
			return
		}
	}
	t.Fatalf("no orphan deletion recorded for %s", resource)
}

func TestReconcilerMarksProvisioningTimeout(t *testing.T) {
	ctx := context.Background()
	client := fakeClient()
	repository := NewRepository(client, testNamespace)
	reconciler := NewReconciler(client, repository, testNamespace, testAdapterRegistry(t, "pool"), nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	reconciler.provisionTimeout = time.Millisecond
	agent, _, _ := repository.Create(ctx, "owner", "key", validCreateRequest("alpha"))
	time.Sleep(2 * time.Millisecond)
	if err := reconciler.Reconcile(ctx, agent.ID); err != nil {
		t.Fatal(err)
	}
	agent, _ = repository.Get(ctx, "owner", agent.ID)
	if agent.Status.State != lifecycle.StateFailed || agent.Status.Reason != "provisioning_timeout" {
		t.Fatalf("timed out agent = %#v", agent.Status)
	}
}

func markClaimReady(t *testing.T, ctx context.Context, client dynamic.Interface, claim *unstructured.Unstructured, sandboxName string) {
	t.Helper()
	_ = unstructured.SetNestedField(claim.Object, sandboxName, "status", "sandbox", "name")
	_ = unstructured.SetNestedSlice(claim.Object, []any{map[string]any{"type": "Ready", "status": "True", "reason": "Ready"}}, "status", "conditions")
	if _, err := client.Resource(SandboxClaimGVR).Namespace(testNamespace).UpdateStatus(ctx, claim, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func createReadySandbox(t *testing.T, ctx context.Context, client dynamic.Interface, agentID, name string) {
	t.Helper()
	sandbox := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "agents.x-k8s.io/v1beta1", "kind": "Sandbox",
		"metadata": map[string]any{"name": name, "namespace": testNamespace, "labels": map[string]any{ManagedLabel: "true", AgentIDLabel: agentID}},
		"spec":     map[string]any{"operatingMode": "Running"},
		"status":   map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True"}}},
	}}
	if _, err := client.Resource(SandboxGVR).Namespace(testNamespace).Create(ctx, sandbox, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatal(err)
	}
	pod := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{"name": name, "namespace": testNamespace, "labels": map[string]any{ManagedLabel: "true", AgentIDLabel: agentID}},
		"status":   map[string]any{"phase": "Running", "conditions": []any{map[string]any{"type": "Ready", "status": "True"}}},
	}}
	if _, err := client.Resource(PodGVR).Namespace(testNamespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatal(err)
	}
}
