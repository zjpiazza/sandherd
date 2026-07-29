package kubernetes

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/zjpiazza/sandherd/internal/lifecycle"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
)

type Controller struct {
	client     dynamic.Interface
	repository *Repository
	reconciler *Reconciler
	namespace  string
	logger     *slog.Logger
	queue      chan string
	mu         sync.Mutex
	pending    map[string]bool
	dirty      map[string]bool
	retries    map[string]int
	maxRetries int
	resync     time.Duration
}

func NewController(client dynamic.Interface, repository *Repository, reconciler *Reconciler, namespace string, logger *slog.Logger) *Controller {
	return &Controller{
		client: client, repository: repository, reconciler: reconciler, namespace: namespace, logger: logger,
		queue: make(chan string, 1024), pending: make(map[string]bool), dirty: make(map[string]bool), retries: make(map[string]int), maxRetries: 5, resync: 30 * time.Second,
	}
}

func (c *Controller) Run(ctx context.Context) error {
	for _, resource := range []schema.GroupVersionResource{AgentGVR, SandboxClaimGVR, SandboxGVR, PodGVR} {
		go c.watchResource(ctx, resource)
	}
	for range 2 {
		go c.worker(ctx)
	}
	if err := c.resyncAll(ctx); err != nil {
		return err
	}
	ticker := time.NewTicker(c.resync)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if err := c.resyncAll(ctx); err != nil {
				c.logger.Error("lifecycle resync failed", "error", err)
			}
			if err := c.deleteOrphanClaims(ctx); err != nil {
				c.logger.Error("orphan sweep failed", "error", err)
			}
		case <-ctx.Done():
			return nil
		}
	}
}

func (c *Controller) Enqueue(id string) {
	if id == "" {
		return
	}
	c.mu.Lock()
	if c.pending[id] {
		c.dirty[id] = true
		c.mu.Unlock()
		return
	}
	c.pending[id] = true
	c.mu.Unlock()
	select {
	case c.queue <- id:
	default:
		c.mu.Lock()
		delete(c.pending, id)
		delete(c.dirty, id)
		c.mu.Unlock()
		c.logger.Error("lifecycle queue is full", "agent_id", id)
	}
}

func (c *Controller) worker(ctx context.Context) {
	for {
		select {
		case id := <-c.queue:
			err := c.reconciler.Reconcile(ctx, id)
			c.mu.Lock()
			if err == nil {
				delete(c.retries, id)
				if c.dirty[id] {
					delete(c.dirty, id)
					c.mu.Unlock()
					select {
					case c.queue <- id:
					case <-ctx.Done():
					}
					continue
				}
				delete(c.pending, id)
				c.mu.Unlock()
				continue
			}
			delete(c.pending, id)
			delete(c.dirty, id)
			attempt := c.retries[id] + 1
			c.retries[id] = attempt
			c.mu.Unlock()
			c.logger.Error("agent reconciliation failed", "agent_id", id, "attempt", attempt, "error", err)
			if attempt <= c.maxRetries {
				delay := time.Duration(1<<min(attempt-1, 5)) * 100 * time.Millisecond
				time.AfterFunc(delay, func() { c.Enqueue(id) })
			} else {
				c.markRetryExhausted(ctx, id)
			}
		case <-ctx.Done():
			return
		}
	}
}

func (c *Controller) watchResource(ctx context.Context, resource schema.GroupVersionResource) {
	backoff := 100 * time.Millisecond
	for ctx.Err() == nil {
		stream, err := c.client.Resource(resource).Namespace(c.namespace).Watch(ctx, metav1.ListOptions{LabelSelector: ManagedLabel + "=true", AllowWatchBookmarks: true})
		if err != nil {
			c.logger.Error("resource watch failed", "resource", resource.Resource, "error", err)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return
			}
			if backoff < 5*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = 100 * time.Millisecond
		c.consumeWatch(ctx, stream)
	}
}

func (c *Controller) consumeWatch(ctx context.Context, stream watch.Interface) {
	defer stream.Stop()
	for {
		select {
		case event, open := <-stream.ResultChan():
			if !open {
				return
			}
			object, ok := event.Object.(*unstructured.Unstructured)
			if !ok {
				continue
			}
			id := object.GetLabels()[AgentIDLabel]
			if id == "" {
				id, _, _ = unstructured.NestedString(object.Object, "spec", "id")
			}
			c.Enqueue(id)
		case <-ctx.Done():
			return
		}
	}
}

func (c *Controller) resyncAll(ctx context.Context) error {
	agents, err := c.repository.ListAll(ctx)
	if err != nil {
		return err
	}
	for _, agent := range agents {
		c.Enqueue(agent.ID)
	}
	return nil
}

func (c *Controller) deleteOrphanClaims(ctx context.Context) error {
	claims, err := c.client.Resource(SandboxClaimGVR).Namespace(c.namespace).List(ctx, metav1.ListOptions{LabelSelector: ManagedLabel + "=true"})
	if err != nil {
		return mapKubernetesError(err)
	}
	for _, claim := range claims.Items {
		id := claim.GetLabels()[AgentIDLabel]
		if id == "" {
			continue
		}
		if _, err := c.repository.GetAny(ctx, id); err != nil {
			if typed, ok := err.(*lifecycle.Error); ok && typed.Code == "agent_not_found" {
				policy := metav1.DeletePropagationForeground
				if deleteErr := c.client.Resource(SandboxClaimGVR).Namespace(c.namespace).Delete(ctx, claim.GetName(), metav1.DeleteOptions{PropagationPolicy: &policy}); deleteErr != nil && !apierrors.IsNotFound(deleteErr) {
					return mapKubernetesError(deleteErr)
				}
				c.logger.Warn("deleted orphan sandbox claim", "agent_id", id, "claim", claim.GetName())
			}
		}
	}
	return nil
}

func (c *Controller) markRetryExhausted(ctx context.Context, id string) {
	agent, err := c.repository.GetAny(ctx, id)
	if err != nil || agent.DesiredState == lifecycle.DesiredDeleted {
		return
	}
	now := time.Now().UTC()
	status := agent.Status
	status.State = lifecycle.StateFailed
	status.Reason = "reconcile_retries_exhausted"
	status.Message = "lifecycle reconciliation exceeded its retry limit"
	status.ObservedGeneration = agent.Generation
	status.LastTransitionAt = &now
	updated, updateErr := c.repository.SetStatus(ctx, id, status)
	if updateErr == nil && c.reconciler.events != nil {
		c.reconciler.events.Publish(lifecycle.Event{
			Type: "agent.state_changed", AgentID: id, PreviousState: agent.Status.State, State: lifecycle.StateFailed,
			OccurredAt: now, Owner: updated.Owner,
		})
	}
}
