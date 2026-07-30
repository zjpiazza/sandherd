package kubernetes

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/zjpiazza/sandherd/internal/lifecycle"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

type EventPublisher interface {
	Publish(lifecycle.Event)
}

type Reconciler struct {
	client           dynamic.Interface
	repository       *Repository
	namespace        string
	profiles         map[string]string
	storageProfiles  map[string]string
	secretProfiles   map[string]string
	events           EventPublisher
	logger           *slog.Logger
	provisionTimeout time.Duration
}

func NewReconciler(client dynamic.Interface, repository *Repository, namespace string, profiles map[string]string, events EventPublisher, logger *slog.Logger) *Reconciler {
	return &Reconciler{
		client: client, repository: repository, namespace: namespace, profiles: profiles,
		storageProfiles: map[string]string{"default": ""}, secretProfiles: map[string]string{},
		events: events, logger: logger, provisionTimeout: 10 * time.Minute,
	}
}

func (r *Reconciler) ConfigureWorkspaceProfiles(storageProfiles, secretProfiles map[string]string) {
	if storageProfiles != nil {
		r.storageProfiles = storageProfiles
	}
	if secretProfiles != nil {
		r.secretProfiles = secretProfiles
	}
}

func (r *Reconciler) Reconcile(ctx context.Context, id string) error {
	agent, err := r.repository.GetAny(ctx, id)
	if err != nil {
		if typed, ok := err.(*lifecycle.Error); ok && typed.Code == "agent_not_found" {
			return nil
		}
		return err
	}
	switch agent.DesiredState {
	case lifecycle.DesiredDeleted:
		return r.reconcileDelete(ctx, agent)
	case lifecycle.DesiredStopped:
		return r.reconcileStopped(ctx, agent)
	default:
		return r.reconcileRunning(ctx, agent)
	}
}

func (r *Reconciler) reconcileRunning(ctx context.Context, agent lifecycle.Agent) error {
	if (agent.Status.State == lifecycle.StateRequested || agent.Status.State == lifecycle.StateProvisioning || agent.Status.State == lifecycle.StateStarting) &&
		agent.Status.LastTransitionAt != nil && time.Since(*agent.Status.LastTransitionAt) > r.provisionTimeout {
		return r.transition(ctx, agent, lifecycle.StateFailed, "provisioning_timeout", "sandbox and runner readiness timed out")
	}
	pool, approved := r.profiles[agent.Spec.SandboxProfile]
	if !approved {
		return r.transition(ctx, agent, lifecycle.StateFailed, "sandbox_profile_not_found", "the sandbox profile is not approved")
	}
	if agent.Spec.SecretProfile != "" {
		pool, approved = r.secretProfiles[agent.Spec.SecretProfile]
		if !approved {
			return r.transition(ctx, agent, lifecycle.StateFailed, "secret_profile_not_found", "the repository secret profile is not approved")
		}
	}
	storageClass, approved := r.storageProfiles[agent.Spec.Workspace.StorageProfile]
	if !approved {
		return r.transition(ctx, agent, lifecycle.StateFailed, "storage_profile_not_found", "the workspace storage profile is not approved")
	}
	claimName := claimName(agent.ID)
	claims := r.client.Resource(SandboxClaimGVR).Namespace(r.namespace)
	claim, err := claims.Get(ctx, claimName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		claim = sandboxClaimWithStorage(agent, r.namespace, pool, storageClass)
		if _, err = claims.Create(ctx, claim, metav1.CreateOptions{FieldManager: DefaultFieldOwner}); err != nil && !apierrors.IsAlreadyExists(err) {
			return mapKubernetesError(err)
		}
		return r.transition(ctx, agent, lifecycle.StateProvisioning, "sandbox_claim_created", "the sandbox claim is provisioning")
	}
	if err != nil {
		return mapKubernetesError(err)
	}
	if reason, message, failed := failedCondition(claim); failed {
		r.logger.Warn("sandbox claim reported failure", "agent_id", agent.ID, "reason", reason, "detail", message)
		return r.transition(ctx, agent, lifecycle.StateFailed, "sandbox_failed", "sandbox provisioning failed")
	}
	sandboxName, _, _ := unstructured.NestedString(claim.Object, "status", "sandbox", "name")
	if sandboxName == "" {
		return r.transition(ctx, agent, lifecycle.StateProvisioning, "waiting_for_sandbox", "waiting for a sandbox assignment")
	}
	sandbox, err := r.client.Resource(SandboxGVR).Namespace(r.namespace).Get(ctx, sandboxName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return r.transition(ctx, agent, lifecycle.StateProvisioning, "waiting_for_sandbox", "waiting for the assigned sandbox")
	}
	if err != nil {
		return mapKubernetesError(err)
	}
	operatingMode, _, _ := unstructured.NestedString(sandbox.Object, "spec", "operatingMode")
	if operatingMode == "Suspended" {
		if err := r.patchOperatingMode(ctx, sandboxName, "Running"); err != nil {
			return err
		}
		return r.transition(ctx, agent, lifecycle.StateStarting, "sandbox_resuming", "the sandbox is resuming")
	}
	pod, err := r.client.Resource(PodGVR).Namespace(r.namespace).Get(ctx, sandboxName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return r.transition(ctx, agent, lifecycle.StateStarting, "waiting_for_runner", "waiting for the runner pod")
	}
	if err != nil {
		return mapKubernetesError(err)
	}
	phase, _, _ := unstructured.NestedString(pod.Object, "status", "phase")
	if reason, message, failed := bootstrapFailure(pod); failed {
		return r.transition(ctx, agent, lifecycle.StateFailed, reason, message)
	}
	if phase == "Failed" {
		return r.transition(ctx, agent, lifecycle.StateFailed, "runner_failed", "the runner pod failed")
	}
	if conditionTrue(claim, "Ready") && conditionTrue(pod, "Ready") {
		return r.transition(ctx, agent, lifecycle.StateRunning, "", "")
	}
	return r.transition(ctx, agent, lifecycle.StateStarting, "waiting_for_runner", "waiting for runner readiness")
}

func (r *Reconciler) reconcileStopped(ctx context.Context, agent lifecycle.Agent) error {
	claim, err := r.client.Resource(SandboxClaimGVR).Namespace(r.namespace).Get(ctx, claimName(agent.ID), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return r.transition(ctx, agent, lifecycle.StateStopped, "", "")
	}
	if err != nil {
		return mapKubernetesError(err)
	}
	sandboxName, _, _ := unstructured.NestedString(claim.Object, "status", "sandbox", "name")
	if sandboxName == "" {
		return r.transition(ctx, agent, lifecycle.StateStopped, "", "")
	}
	sandbox, err := r.client.Resource(SandboxGVR).Namespace(r.namespace).Get(ctx, sandboxName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return r.transition(ctx, agent, lifecycle.StateStopped, "", "")
	}
	if err != nil {
		return mapKubernetesError(err)
	}
	if conditionTrue(sandbox, "Suspended") {
		return r.transition(ctx, agent, lifecycle.StateStopped, "", "")
	}
	operatingMode, _, _ := unstructured.NestedString(sandbox.Object, "spec", "operatingMode")
	if operatingMode != "Suspended" {
		if err := r.patchOperatingMode(ctx, sandboxName, "Suspended"); err != nil {
			return err
		}
	}
	return r.transition(ctx, agent, lifecycle.StateStopping, "sandbox_stopping", "the sandbox is stopping")
}

func (r *Reconciler) reconcileDelete(ctx context.Context, agent lifecycle.Agent) error {
	if agent.Spec.Workspace.RetentionPolicy == "retain" {
		if err := r.retainPVCs(ctx, agent.ID); err != nil {
			return err
		}
	}
	claims := r.client.Resource(SandboxClaimGVR).Namespace(r.namespace)
	_, err := claims.Get(ctx, claimName(agent.ID), metav1.GetOptions{})
	if err == nil {
		policy := metav1.DeletePropagationForeground
		if err := claims.Delete(ctx, claimName(agent.ID), metav1.DeleteOptions{PropagationPolicy: &policy}); err != nil && !apierrors.IsNotFound(err) {
			return mapKubernetesError(err)
		}
		return r.transition(ctx, agent, lifecycle.StateDeleting, "sandbox_deleting", "the sandbox is being deleted")
	}
	if !apierrors.IsNotFound(err) {
		return mapKubernetesError(err)
	}
	if r.events != nil {
		r.events.Publish(lifecycle.Event{Type: "agent.deleted", AgentID: agent.ID, PreviousState: agent.Status.State, OccurredAt: time.Now().UTC(), Owner: agent.Owner})
	}
	return r.repository.FinalizeDelete(ctx, agent.ID)
}

func (r *Reconciler) patchOperatingMode(ctx context.Context, sandboxName, operatingMode string) error {
	patch := []byte(fmt.Sprintf(`{"spec":{"operatingMode":%q}}`, operatingMode))
	_, err := r.client.Resource(SandboxGVR).Namespace(r.namespace).Patch(ctx, sandboxName, types.MergePatchType, patch, metav1.PatchOptions{FieldManager: DefaultFieldOwner})
	if err != nil {
		return mapKubernetesError(err)
	}
	return nil
}

func (r *Reconciler) retainPVCs(ctx context.Context, agentID string) error {
	list, err := r.client.Resource(PVCGVR).Namespace(r.namespace).List(ctx, metav1.ListOptions{LabelSelector: AgentIDLabel + "=" + agentID})
	if err != nil {
		return mapKubernetesError(err)
	}
	for i := range list.Items {
		pvc := list.Items[i].DeepCopy()
		pvc.SetOwnerReferences(nil)
		if _, err := r.client.Resource(PVCGVR).Namespace(r.namespace).Update(ctx, pvc, metav1.UpdateOptions{FieldManager: DefaultFieldOwner}); err != nil {
			return mapKubernetesError(err)
		}
	}
	return nil
}

func (r *Reconciler) transition(ctx context.Context, agent lifecycle.Agent, state lifecycle.State, reason, message string) error {
	if agent.Status.State == state && agent.Status.Reason == reason && agent.Status.Message == message && agent.Status.ObservedGeneration == agent.Generation {
		return nil
	}
	previous := agent.Status.State
	now := time.Now().UTC()
	status := agent.Status
	status.State = state
	status.ObservedGeneration = agent.Generation
	status.Reason = reason
	status.Message = message
	status.LastTransitionAt = &now
	if state == lifecycle.StateRunning && status.ReadyAt == nil {
		status.ReadyAt = &now
	}
	if state == lifecycle.StateStopped {
		status.StoppedAt = &now
	}
	updated, err := r.repository.SetStatus(ctx, agent.ID, status)
	if err != nil {
		return err
	}
	if previous != state && r.events != nil {
		r.events.Publish(lifecycle.Event{Type: "agent.state_changed", AgentID: agent.ID, PreviousState: previous, State: state, OccurredAt: now, Owner: updated.Owner})
	}
	r.logger.Info("agent state reconciled", "agent_id", agent.ID, "previous_state", previous, "state", state, "reason", reason)
	return nil
}

func sandboxClaim(agent lifecycle.Agent, namespace, pool string) *unstructured.Unstructured {
	return sandboxClaimWithStorage(agent, namespace, pool, "")
}

func sandboxClaimWithStorage(agent lifecycle.Agent, namespace, pool, storageClass string) *unstructured.Unstructured {
	labels := map[string]any{ManagedLabel: "true", AgentIDLabel: agent.ID, OwnerHashLabel: ownerHash(agent.Owner)}
	environment := []any{
		map[string]any{"name": "SANDHERD_AGENT_ID", "value": agent.ID, "containerName": "runner"},
	}
	if agent.Spec.Repository != nil {
		environment = append(environment,
			map[string]any{"name": "SANDHERD_REPOSITORY_URL", "value": agent.Spec.Repository.URL, "containerName": "workspace-bootstrap"},
			map[string]any{"name": "SANDHERD_REPOSITORY_REVISION", "value": agent.Spec.Repository.Revision, "containerName": "workspace-bootstrap"},
		)
	}
	volumeSpec := map[string]any{
		"accessModes": []any{"ReadWriteOnce"},
		"resources":   map[string]any{"requests": map[string]any{"storage": agent.Spec.Workspace.Size}},
	}
	if storageClass != "" {
		volumeSpec["storageClassName"] = storageClass
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "extensions.agents.x-k8s.io/v1beta1",
		"kind":       "SandboxClaim",
		"metadata":   map[string]any{"name": claimName(agent.ID), "namespace": namespace, "labels": labels},
		"spec": map[string]any{
			"warmPoolRef":           map[string]any{"name": pool},
			"lifecycle":             map[string]any{"shutdownPolicy": "DeleteForeground"},
			"additionalPodMetadata": map[string]any{"labels": labels},
			"env":                   environment,
			"volumeClaimTemplates": []any{map[string]any{
				"metadata": map[string]any{"name": "workspace", "labels": labels},
				"spec":     volumeSpec,
			}},
		},
	}}
}

func bootstrapFailure(pod *unstructured.Unstructured) (string, string, bool) {
	statuses, _, _ := unstructured.NestedSlice(pod.Object, "status", "initContainerStatuses")
	for _, value := range statuses {
		status, ok := value.(map[string]any)
		if !ok || status["name"] != "workspace-bootstrap" {
			continue
		}
		exitCode, found, _ := unstructured.NestedInt64(status, "state", "terminated", "exitCode")
		if !found || exitCode == 0 {
			continue
		}
		switch exitCode {
		case 20:
			return "bootstrap_invalid", "workspace bootstrap configuration is invalid", true
		case 21:
			return "workspace_unsafe", "workspace bootstrap rejected an unsafe workspace", true
		case 22:
			return "repository_auth_failed", "repository authentication failed", true
		case 23:
			return "repository_bootstrap_failed", "repository bootstrap failed", true
		case 24:
			return "workspace_full", "the workspace is full", true
		case 25:
			return "bootstrap_timeout", "repository bootstrap timed out", true
		default:
			return "bootstrap_failed", "workspace bootstrap failed", true
		}
	}
	return "", "", false
}

func claimName(id string) string { return "sandbox-" + strings.ReplaceAll(id, "-", "") }

func conditionTrue(object *unstructured.Unstructured, conditionType string) bool {
	conditions, _, _ := unstructured.NestedSlice(object.Object, "status", "conditions")
	for _, item := range conditions {
		condition, ok := item.(map[string]any)
		if ok && condition["type"] == conditionType && condition["status"] == "True" {
			return true
		}
	}
	return false
}

func failedCondition(object *unstructured.Unstructured) (string, string, bool) {
	conditions, _, _ := unstructured.NestedSlice(object.Object, "status", "conditions")
	for _, item := range conditions {
		condition, ok := item.(map[string]any)
		if !ok || condition["status"] != "False" {
			continue
		}
		reason, _ := condition["reason"].(string)
		if strings.Contains(strings.ToLower(reason), "fail") || strings.Contains(strings.ToLower(reason), "error") {
			message, _ := condition["message"].(string)
			return reason, message, true
		}
	}
	return "", "", false
}
