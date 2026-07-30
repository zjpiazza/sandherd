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

func TestCredentialProfileAndAdapterChangeValidation(t *testing.T) {
	request := CreateRequest{Name: "agent", Spec: AgentSpec{
		Kind: "shell", SandboxProfile: "standard", CredentialProfile: "personal",
		Resources: ResourceSpec{CPU: "1", Memory: "1Gi"},
		Workspace: WorkspaceSpec{Size: "10Gi", StorageProfile: "default", RetentionPolicy: "retain"},
	}}
	if err := request.Validate(); err != nil {
		t.Fatalf("credential profile rejected: %v", err)
	}
	change := ChangeAdapterRequest{Kind: "shell_minimal", CredentialProfile: "personal"}
	if err := change.Validate(); err != nil {
		t.Fatalf("adapter change rejected: %v", err)
	}
	change.CredentialProfile = "../secret"
	if err := change.Validate(); err == nil {
		t.Fatal("unsafe credential profile was accepted")
	}
}

func TestRepositoryValidationRejectsCredentialAndOptionInjection(t *testing.T) {
	request := CreateRequest{Name: "agent", Spec: AgentSpec{
		Kind: "codex", SandboxProfile: "standard",
		Repository: &RepositorySpec{URL: "https://github.com/example/repo.git", Revision: "main"},
		Resources:  ResourceSpec{CPU: "1", Memory: "1Gi"},
		Workspace:  WorkspaceSpec{Size: "10Gi", StorageProfile: "default", RetentionPolicy: "retain"},
	}}
	for _, invalid := range []RepositorySpec{
		{URL: "https://user:secret@github.com/example/repo.git", Revision: "main"},
		{URL: "ssh://git:secret@github.com/example/repo.git", Revision: "main"},
		{URL: "https://github.com/example/repo.git?token=secret", Revision: "main"},
		{URL: "https://github.com/example/repo.git", Revision: "--upload-pack=evil"},
		{URL: "https://github.com/example/repo.git", Revision: "main\nother"},
	} {
		request.Spec.Repository = &invalid
		if err := request.Validate(); err == nil {
			t.Fatalf("repository %#v was accepted", invalid)
		}
	}
	request.Spec.Repository = &RepositorySpec{URL: "ssh://git@github.com/example/repo.git", Revision: "refs/heads/main"}
	if err := request.Validate(); err != nil {
		t.Fatalf("valid SSH repository rejected: %v", err)
	}
	request.Spec.Repository = nil
	request.Spec.SecretProfile = "private"
	if err := request.Validate(); err == nil || !strings.Contains(err.Error(), "requires") {
		t.Fatalf("secret profile without repository error = %v", err)
	}
}

func TestStateTransitionGuards(t *testing.T) {
	if !CanStop(StateRunning) || !CanStop(StateStopped) || CanStop(StateFailed) {
		t.Fatal("stop transition guard is incorrect")
	}
	if !CanResume(StateStopped) || !CanResume(StateRunning) || CanResume(StateFailed) {
		t.Fatal("resume transition guard is incorrect")
	}
	if !CanChangeAdapter(StateRunning) || !CanChangeAdapter(StateStopped) || !CanChangeAdapter(StateFailed) || CanChangeAdapter(StateReconfiguring) || CanChangeAdapter(StateDeleting) {
		t.Fatal("adapter-change transition guard is incorrect")
	}
}
