package bootstrap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fakeGit(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "git")
	script := `#!/bin/sh
set -eu
if [ "${FAKE_GIT_FAILURE:-}" = auth ]; then
  printf '%s\n' 'fatal: Authentication failed' >&2
  exit 128
fi
if [ "${FAKE_GIT_FAILURE:-}" = full ]; then
  printf '%s\n' 'fatal: No space left on device' >&2
  exit 128
fi
case "$1" in
  init)
    mkdir -p "$3/.git"
    ;;
  -C)
    case "$3" in
      config)
        if [ "${4:-}" = remote.origin.url ]; then
          printf '%s\n' "$5" > "$2/.git/origin"
        else
          cat "$2/.git/origin"
        fi
        ;;
      checkout)
        printf '%s\n' 'bootstrapped' > "$2/README.md"
        ;;
      rev-parse)
        printf '%s\n' '0123456789abcdef0123456789abcdef01234567'
        ;;
    esac
    ;;
esac
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestExecuteRecoversCommitWhenFinalMetadataWriteWasInterrupted(t *testing.T) {
	workspace := t.TempDir()
	options := Options{
		Workspace: workspace, RepositoryURL: "https://github.com/example/project.git", Revision: "main",
		GitBinary: fakeGit(t), Timeout: time.Second,
	}
	if _, err := Execute(context.Background(), options); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(workspace, ".sandherd")
	if err := os.Remove(filepath.Join(state, "bootstrap.json")); err != nil {
		t.Fatal(err)
	}
	intent, _ := json.Marshal(bootstrapIntent{RepositoryURL: options.RepositoryURL, RequestedRevision: options.Revision})
	if err := os.WriteFile(filepath.Join(state, "bootstrap-intent.json"), intent, 0o600); err != nil {
		t.Fatal(err)
	}
	repositoryFile := filepath.Join(workspace, "repository", "README.md")
	if err := os.WriteFile(repositoryFile, []byte("preserve recovery changes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	metadata, err := Execute(context.Background(), options)
	if err != nil || metadata.ResolvedCommit == "" {
		t.Fatalf("recovery = %#v, %v", metadata, err)
	}
	contents, _ := os.ReadFile(repositoryFile)
	if string(contents) != "preserve recovery changes\n" {
		t.Fatalf("recovery replaced repository: %q", contents)
	}
}

func TestExecuteBootstrapsAtomicallyAndIsIdempotent(t *testing.T) {
	workspace := t.TempDir()
	now := time.Date(2026, 7, 30, 1, 2, 3, 0, time.UTC)
	options := Options{
		Workspace: workspace, RepositoryURL: "https://github.com/example/project.git", Revision: "main",
		GitBinary: fakeGit(t), Timeout: time.Second, Environment: os.Environ(), Now: func() time.Time { return now },
	}
	metadata, err := Execute(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if metadata.ResolvedCommit != "0123456789abcdef0123456789abcdef01234567" || metadata.BootstrappedAt != now {
		t.Fatalf("metadata = %#v", metadata)
	}
	repository := filepath.Join(workspace, "repository")
	if contents, err := os.ReadFile(filepath.Join(repository, "README.md")); err != nil || string(contents) != "bootstrapped\n" {
		t.Fatalf("repository contents = %q, %v", contents, err)
	}
	if err := os.WriteFile(filepath.Join(repository, "README.md"), []byte("agent changes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := Execute(context.Background(), options)
	if err != nil || second.ResolvedCommit != metadata.ResolvedCommit {
		t.Fatalf("second bootstrap = %#v, %v", second, err)
	}
	contents, _ := os.ReadFile(filepath.Join(repository, "README.md"))
	if string(contents) != "agent changes\n" {
		t.Fatalf("idempotent bootstrap replaced agent changes: %q", contents)
	}
	stateContents, _ := os.ReadFile(filepath.Join(workspace, ".sandherd", "bootstrap.json"))
	if strings.Contains(string(stateContents), "credential") || strings.Contains(string(stateContents), "password") {
		t.Fatalf("metadata contains credential material: %s", stateContents)
	}
}

func TestExecuteEmptyWorkspaceIsWritableAndIdempotent(t *testing.T) {
	workspace := t.TempDir()
	options := Options{Workspace: workspace}
	metadata, err := Execute(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if metadata.RepositoryURL != "" || metadata.RequestedRevision != "HEAD" {
		t.Fatalf("empty workspace metadata = %#v", metadata)
	}
	repositoryFile := filepath.Join(workspace, "repository", "agent-change")
	if err := os.WriteFile(repositoryFile, []byte("preserve\n"), 0o600); err != nil {
		t.Fatalf("write empty workspace: %v", err)
	}
	if _, err := Execute(context.Background(), options); err != nil {
		t.Fatalf("repeat empty workspace bootstrap: %v", err)
	}
	contents, err := os.ReadFile(repositoryFile)
	if err != nil || string(contents) != "preserve\n" {
		t.Fatalf("preserved agent change = %q, %v", contents, err)
	}
}

func TestAdapterStateIsSeparatedAndPreserved(t *testing.T) {
	workspace := t.TempDir()
	if _, err := Execute(context.Background(), Options{Workspace: workspace, AdapterID: "shell"}); err != nil {
		t.Fatal(err)
	}
	shellState := filepath.Join(workspace, ".sandherd", "adapters", "shell")
	if err := os.WriteFile(filepath.Join(shellState, "session"), []byte("preserved"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Execute(context.Background(), Options{Workspace: workspace, AdapterID: "shell_minimal"}); err != nil {
		t.Fatal(err)
	}
	if contents, err := os.ReadFile(filepath.Join(shellState, "session")); err != nil || string(contents) != "preserved" {
		t.Fatalf("original adapter state contents=%q error=%v", contents, err)
	}
	if info, err := os.Stat(filepath.Join(workspace, ".sandherd", "adapters", "shell_minimal")); err != nil || !info.IsDir() {
		t.Fatalf("replacement adapter state was not created: info=%v error=%v", info, err)
	}
}

func TestAdapterStateRejectsUnsafeID(t *testing.T) {
	if _, err := Execute(context.Background(), Options{Workspace: t.TempDir(), AdapterID: "../escape"}); err == nil {
		t.Fatal("unsafe adapter ID was accepted")
	}
}

func TestExecuteRecoversInterruptedStagingWithoutFollowingSymlink(t *testing.T) {
	workspace := t.TempDir()
	state := filepath.Join(workspace, ".sandherd")
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	protected := filepath.Join(outside, "protected")
	if err := os.WriteFile(protected, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(state, "bootstrap-staging")); err != nil {
		t.Fatal(err)
	}
	_, err := Execute(context.Background(), Options{
		Workspace: workspace, RepositoryURL: "https://github.com/example/project.git", Revision: "main",
		GitBinary: fakeGit(t), Timeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if contents, err := os.ReadFile(protected); err != nil || string(contents) != "keep" {
		t.Fatalf("bootstrap followed staging symlink: %q, %v", contents, err)
	}
}

func TestExecuteRejectsUnsafePathsAndInput(t *testing.T) {
	tests := []struct {
		name     string
		prepare  func(t *testing.T, workspace string)
		url      string
		revision string
		code     string
	}{
		{name: "credentials in URL", url: "https://user:secret@example.com/repo.git", revision: "main", code: "bootstrap_invalid"},
		{name: "revision option injection", url: "https://example.com/repo.git", revision: "--upload-pack=evil", code: "bootstrap_invalid"},
		{name: "workspace state symlink", url: "https://example.com/repo.git", revision: "main", code: "workspace_unsafe", prepare: func(t *testing.T, workspace string) {
			if err := os.Symlink(t.TempDir(), filepath.Join(workspace, ".sandherd")); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			workspace := t.TempDir()
			if test.prepare != nil {
				test.prepare(t, workspace)
			}
			_, err := Execute(context.Background(), Options{Workspace: workspace, RepositoryURL: test.url, Revision: test.revision, GitBinary: fakeGit(t), Timeout: time.Second})
			var typed *Error
			if !errors.As(err, &typed) || typed.Code != test.code {
				t.Fatalf("error = %v, want code %s", err, test.code)
			}
		})
	}
}

func TestEmptyBootstrapRefusesUntrackedWorkspaceContent(t *testing.T) {
	workspace := t.TempDir()
	repository := filepath.Join(workspace, "repository")
	if err := os.Mkdir(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "unknown"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Execute(context.Background(), Options{Workspace: workspace})
	var typed *Error
	if !errors.As(err, &typed) || typed.Code != "workspace_not_empty" {
		t.Fatalf("untracked workspace error = %#v", err)
	}
}

func TestExecuteRejectsWorkspacePathThroughParentSymlink(t *testing.T) {
	targetParent := t.TempDir()
	target := filepath.Join(targetParent, "workspace")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	linkParent := filepath.Join(t.TempDir(), "linked-parent")
	if err := os.Symlink(targetParent, linkParent); err != nil {
		t.Fatal(err)
	}
	_, err := Execute(context.Background(), Options{Workspace: filepath.Join(linkParent, "workspace")})
	var typed *Error
	if !errors.As(err, &typed) || typed.Code != "workspace_unsafe" {
		t.Fatalf("parent symlink error = %#v", err)
	}
}

func TestExecuteClassifiesAuthAndFullDiskWithoutLeakingGitOutput(t *testing.T) {
	tests := []struct {
		failure string
		code    string
		exit    int
	}{
		{failure: "auth", code: "repository_auth_failed", exit: ExitRepositoryAuth},
		{failure: "full", code: "workspace_full", exit: ExitWorkspaceFull},
	}
	for _, test := range tests {
		t.Run(test.failure, func(t *testing.T) {
			_, err := Execute(context.Background(), Options{
				Workspace: t.TempDir(), RepositoryURL: "https://example.com/repo.git", Revision: "main",
				GitBinary: fakeGit(t), Timeout: time.Second, Environment: append(os.Environ(), "FAKE_GIT_FAILURE="+test.failure),
			})
			var typed *Error
			if !errors.As(err, &typed) || typed.Code != test.code || typed.ExitCode != test.exit {
				t.Fatalf("error = %#v", err)
			}
			if strings.Contains(err.Error(), "fatal:") {
				t.Fatalf("git output leaked through stable error: %v", err)
			}
		})
	}
}

func TestExecuteClassifiesTimeout(t *testing.T) {
	git := filepath.Join(t.TempDir(), "git")
	if err := os.WriteFile(git, []byte("#!/bin/sh\nexec sleep 5\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := Execute(context.Background(), Options{
		Workspace: t.TempDir(), RepositoryURL: "https://example.com/repo.git", Revision: "main",
		GitBinary: git, Timeout: 10 * time.Millisecond,
	})
	var typed *Error
	if !errors.As(err, &typed) || typed.Code != "bootstrap_timeout" || typed.ExitCode != ExitBootstrapTimeout {
		t.Fatalf("timeout error = %#v", err)
	}
}

func TestAskPassReadsJSONCredential(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credential.json")
	contents, _ := json.Marshal(credential{Username: "git-user", Password: "super-secret"})
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := AskPass(path, "Username for https://example.com:", &output); err != nil || output.String() != "git-user\n" {
		t.Fatalf("username = %q, %v", output.String(), err)
	}
	output.Reset()
	if err := AskPass(path, "Password for https://example.com:", &output); err != nil || output.String() != "super-secret\n" {
		t.Fatalf("password = %q, %v", output.String(), err)
	}
}

func TestSSHWrapperForcesPinnedHostAndIdentity(t *testing.T) {
	ssh := filepath.Join(t.TempDir(), "ssh")
	if err := os.WriteFile(ssh, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SANDHERD_SSH_BINARY", ssh)
	t.Setenv("SANDHERD_GIT_SSH_KEY_FILE", "/secrets/identity")
	t.Setenv("SANDHERD_GIT_KNOWN_HOSTS_FILE", "/secrets/known_hosts")
	var output bytes.Buffer
	if err := RunSSHWrapper(context.Background(), []string{"git@example.com", "git-upload-pack repo"}, strings.NewReader(""), &output, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	arguments := output.String()
	for _, expected := range []string{"BatchMode=yes", "IdentitiesOnly=yes", "StrictHostKeyChecking=yes", "UserKnownHostsFile=/secrets/known_hosts", "/secrets/identity", "git@example.com"} {
		if !strings.Contains(arguments, expected) {
			t.Fatalf("SSH arguments missing %q: %s", expected, arguments)
		}
	}
}

func TestRunReportsOnlyStableErrorCode(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"--workspace", t.TempDir(), "--repository-url", "https://example.com/repo.git", "--revision", "main",
		"--git", fakeGit(t), "--timeout", "1s",
	}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 || !strings.Contains(stdout.String(), "workspace ready") || stderr.Len() != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}
