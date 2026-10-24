package kubernetes

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/zjpiazza/sandherd/internal/lifecycle"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

const (
	RuntimeResourceLabel = "sandherd.dev/runtime-resource"
	SourcePoolAnnotation = "sandherd.dev/source-pool"
)

var (
	errRuntimeProfileUnavailable = errors.New("approved runtime pool or template is unavailable")
	errRuntimeProfileInvalid     = errors.New("approved runtime template cannot bind the requested resources")
	errRuntimeBindingConflict    = errors.New("derived runtime resource has conflicting ownership or configuration")
)

// ensureResourcePool derives an immutable, Agent-owned template from trusted
// operator configuration. Agent Sandbox v0.5.3 cannot override container
// resources on a claim, so budgets must exist on its template before scheduling.
func (r *Reconciler) ensureResourcePool(ctx context.Context, agent lifecycle.Agent, sourcePool string) (string, error) {
	if agent.ResourceUID == "" {
		return "", errRuntimeBindingConflict
	}
	pool, err := r.client.Resource(SandboxWarmPoolGVR).Namespace(r.namespace).Get(ctx, sourcePool, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "", errRuntimeProfileUnavailable
	}
	if err != nil {
		return "", mapKubernetesError(err)
	}
	templateName, _, _ := unstructured.NestedString(pool.Object, "spec", "sandboxTemplateRef", "name")
	if templateName == "" {
		return "", errRuntimeProfileInvalid
	}
	source, err := r.client.Resource(SandboxTemplateGVR).Namespace(r.namespace).Get(ctx, templateName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "", errRuntimeProfileUnavailable
	}
	if err != nil {
		return "", mapKubernetesError(err)
	}
	spec, found, err := unstructured.NestedMap(source.Object, "spec")
	if err != nil || !found {
		return "", errRuntimeProfileInvalid
	}
	if err := bindRunnerResources(spec, agent.Spec.Resources); err != nil {
		return "", err
	}
	// Hash the approved source and effective spec, not resourceVersion or
	// status, so equivalent quantities and controller restarts reuse bindings.
	encoded, err := json.Marshal([]any{sourcePool, spec})
	if err != nil {
		return "", errRuntimeProfileInvalid
	}
	digest := sha256.Sum256(encoded)
	name := "rt-" + strings.ReplaceAll(agent.ID, "-", "") + "-" + hex.EncodeToString(digest[:6])
	metadata := func() map[string]any {
		return map[string]any{
			"name": name, "namespace": r.namespace,
			"labels":      map[string]any{ManagedLabel: "true", RuntimeResourceLabel: "true", AgentIDLabel: agent.ID, OwnerHashLabel: ownerHash(agent.Owner)},
			"annotations": map[string]any{SourcePoolAnnotation: sourcePool},
			"ownerReferences": []any{map[string]any{
				"apiVersion": "sandherd.dev/v1alpha1", "kind": "Agent", "name": resourceName(agent.ID),
				"uid": agent.ResourceUID, "controller": true, "blockOwnerDeletion": true,
			}},
		}
	}
	template := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "extensions.agents.x-k8s.io/v1beta1", "kind": "SandboxTemplate", "metadata": metadata(), "spec": spec,
	}}
	if err := r.ensureRuntimeResource(ctx, SandboxTemplateGVR, template, agent); err != nil {
		return "", err
	}
	derivedPool := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "extensions.agents.x-k8s.io/v1beta1", "kind": "SandboxWarmPool", "metadata": metadata(),
		"spec": map[string]any{
			// Claim-specific environment and PVCs force cold starts in v0.5.3.
			// A per-Agent pool must never prestart an unassigned credentialed Pod.
			"replicas": int64(0), "sandboxTemplateRef": map[string]any{"name": name},
			"updateStrategy": map[string]any{"type": "OnReplenish"},
		},
	}}
	if err := r.ensureRuntimeResource(ctx, SandboxWarmPoolGVR, derivedPool, agent); err != nil {
		return "", err
	}
	return name, nil
}

func bindRunnerResources(spec map[string]any, requested lifecycle.ResourceSpec) error {
	cpu, err := resource.ParseQuantity(requested.CPU)
	if err != nil || cpu.Sign() <= 0 || cpu.Cmp(*resource.NewMilliQuantity(cpu.MilliValue(), resource.DecimalSI)) != 0 {
		return errRuntimeProfileInvalid
	}
	memory, err := resource.ParseQuantity(requested.Memory)
	if err != nil || memory.Sign() <= 0 {
		return errRuntimeProfileInvalid
	}
	containers, found, err := unstructured.NestedSlice(spec, "podTemplate", "spec", "containers")
	if err != nil || !found {
		return errRuntimeProfileInvalid
	}
	matched := 0
	for _, value := range containers {
		container, ok := value.(map[string]any)
		if !ok {
			return errRuntimeProfileInvalid
		}
		if container["name"] != "runner" {
			continue
		}
		matched++
		for _, category := range []string{"requests", "limits"} {
			if err := unstructured.SetNestedField(container, cpu.String(), "resources", category, "cpu"); err != nil {
				return errRuntimeProfileInvalid
			}
			if err := unstructured.SetNestedField(container, memory.String(), "resources", category, "memory"); err != nil {
				return errRuntimeProfileInvalid
			}
		}
	}
	if matched != 1 {
		return errRuntimeProfileInvalid
	}
	if err := unstructured.SetNestedSlice(spec, containers, "podTemplate", "spec", "containers"); err != nil {
		return errRuntimeProfileInvalid
	}
	return nil
}

func (r *Reconciler) ensureRuntimeResource(ctx context.Context, gvr schema.GroupVersionResource, desired *unstructured.Unstructured, agent lifecycle.Agent) error {
	client := r.client.Resource(gvr).Namespace(r.namespace)
	current, err := client.Get(ctx, desired.GetName(), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		current, err = client.Create(ctx, desired, metav1.CreateOptions{FieldManager: DefaultFieldOwner})
		if apierrors.IsAlreadyExists(err) {
			current, err = client.Get(ctx, desired.GetName(), metav1.GetOptions{})
		}
	}
	if err != nil {
		return mapKubernetesError(err)
	}
	if !runtimeResourceOwnedBy(current, agent) || current.GetDeletionTimestamp() != nil || !reflect.DeepEqual(current.Object["spec"], desired.Object["spec"]) {
		return errRuntimeBindingConflict
	}
	return nil
}

func runtimeResourceOwnedBy(object *unstructured.Unstructured, agent lifecycle.Agent) bool {
	if agent.ResourceUID == "" || object.GetLabels()[RuntimeResourceLabel] != "true" || object.GetLabels()[AgentIDLabel] != agent.ID {
		return false
	}
	for _, owner := range object.GetOwnerReferences() {
		if owner.APIVersion == "sandherd.dev/v1alpha1" && owner.Kind == "Agent" && owner.Name == resourceName(agent.ID) && string(owner.UID) == agent.ResourceUID && owner.Controller != nil && *owner.Controller {
			return true
		}
	}
	return false
}

// cleanupResourcePools removes superseded per-Agent bindings, or all bindings
// before final deletion. It never deletes an operator's source templates/pools.
func (r *Reconciler) cleanupResourcePools(ctx context.Context, agent lifecycle.Agent, keep string) (bool, error) {
	complete := true
	for _, gvr := range []schema.GroupVersionResource{SandboxWarmPoolGVR, SandboxTemplateGVR} {
		client := r.client.Resource(gvr).Namespace(r.namespace)
		list, err := client.List(ctx, metav1.ListOptions{LabelSelector: fmt.Sprintf("%s=true,%s=%s", RuntimeResourceLabel, AgentIDLabel, agent.ID)})
		if err != nil {
			return false, mapKubernetesError(err)
		}
		for i := range list.Items {
			object := &list.Items[i]
			if object.GetName() == keep || !runtimeResourceOwnedBy(object, agent) {
				continue
			}
			complete = false
			if object.GetDeletionTimestamp() != nil {
				continue
			}
			policy := metav1.DeletePropagationForeground
			uid := types.UID(object.GetUID())
			if err := client.Delete(ctx, object.GetName(), metav1.DeleteOptions{PropagationPolicy: &policy, Preconditions: &metav1.Preconditions{UID: &uid}}); err != nil && !apierrors.IsNotFound(err) {
				return false, mapKubernetesError(err)
			}
		}
	}
	return complete, nil
}
