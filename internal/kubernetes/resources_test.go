package kubernetes

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"testing"

	"github.com/zjpiazza/sandherd/internal/lifecycle"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

func seedSourcePool(t *testing.T, client dynamic.Interface, name string) {
	t.Helper()
	ctx := context.Background()
	template := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "extensions.agents.x-k8s.io/v1beta1", "kind": "SandboxTemplate",
		"metadata": map[string]any{"name": name, "namespace": testNamespace},
		"spec": map[string]any{
			"networkPolicyManagement": "Managed", "networkPolicy": map[string]any{"ingress": []any{}, "egress": []any{}},
			"podTemplate": map[string]any{
				"metadata": map[string]any{"labels": map[string]any{"sandherd.dev/adapter-id": "codex"}},
				"spec": map[string]any{
					"automountServiceAccountToken": false, "serviceAccountName": "sandbox", "runtimeClassName": "isolated",
					"securityContext": map[string]any{"runAsNonRoot": true, "runAsUser": int64(65532)},
					"initContainers":  []any{map[string]any{"name": "workspace-bootstrap", "image": "approved-bootstrap@sha256:fixture"}},
					"volumes":         []any{map[string]any{"name": "credential", "secret": map[string]any{"secretName": "approved-credential"}}},
					"containers": []any{
						map[string]any{
							"name": "runner", "image": "approved-runner@sha256:fixture", "command": []any{"/runner"},
							"env":             []any{map[string]any{"name": "APP_TOKEN", "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": "approved-credential", "key": "token"}}}},
							"securityContext": map[string]any{"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true},
							"resources": map[string]any{
								"requests": map[string]any{"cpu": "100m", "memory": "128Mi", "ephemeral-storage": "128Mi"},
								"limits":   map[string]any{"cpu": "4", "memory": "8Gi", "ephemeral-storage": "1Gi"},
							},
						},
						map[string]any{"name": "credential-sync", "resources": map[string]any{"limits": map[string]any{"cpu": "100m", "memory": "64Mi"}}},
					},
				},
			},
		},
	}}
	pool := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "extensions.agents.x-k8s.io/v1beta1", "kind": "SandboxWarmPool",
		"metadata": map[string]any{"name": name, "namespace": testNamespace},
		"spec":     map[string]any{"replicas": int64(0), "sandboxTemplateRef": map[string]any{"name": name}},
	}}
	for gvr, object := range map[schema.GroupVersionResource]*unstructured.Unstructured{SandboxTemplateGVR: template, SandboxWarmPoolGVR: pool} {
		if _, err := client.Resource(gvr).Namespace(testNamespace).Create(ctx, object, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			t.Fatal(err)
		}
	}
}

func sourcePoolForTest(t *testing.T, ctx context.Context, client dynamic.Interface, name string) string {
	t.Helper()
	pool, err := client.Resource(SandboxWarmPoolGVR).Namespace(testNamespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return pool.GetAnnotations()[SourcePoolAnnotation]
}

func TestResourcePoolEnforcesBudgetsAndPreservesApprovedIsolation(t *testing.T) {
	ctx := context.Background()
	client := fakeClient()
	seedSourcePool(t, client, "approved")
	repository := NewRepository(client, testNamespace)
	reconciler := NewReconciler(client, repository, testNamespace, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	request := validCreateRequest("budget")
	request.Spec.Resources = lifecycle.ResourceSpec{CPU: "750m", Memory: "768Mi"}
	agent, _, err := repository.Create(ctx, "owner", "key", request)
	if err != nil {
		t.Fatal(err)
	}
	source, _ := client.Resource(SandboxTemplateGVR).Namespace(testNamespace).Get(ctx, "approved", metav1.GetOptions{})
	name, err := reconciler.ensureResourcePool(ctx, agent, "approved")
	if err != nil {
		t.Fatal(err)
	}
	derived, _ := client.Resource(SandboxTemplateGVR).Namespace(testNamespace).Get(ctx, name, metav1.GetOptions{})
	containers, _, _ := unstructured.NestedSlice(derived.Object, "spec", "podTemplate", "spec", "containers")
	runner := containers[0].(map[string]any)
	for _, category := range []string{"requests", "limits"} {
		cpu, _, _ := unstructured.NestedString(runner, "resources", category, "cpu")
		memory, _, _ := unstructured.NestedString(runner, "resources", category, "memory")
		if cpu != "750m" || memory != "768Mi" {
			t.Fatalf("%s budget = %s/%s", category, cpu, memory)
		}
	}
	// Remove only the two budget fields from each version, then compare all
	// remaining approved settings, including credentials, sidecars and policy.
	before := source.DeepCopy()
	after := derived.DeepCopy()
	for _, object := range []*unstructured.Unstructured{before, after} {
		values, _, _ := unstructured.NestedSlice(object.Object, "spec", "podTemplate", "spec", "containers")
		for _, category := range []string{"requests", "limits"} {
			unstructured.RemoveNestedField(values[0].(map[string]any), "resources", category, "cpu")
			unstructured.RemoveNestedField(values[0].(map[string]any), "resources", category, "memory")
		}
		_ = unstructured.SetNestedSlice(object.Object, values, "spec", "podTemplate", "spec", "containers")
	}
	if !reflect.DeepEqual(before.Object["spec"], after.Object["spec"]) {
		t.Fatal("resource binding changed an approved non-budget setting")
	}
	unchanged, _ := client.Resource(SandboxTemplateGVR).Namespace(testNamespace).Get(ctx, "approved", metav1.GetOptions{})
	if !reflect.DeepEqual(source.Object, unchanged.Object) {
		t.Fatal("resource binding mutated the source template")
	}
	pool, _ := client.Resource(SandboxWarmPoolGVR).Namespace(testNamespace).Get(ctx, name, metav1.GetOptions{})
	replicas, _, _ := unstructured.NestedInt64(pool.Object, "spec", "replicas")
	if replicas != 0 || !runtimeResourceOwnedBy(pool, agent) || !runtimeResourceOwnedBy(derived, agent) {
		t.Fatal("derived runtime resources lack safe capacity or ownership")
	}
	if repeated, err := reconciler.ensureResourcePool(ctx, agent, "approved"); err != nil || repeated != name {
		t.Fatalf("repeated binding = %s, %v", repeated, err)
	}
	other, _, _ := repository.Create(ctx, "another-owner", "other", validCreateRequest("other"))
	other.Spec.Resources = agent.Spec.Resources
	if another, err := reconciler.ensureResourcePool(ctx, other, "approved"); err != nil || another == name {
		t.Fatalf("agent resource isolation failed: %s, %v", another, err)
	}
}

func TestResourceBindingsNormalizeBudgetsAndRejectTampering(t *testing.T) {
	ctx := context.Background()
	client := fakeClient()
	seedSourcePool(t, client, "approved")
	repository := NewRepository(client, testNamespace)
	reconciler := NewReconciler(client, repository, testNamespace, nil, nil, nil)
	agent, _, _ := repository.Create(ctx, "owner", "key", validCreateRequest("budget"))
	agent.Spec.Resources = lifecycle.ResourceSpec{CPU: "1", Memory: "1Gi"}
	name, err := reconciler.ensureResourcePool(ctx, agent, "approved")
	if err != nil {
		t.Fatal(err)
	}
	agent.Spec.Resources = lifecycle.ResourceSpec{CPU: "1000m", Memory: "1024Mi"}
	if normalized, err := reconciler.ensureResourcePool(ctx, agent, "approved"); err != nil || normalized != name {
		t.Fatalf("equivalent budgets created a different binding: %s, %v", normalized, err)
	}
	template, _ := client.Resource(SandboxTemplateGVR).Namespace(testNamespace).Get(ctx, name, metav1.GetOptions{})
	_ = unstructured.SetNestedField(template.Object, true, "spec", "podTemplate", "spec", "hostNetwork")
	if _, err := client.Resource(SandboxTemplateGVR).Namespace(testNamespace).Update(ctx, template, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.ensureResourcePool(ctx, agent, "approved"); !errors.Is(err, errRuntimeBindingConflict) {
		t.Fatalf("tampered runtime was accepted: %v", err)
	}
}

func TestResourceBindingFailuresDoNotCreateRuntimeResources(t *testing.T) {
	for _, budget := range []lifecycle.ResourceSpec{{CPU: "0", Memory: "1Gi"}, {CPU: "0.0001", Memory: "1Gi"}, {CPU: "1", Memory: "bad"}} {
		ctx := context.Background()
		client := fakeClient()
		seedSourcePool(t, client, "approved")
		repository := NewRepository(client, testNamespace)
		reconciler := NewReconciler(client, repository, testNamespace, nil, nil, nil)
		agent, _, _ := repository.Create(ctx, "owner", "key", validCreateRequest("budget"))
		agent.Spec.Resources = budget
		if _, err := reconciler.ensureResourcePool(ctx, agent, "approved"); !errors.Is(err, errRuntimeProfileInvalid) {
			t.Fatalf("invalid budget %#v accepted: %v", budget, err)
		}
		list, err := client.Resource(SandboxTemplateGVR).Namespace(testNamespace).List(ctx, metav1.ListOptions{LabelSelector: RuntimeResourceLabel + "=true"})
		if err != nil || len(list.Items) != 0 {
			t.Fatal("invalid budget created runtime resources")
		}
	}
}

func TestRuntimeBindingsRejectForeignOwnershipAndNeverDeleteIt(t *testing.T) {
	ctx := context.Background()
	client := fakeClient()
	seedSourcePool(t, client, "approved")
	repository := NewRepository(client, testNamespace)
	reconciler := NewReconciler(client, repository, testNamespace, nil, nil, nil)
	agent, _, _ := repository.Create(ctx, "owner", "key", validCreateRequest("budget"))
	name, err := reconciler.ensureResourcePool(ctx, agent, "approved")
	if err != nil {
		t.Fatal(err)
	}
	for _, gvr := range []schema.GroupVersionResource{SandboxTemplateGVR, SandboxWarmPoolGVR} {
		object, err := client.Resource(gvr).Namespace(testNamespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		owners := object.GetOwnerReferences()
		owners[0].UID = types.UID("different-agent-with-the-same-name")
		object.SetOwnerReferences(owners)
		if _, err := client.Resource(gvr).Namespace(testNamespace).Update(ctx, object, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := reconciler.ensureResourcePool(ctx, agent, "approved"); !errors.Is(err, errRuntimeBindingConflict) {
		t.Fatalf("foreign runtime was accepted: %v", err)
	}
	client.ClearActions()
	if _, err := reconciler.cleanupResourcePools(ctx, agent, ""); err != nil {
		t.Fatal(err)
	}
	for _, action := range client.Actions() {
		if action.GetVerb() == "delete" || action.GetVerb() == "update" || action.GetVerb() == "patch" {
			t.Fatalf("foreign runtime resource was mutated: %#v", action)
		}
	}
}

func TestRuntimeResourceCleanupPreservesSourceAndCurrentBinding(t *testing.T) {
	ctx := context.Background()
	client := fakeClient()
	seedSourcePool(t, client, "approved")
	repository := NewRepository(client, testNamespace)
	reconciler := NewReconciler(client, repository, testNamespace, nil, nil, nil)
	agent, _, _ := repository.Create(ctx, "owner", "key", validCreateRequest("budget"))
	old, err := reconciler.ensureResourcePool(ctx, agent, "approved")
	if err != nil {
		t.Fatal(err)
	}
	agent.Spec.Resources.CPU = "2"
	current, err := reconciler.ensureResourcePool(ctx, agent, "approved")
	if err != nil || current == old {
		t.Fatalf("changed budget did not create a new immutable binding: %v", err)
	}
	if _, err := reconciler.cleanupResourcePools(ctx, agent, current); err != nil {
		t.Fatal(err)
	}
	for _, gvr := range []schema.GroupVersionResource{SandboxWarmPoolGVR, SandboxTemplateGVR} {
		resource := client.Resource(gvr).Namespace(testNamespace)
		if _, err := resource.Get(ctx, old, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
			t.Fatal("superseded runtime resource was retained")
		}
		for _, retained := range []string{current, "approved"} {
			if _, err := resource.Get(ctx, retained, metav1.GetOptions{}); err != nil {
				t.Fatalf("cleanup removed %s: %v", retained, err)
			}
		}
	}
	if clean, err := reconciler.cleanupResourcePools(ctx, agent, ""); clean || err != nil {
		t.Fatalf("deletion should wait for resources: %v, %v", clean, err)
	}
	if clean, err := reconciler.cleanupResourcePools(ctx, agent, ""); !clean || err != nil {
		t.Fatalf("runtime resource cleanup did not complete: %v, %v", clean, err)
	}
}
