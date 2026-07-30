// Package kubernetes adapts Sandherd lifecycle operations to Kubernetes without
// exposing Kubernetes resource details through the public API.
package kubernetes

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zjpiazza/sandherd/internal/lifecycle"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/client-go/dynamic"
)

var (
	AgentGVR        = schema.GroupVersionResource{Group: "sandherd.dev", Version: "v1alpha1", Resource: "agents"}
	SandboxClaimGVR = schema.GroupVersionResource{Group: "extensions.agents.x-k8s.io", Version: "v1beta1", Resource: "sandboxclaims"}
	SandboxGVR      = schema.GroupVersionResource{Group: "agents.x-k8s.io", Version: "v1beta1", Resource: "sandboxes"}
	PodGVR          = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"}
	PVCGVR          = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "persistentvolumeclaims"}
)

const (
	ManagedLabel      = "sandherd.dev/managed"
	AgentIDLabel      = "sandherd.dev/agent-id"
	OwnerHashLabel    = "sandherd.dev/owner-hash"
	idempotencyKey    = "sandherd.dev/idempotency-key"
	idempotencyHash   = "sandherd.dev/idempotency-hash"
	AgentFinalizer    = "sandherd.dev/sandbox-cleanup"
	DefaultFieldOwner = "sandherd-control-plane"
)

type ListOptions struct {
	Limit  int
	Cursor string
	State  lifecycle.State
	Name   string
}

type Repository struct {
	client    dynamic.Interface
	namespace string
	createMu  sync.Mutex
}

// RunnerTarget is an internal routing decision. It must never be serialized by
// the public API because it contains Kubernetes resource identity.
type RunnerTarget struct {
	AgentID     string
	SandboxName string
	Namespace   string
}

func NewRepository(client dynamic.Interface, namespace string) *Repository {
	return &Repository{client: client, namespace: namespace}
}

func (r *Repository) Create(ctx context.Context, owner, key string, request lifecycle.CreateRequest) (lifecycle.Agent, bool, error) {
	r.createMu.Lock()
	defer r.createMu.Unlock()
	requestBytes, _ := json.Marshal(request)
	hashBytes := sha256.Sum256(requestBytes)
	requestHash := hex.EncodeToString(hashBytes[:])

	existing, err := r.listForOwner(ctx, owner)
	if err != nil {
		return lifecycle.Agent{}, false, err
	}
	for i := range existing {
		agent, conversionErr := agentFromObject(&existing[i])
		if conversionErr != nil || agent.Owner != owner {
			continue
		}
		annotations := existing[i].GetAnnotations()
		if annotations[idempotencyKey] == key {
			if annotations[idempotencyHash] != requestHash {
				return lifecycle.Agent{}, false, lifecycle.NewError(http.StatusConflict, "idempotency_conflict", "the idempotency key was used with a different request")
			}
			return agent, false, nil
		}
		if agent.Name == request.Name {
			return lifecycle.Agent{}, false, lifecycle.NewError(http.StatusConflict, "name_conflict", "an agent with this name already exists")
		}
	}

	id := lifecycle.NewID()
	now := time.Now().UTC()
	object := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "sandherd.dev/v1alpha1",
		"kind":       "Agent",
		"metadata": map[string]any{
			"name":       resourceName(id),
			"namespace":  r.namespace,
			"generation": int64(1),
			"labels": map[string]any{
				ManagedLabel:   "true",
				AgentIDLabel:   id,
				OwnerHashLabel: ownerHash(owner),
			},
			"annotations": map[string]any{idempotencyKey: key, idempotencyHash: requestHash},
			"finalizers":  []any{AgentFinalizer},
		},
		"spec": map[string]any{
			"id":           id,
			"name":         request.Name,
			"owner":        owner,
			"desiredState": string(lifecycle.DesiredRunning),
			"agent":        toMap(request.Spec),
		},
	}}
	created, err := r.client.Resource(AgentGVR).Namespace(r.namespace).Create(ctx, object, metav1.CreateOptions{FieldManager: DefaultFieldOwner})
	if err != nil {
		return lifecycle.Agent{}, false, mapKubernetesError(err)
	}
	status := lifecycle.AgentStatus{State: lifecycle.StateRequested, ObservedGeneration: 0, LastTransitionAt: &now}
	updated, err := r.updateStatusObject(ctx, created, status)
	if err != nil {
		return lifecycle.Agent{}, false, err
	}
	agent, err := agentFromObject(updated)
	return agent, true, err
}

func (r *Repository) Get(ctx context.Context, owner, id string) (lifecycle.Agent, error) {
	object, err := r.client.Resource(AgentGVR).Namespace(r.namespace).Get(ctx, resourceName(id), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return lifecycle.Agent{}, lifecycle.NewError(http.StatusNotFound, "agent_not_found", "agent was not found")
	}
	if err != nil {
		return lifecycle.Agent{}, mapKubernetesError(err)
	}
	agent, err := agentFromObject(object)
	if err != nil {
		return lifecycle.Agent{}, err
	}
	if agent.Owner != owner {
		return lifecycle.Agent{}, lifecycle.NewError(http.StatusNotFound, "agent_not_found", "agent was not found")
	}
	return agent, nil
}

func (r *Repository) ResolveRunner(ctx context.Context, owner, id string) (RunnerTarget, error) {
	agent, err := r.Get(ctx, owner, id)
	if err != nil {
		return RunnerTarget{}, err
	}
	if agent.Status.State != lifecycle.StateRunning {
		result := lifecycle.NewError(http.StatusConflict, "agent_not_running", "the agent is not running")
		result.Retryable = agent.Status.State == lifecycle.StateProvisioning || agent.Status.State == lifecycle.StateStarting
		return RunnerTarget{}, result
	}
	claim, err := r.client.Resource(SandboxClaimGVR).Namespace(r.namespace).Get(ctx, claimName(id), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		result := lifecycle.NewError(http.StatusConflict, "agent_not_running", "the agent runner is not available")
		result.Retryable = true
		return RunnerTarget{}, result
	}
	if err != nil {
		return RunnerTarget{}, mapKubernetesError(err)
	}
	sandboxName, _, _ := unstructured.NestedString(claim.Object, "status", "sandbox", "name")
	if sandboxName == "" {
		result := lifecycle.NewError(http.StatusConflict, "agent_not_running", "the agent runner is not available")
		result.Retryable = true
		return RunnerTarget{}, result
	}
	return RunnerTarget{AgentID: agent.ID, SandboxName: sandboxName, Namespace: r.namespace}, nil
}

func (r *Repository) GetAny(ctx context.Context, id string) (lifecycle.Agent, error) {
	object, err := r.client.Resource(AgentGVR).Namespace(r.namespace).Get(ctx, resourceName(id), metav1.GetOptions{})
	if err != nil {
		return lifecycle.Agent{}, mapKubernetesError(err)
	}
	return agentFromObject(object)
}

func (r *Repository) List(ctx context.Context, owner string, options ListOptions) (lifecycle.AgentList, error) {
	objects, err := r.listForOwner(ctx, owner)
	if err != nil {
		return lifecycle.AgentList{}, err
	}
	agents := make([]lifecycle.Agent, 0, len(objects))
	for i := range objects {
		agent, conversionErr := agentFromObject(&objects[i])
		if conversionErr != nil {
			return lifecycle.AgentList{}, conversionErr
		}
		if agent.Owner != owner {
			continue
		}
		if options.State != "" && agent.Status.State != options.State {
			continue
		}
		if options.Name != "" && agent.Name != options.Name {
			continue
		}
		agents = append(agents, agent)
	}
	// UUIDv7 IDs are time ordered; the ID suffix provides the stable tie-breaker.
	sort.Slice(agents, func(i, j int) bool { return agents[i].ID < agents[j].ID })
	if options.Cursor != "" {
		cursor, decodeErr := base64.RawURLEncoding.DecodeString(options.Cursor)
		if decodeErr != nil {
			return lifecycle.AgentList{}, lifecycle.NewError(http.StatusBadRequest, "invalid_cursor", "list cursor is invalid")
		}
		index := sort.Search(len(agents), func(i int) bool { return agents[i].ID > string(cursor) })
		agents = agents[index:]
	}
	limit := options.Limit
	if limit == 0 {
		limit = 50
	}
	result := lifecycle.AgentList{Items: agents}
	if len(result.Items) > limit {
		result.NextCursor = base64.RawURLEncoding.EncodeToString([]byte(result.Items[limit-1].ID))
		result.Items = result.Items[:limit]
	}
	return result, nil
}

func (r *Repository) ListAll(ctx context.Context) ([]lifecycle.Agent, error) {
	list, err := r.client.Resource(AgentGVR).Namespace(r.namespace).List(ctx, metav1.ListOptions{LabelSelector: ManagedLabel + "=true"})
	if err != nil {
		return nil, mapKubernetesError(err)
	}
	result := make([]lifecycle.Agent, 0, len(list.Items))
	for i := range list.Items {
		agent, conversionErr := agentFromObject(&list.Items[i])
		if conversionErr != nil {
			return nil, conversionErr
		}
		result = append(result, agent)
	}
	return result, nil
}

func (r *Repository) SetDesired(ctx context.Context, owner, id, ifMatch string, desired lifecycle.DesiredState, transitional lifecycle.State) (lifecycle.Agent, error) {
	object, err := r.client.Resource(AgentGVR).Namespace(r.namespace).Get(ctx, resourceName(id), metav1.GetOptions{})
	if err != nil {
		return lifecycle.Agent{}, mapKubernetesError(err)
	}
	agent, err := agentFromObject(object)
	if err != nil {
		return lifecycle.Agent{}, err
	}
	if agent.Owner != owner {
		return lifecycle.Agent{}, lifecycle.NewError(http.StatusNotFound, "agent_not_found", "agent was not found")
	}
	if ifMatch != "" && ifMatch != ETag(agent.ResourceVersion) {
		return lifecycle.Agent{}, lifecycle.NewError(http.StatusPreconditionFailed, "precondition_failed", "the agent changed since it was read")
	}
	if err := unstructured.SetNestedField(object.Object, string(desired), "spec", "desiredState"); err != nil {
		return lifecycle.Agent{}, err
	}
	object, err = r.client.Resource(AgentGVR).Namespace(r.namespace).Update(ctx, object, metav1.UpdateOptions{FieldManager: DefaultFieldOwner})
	if err != nil {
		return lifecycle.Agent{}, mapKubernetesError(err)
	}
	now := time.Now().UTC()
	agent.Status.State = transitional
	agent.Status.Reason = ""
	agent.Status.Message = ""
	agent.Status.LastTransitionAt = &now
	object, err = r.updateStatusObject(ctx, object, agent.Status)
	if err != nil {
		return lifecycle.Agent{}, err
	}
	return agentFromObject(object)
}

func (r *Repository) SetStatus(ctx context.Context, id string, status lifecycle.AgentStatus) (lifecycle.Agent, error) {
	object, err := r.client.Resource(AgentGVR).Namespace(r.namespace).Get(ctx, resourceName(id), metav1.GetOptions{})
	if err != nil {
		return lifecycle.Agent{}, mapKubernetesError(err)
	}
	object, err = r.updateStatusObject(ctx, object, status)
	if err != nil {
		return lifecycle.Agent{}, err
	}
	return agentFromObject(object)
}

func (r *Repository) FinalizeDelete(ctx context.Context, id string) error {
	resources := r.client.Resource(AgentGVR).Namespace(r.namespace)
	object, err := resources.Get(ctx, resourceName(id), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return mapKubernetesError(err)
	}
	object.SetFinalizers(removeString(object.GetFinalizers(), AgentFinalizer))
	if _, err = resources.Update(ctx, object, metav1.UpdateOptions{FieldManager: DefaultFieldOwner}); err != nil {
		return mapKubernetesError(err)
	}
	if err = resources.Delete(ctx, object.GetName(), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return mapKubernetesError(err)
	}
	return nil
}

func (r *Repository) updateStatusObject(ctx context.Context, object *unstructured.Unstructured, status lifecycle.AgentStatus) (*unstructured.Unstructured, error) {
	if err := unstructured.SetNestedMap(object.Object, toMap(status), "status"); err != nil {
		return nil, err
	}
	updated, err := r.client.Resource(AgentGVR).Namespace(r.namespace).UpdateStatus(ctx, object, metav1.UpdateOptions{FieldManager: DefaultFieldOwner})
	if err != nil {
		return nil, mapKubernetesError(err)
	}
	return updated, nil
}

func (r *Repository) listForOwner(ctx context.Context, owner string) ([]unstructured.Unstructured, error) {
	requirement, _ := labels.NewRequirement(OwnerHashLabel, selection.Equals, []string{ownerHash(owner)})
	list, err := r.client.Resource(AgentGVR).Namespace(r.namespace).List(ctx, metav1.ListOptions{LabelSelector: requirement.String()})
	if err != nil {
		return nil, mapKubernetesError(err)
	}
	return list.Items, nil
}

func agentFromObject(object *unstructured.Unstructured) (lifecycle.Agent, error) {
	var result lifecycle.Agent
	result.APIVersion = "v1alpha1"
	result.ID, _, _ = unstructured.NestedString(object.Object, "spec", "id")
	result.Name, _, _ = unstructured.NestedString(object.Object, "spec", "name")
	result.Owner, _, _ = unstructured.NestedString(object.Object, "spec", "owner")
	desired, _, _ := unstructured.NestedString(object.Object, "spec", "desiredState")
	result.DesiredState = lifecycle.DesiredState(desired)
	result.Generation = object.GetGeneration()
	if result.Generation == 0 {
		result.Generation = 1
	}
	result.ResourceVersion = object.GetResourceVersion()
	result.CreatedAt = object.GetCreationTimestamp().Time.UTC()
	result.UpdatedAt = result.CreatedAt
	if result.CreatedAt.IsZero() {
		result.CreatedAt = time.Now().UTC()
		result.UpdatedAt = result.CreatedAt
	}
	if raw, ok, _ := unstructured.NestedMap(object.Object, "spec", "agent"); ok {
		if err := fromMap(raw, &result.Spec); err != nil {
			return result, fmt.Errorf("decode Agent spec: %w", err)
		}
	}
	if raw, ok, _ := unstructured.NestedMap(object.Object, "status"); ok {
		if err := fromMap(raw, &result.Status); err != nil {
			return result, fmt.Errorf("decode Agent status: %w", err)
		}
	}
	if result.Status.State == "" {
		result.Status.State = lifecycle.StateRequested
	}
	if result.Status.LastTransitionAt != nil {
		result.UpdatedAt = result.Status.LastTransitionAt.UTC()
	}
	return result, nil
}

func ETag(resourceVersion string) string {
	if resourceVersion == "" {
		resourceVersion = "0"
	}
	return strconv.Quote(resourceVersion)
}

func resourceName(id string) string { return "agent-" + strings.ReplaceAll(id, "-", "") }

func ownerHash(owner string) string {
	sum := sha256.Sum256([]byte(owner))
	return hex.EncodeToString(sum[:8])
}

func toMap(value any) map[string]any {
	encoded, _ := json.Marshal(value)
	var result map[string]any
	_ = json.Unmarshal(encoded, &result)
	return result
}

func fromMap(value map[string]any, target any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(encoded, target)
}

func removeString(values []string, target string) []string {
	result := values[:0]
	for _, value := range values {
		if value != target {
			result = append(result, value)
		}
	}
	return result
}

func mapKubernetesError(err error) error {
	if typed, ok := err.(*lifecycle.Error); ok {
		return typed
	}
	switch {
	case apierrors.IsNotFound(err):
		return lifecycle.NewError(http.StatusNotFound, "agent_not_found", "agent was not found")
	case apierrors.IsConflict(err):
		result := lifecycle.NewError(http.StatusConflict, "resource_conflict", "the resource changed during the operation")
		result.Retryable = true
		result.Cause = err
		return result
	case apierrors.IsForbidden(err):
		return lifecycle.NewError(http.StatusForbidden, "forbidden", "the control plane is not authorized for this operation")
	default:
		result := lifecycle.NewError(http.StatusInternalServerError, "internal_error", "a Kubernetes operation failed")
		result.Retryable = true
		result.Cause = err
		return result
	}
}
