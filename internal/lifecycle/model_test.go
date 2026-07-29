package lifecycle

import (
	"strings"
	"testing"
	"time"
)

func TestNewIDIsUUIDv7AndTimeOrdered(t *testing.T) {
	first := NewID()
	time.Sleep(time.Millisecond)
	second := NewID()
	if len(first) != 36 || first[14] != '7' || first[19] < '8' || first[19] > 'b' {
		t.Fatalf("NewID() = %q, want UUIDv7", first)
	}
	if first >= second {
		t.Fatalf("IDs are not time ordered: %s >= %s", first, second)
	}
}

func TestCreateRequestValidationAndDefaults(t *testing.T) {
	request := CreateRequest{Name: "agent", Spec: AgentSpec{
		Kind: "codex", SandboxProfile: "standard",
		Repository: &RepositorySpec{URL: "https://github.com/example/repo.git"},
		Resources:  ResourceSpec{CPU: "1", Memory: "1Gi"},
		Workspace:  WorkspaceSpec{Size: "10Gi", RetentionPolicy: "retain"},
		Lifecycle:  LifecycleSpec{},
	}}
	request.ApplyDefaults()
	if err := request.Validate(); err != nil {
		t.Fatalf("valid request: %v", err)
	}
	if request.Spec.Repository.Revision != "HEAD" || request.Spec.Workspace.StorageProfile != "default" {
		t.Fatalf("defaults = %#v", request.Spec)
	}
	request.Spec.Repository.URL = "file:///etc/passwd"
	if err := request.Validate(); err == nil || !strings.Contains(err.Error(), "HTTPS or SSH") {
		t.Fatalf("invalid repository error = %v", err)
	}
}

func TestStateTransitionGuards(t *testing.T) {
	if !CanStop(StateRunning) || !CanStop(StateStopped) || CanStop(StateFailed) {
		t.Fatal("stop transition guard is incorrect")
	}
	if !CanResume(StateStopped) || !CanResume(StateRunning) || CanResume(StateFailed) {
		t.Fatal("resume transition guard is incorrect")
	}
}
