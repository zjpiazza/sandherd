package kubernetes

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/zjpiazza/sandherd/internal/lifecycle"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestControllerInitialResyncRecoversAgents(t *testing.T) {
	client := fakeClient()
	repository := NewRepository(client, testNamespace)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	reconciler := NewReconciler(client, repository, testNamespace, map[string]string{"standard": "pool"}, lifecycle.NewEventBus(8), logger)
	controller := NewController(client, repository, reconciler, testNamespace, logger)
	agent, _, err := repository.Create(context.Background(), "owner", "key", validCreateRequest("alpha"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- controller.Run(ctx) }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		_, err = client.Resource(SandboxClaimGVR).Namespace(testNamespace).Get(context.Background(), claimName(agent.ID), metav1.GetOptions{})
		if err == nil {
			break
		}
		if !apierrors.IsNotFound(err) || time.Now().After(deadline) {
			cancel()
			t.Fatalf("controller did not recover agent: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("controller did not stop")
	}
}

func TestControllerDeletesManagedOrphanClaim(t *testing.T) {
	ctx := context.Background()
	client := fakeClient()
	repository := NewRepository(client, testNamespace)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	reconciler := NewReconciler(client, repository, testNamespace, map[string]string{"standard": "pool"}, nil, logger)
	controller := NewController(client, repository, reconciler, testNamespace, logger)
	orphanID := lifecycle.NewID()
	claim := sandboxClaim(lifecycle.Agent{ID: orphanID, Owner: "owner", Spec: validCreateRequest("orphan").Spec}, testNamespace, "pool")
	if _, err := client.Resource(SandboxClaimGVR).Namespace(testNamespace).Create(ctx, claim, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := controller.deleteOrphanClaims(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Resource(SandboxClaimGVR).Namespace(testNamespace).Get(ctx, claim.GetName(), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("orphan claim still exists: %v", err)
	}
}
