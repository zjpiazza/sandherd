// Package bootstrap prepares a durable agent workspace without executing code
// from the requested repository.
package bootstrap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	ExitInvalidConfiguration = 20
	ExitUnsafeWorkspace      = 21
	ExitRepositoryAuth       = 22
	ExitRepositoryBootstrap  = 23
	ExitWorkspaceFull        = 24
	ExitBootstrapTimeout     = 25
)

type Error struct {
	Code     string
	ExitCode int
	Message  string
	Err      error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return e.Message + ": " + e.Err.Error()
	}
	return e.Message
}

func (e *Error) Unwrap() error { return e.Err }

type Options struct {
	Workspace      string
	RepositoryURL  string
	Revision       string
	CredentialFile string
	SSHKeyFile     string
	KnownHostsFile string
	GitBinary      string
	SSHBinary      string
	HelperBinary   string
	Timeout        time.Duration
	Environment    []string
	Now            func() time.Time
}

type Metadata struct {
	RepositoryURL     string    `json:"repositoryUrl,omitempty"`
	RequestedRevision string    `json:"requestedRevision,omitempty"`
	ResolvedCommit    string    `json:"resolvedCommit,omitempty"`
	BootstrappedAt    time.Time `json:"bootstrappedAt"`
}

type bootstrapIntent struct {
	RepositoryURL     string `json:"repositoryUrl"`
	RequestedRevision string `json:"requestedRevision"`
}

type credential struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func Execute(ctx context.Context, options Options) (Metadata, error) {
	options = applyDefaults(options)
	if err := validateOptions(options); err != nil {
		return Metadata{}, err
	}
	paths, err := preparePaths(options.Workspace)
	if err != nil {
		return Metadata{}, err
	}
	lock, err := os.OpenFile(paths.lock, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return Metadata{}, workspaceError("workspace_lock_failed", "open the workspace bootstrap lock", err)
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		return Metadata{}, workspaceError("workspace_lock_failed", "lock the workspace bootstrap state", err)
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)

	if existing, found, err := readMetadata(paths.metadata); err != nil {
		return Metadata{}, err
	} else if found {
		existingRevision := existing.RequestedRevision
		if existing.RepositoryURL == "" && existingRevision == "" {
			existingRevision = "HEAD"
		}
		if existing.RepositoryURL != options.RepositoryURL || existingRevision != options.Revision {
			return Metadata{}, workspaceError("workspace_identity_mismatch", "the existing workspace belongs to a different repository or revision", nil)
		}
		if err := requireDirectory(paths.repository); err != nil {
			return Metadata{}, err
		}
		return existing, nil
	}

	if options.RepositoryURL == "" {
		if err := ensureEmptyRepositoryDirectory(paths.repository); err != nil {
			return Metadata{}, err
		}
		metadata := Metadata{RequestedRevision: options.Revision, BootstrappedAt: options.Now().UTC()}
		if err := writeMetadata(paths.metadata, metadata); err != nil {
			return Metadata{}, err
		}
		return metadata, nil
	}

	bootstrapContext, cancel := context.WithTimeout(ctx, options.Timeout)
	defer cancel()
	environment, err := gitEnvironment(options, paths)
	if err != nil {
		return Metadata{}, err
	}
	intent, foundIntent, err := readIntent(paths.intent)
	if err != nil {
		return Metadata{}, err
	}
	if foundIntent && (intent.RepositoryURL != options.RepositoryURL || intent.RequestedRevision != options.Revision) {
		return Metadata{}, workspaceError("workspace_identity_mismatch", "an interrupted bootstrap belongs to a different repository or revision", nil)
	}
	if foundIntent {
		if recovered, found, recoverErr := recoverCommittedRepository(bootstrapContext, options, paths, environment); recoverErr != nil {
			return Metadata{}, recoverErr
		} else if found {
			return recovered, nil
		}
	} else if err := writeIntent(paths.intent, bootstrapIntent{RepositoryURL: options.RepositoryURL, RequestedRevision: options.Revision}); err != nil {
		return Metadata{}, err
	}
	if err := requireRepositoryDestinationAvailable(paths.repository); err != nil {
		return Metadata{}, err
	}
	if err := resetStaging(paths.staging); err != nil {
		return Metadata{}, err
	}
	defer os.RemoveAll(paths.staging)

	commands := [][]string{
		{"init", "--quiet", paths.staging},
		{"-C", paths.staging, "config", "remote.origin.url", options.RepositoryURL},
		{"-C", paths.staging, "fetch", "--quiet", "--depth=1", "--", "origin", options.Revision},
		{"-C", paths.staging, "checkout", "--quiet", "--detach", "FETCH_HEAD"},
	}
	for _, arguments := range commands {
		if err := runGit(bootstrapContext, options.GitBinary, environment, arguments...); err != nil {
			return Metadata{}, classifyGitError(bootstrapContext, err)
		}
	}
	commit, err := outputGit(bootstrapContext, options.GitBinary, environment, "-C", paths.staging, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return Metadata{}, classifyGitError(bootstrapContext, err)
	}
	commit = strings.TrimSpace(commit)
	if !validCommit(commit) {
		return Metadata{}, repositoryError("repository_commit_invalid", "git returned an invalid resolved commit", nil)
	}
	if err := os.Rename(paths.staging, paths.repository); err != nil {
		return Metadata{}, workspaceError("workspace_commit_failed", "commit the bootstrapped repository to the workspace", err)
	}
	metadata := Metadata{
		RepositoryURL: options.RepositoryURL, RequestedRevision: options.Revision,
		ResolvedCommit: commit, BootstrappedAt: options.Now().UTC(),
	}
	if err := writeMetadata(paths.metadata, metadata); err != nil {
		return Metadata{}, err
	}
	if err := os.Remove(paths.intent); err != nil && !os.IsNotExist(err) {
		return Metadata{}, workspaceError("workspace_metadata_failed", "remove completed workspace bootstrap intent", err)
	}
	return metadata, nil
}

type workspacePaths struct {
	root, state, repository, staging, metadata, intent, lock, home string
}

func applyDefaults(options Options) Options {
	if options.Revision == "" {
		options.Revision = "HEAD"
	}
	if options.GitBinary == "" {
		options.GitBinary = "git"
	}
	if options.SSHBinary == "" {
		options.SSHBinary = "ssh"
	}
	if options.Timeout == 0 {
		options.Timeout = 5 * time.Minute
	}
	if options.Environment == nil {
		options.Environment = os.Environ()
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	return options
}

func validateOptions(options Options) error {
	if !filepath.IsAbs(options.Workspace) || filepath.Clean(options.Workspace) == string(filepath.Separator) {
		return invalidError("workspace must be an absolute path other than the filesystem root")
	}
	if options.Timeout <= 0 {
		return invalidError("bootstrap timeout must be positive")
	}
	if options.RepositoryURL == "" {
		if options.CredentialFile != "" || options.SSHKeyFile != "" || options.KnownHostsFile != "" {
			return invalidError("repository credentials require a repository URL")
		}
		return nil
	}
	parsed, err := url.Parse(options.RepositoryURL)
	if err != nil || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Scheme != "https" && parsed.Scheme != "ssh") {
		return invalidError("repository URL must be an HTTPS or SSH URL without a query or fragment")
	}
	if parsed.Scheme == "https" && parsed.User != nil {
		return invalidError("HTTPS repository credentials must not be embedded in the URL")
	}
	if parsed.Scheme == "ssh" && parsed.User != nil {
		if _, hasPassword := parsed.User.Password(); hasPassword {
			return invalidError("SSH repository credentials must not be embedded in the URL")
		}
	}
	if strings.HasPrefix(options.Revision, "-") || len(options.Revision) > 256 || strings.IndexFunc(options.Revision, func(r rune) bool { return r < 0x21 || r == 0x7f }) >= 0 {
		return invalidError("repository revision is invalid")
	}
	if parsed.Scheme == "https" && (options.SSHKeyFile != "" || options.KnownHostsFile != "") {
		return invalidError("SSH credentials require an SSH repository URL")
	}
	if parsed.Scheme == "ssh" {
		if options.CredentialFile != "" {
			return invalidError("HTTPS credentials cannot be used with an SSH repository")
		}
		if (options.SSHKeyFile == "") != (options.KnownHostsFile == "") {
			return invalidError("SSH key and known-hosts files must be configured together")
		}
	}
	return nil
}

func preparePaths(workspace string) (workspacePaths, error) {
	root := filepath.Clean(workspace)
	info, err := os.Lstat(root)
	if err != nil {
		return workspacePaths{}, workspaceError("workspace_unavailable", "inspect the workspace mount", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return workspacePaths{}, workspaceError("workspace_unsafe", "workspace mount must be a real directory", nil)
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil || resolved != root {
		return workspacePaths{}, workspaceError("workspace_unsafe", "workspace mount path must not traverse symlinks", err)
	}
	state := filepath.Join(root, ".sandherd")
	if err := ensureDirectory(state, 0o700); err != nil {
		return workspacePaths{}, err
	}
	home := filepath.Join(state, "home")
	if err := ensureDirectory(home, 0o700); err != nil {
		return workspacePaths{}, err
	}
	return workspacePaths{
		root: root, state: state, repository: filepath.Join(root, "repository"),
		staging: filepath.Join(state, "bootstrap-staging"), metadata: filepath.Join(state, "bootstrap.json"),
		intent: filepath.Join(state, "bootstrap-intent.json"), lock: filepath.Join(state, "bootstrap.lock"), home: home,
	}, nil
}

func recoverCommittedRepository(ctx context.Context, options Options, paths workspacePaths, environment []string) (Metadata, bool, error) {
	info, err := os.Lstat(paths.repository)
	if os.IsNotExist(err) {
		return Metadata{}, false, nil
	}
	if err != nil {
		return Metadata{}, false, workspaceError("workspace_unavailable", "inspect an interrupted repository bootstrap", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return Metadata{}, false, workspaceError("workspace_unsafe", "the repository path must be a real directory", nil)
	}
	origin, err := outputGit(ctx, options.GitBinary, environment, "-C", paths.repository, "config", "--get", "remote.origin.url")
	if err != nil || strings.TrimSpace(origin) != options.RepositoryURL {
		return Metadata{}, false, workspaceError("workspace_identity_mismatch", "the interrupted repository does not match its bootstrap intent", nil)
	}
	commit, err := outputGit(ctx, options.GitBinary, environment, "-C", paths.repository, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return Metadata{}, false, repositoryError("repository_bootstrap_failed", "verify an interrupted repository bootstrap", err)
	}
	metadata := Metadata{
		RepositoryURL: options.RepositoryURL, RequestedRevision: options.Revision,
		ResolvedCommit: strings.TrimSpace(commit), BootstrappedAt: options.Now().UTC(),
	}
	if !validCommit(metadata.ResolvedCommit) {
		return Metadata{}, false, repositoryError("repository_commit_invalid", "git returned an invalid resolved commit", nil)
	}
	if err := writeMetadata(paths.metadata, metadata); err != nil {
		return Metadata{}, false, err
	}
	if err := os.Remove(paths.intent); err != nil && !os.IsNotExist(err) {
		return Metadata{}, false, workspaceError("workspace_metadata_failed", "remove completed workspace bootstrap intent", err)
	}
	return metadata, true, nil
}

func ensureDirectory(path string, mode os.FileMode) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		if err := os.Mkdir(path, mode); err != nil {
			return workspaceError("workspace_initialize_failed", "create Sandherd workspace state", err)
		}
		return nil
	}
	if err != nil {
		return workspaceError("workspace_unavailable", "inspect Sandherd workspace state", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return workspaceError("workspace_unsafe", "Sandherd workspace state must be a real directory", nil)
	}
	return nil
}

func requireDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return workspaceError("workspace_incomplete", "the bootstrapped repository directory is missing", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return workspaceError("workspace_unsafe", "the repository path must be a real directory", nil)
	}
	return nil
}

func ensureEmptyRepositoryDirectory(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		if err := os.Mkdir(path, 0o750); err != nil {
			return workspaceError("workspace_initialize_failed", "create the empty repository directory", err)
		}
		return nil
	}
	if err != nil {
		return workspaceError("workspace_unavailable", "inspect the repository directory", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return workspaceError("workspace_unsafe", "the repository path must be a real directory", nil)
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return workspaceError("workspace_unavailable", "inspect the empty repository directory", err)
	}
	if len(entries) != 0 {
		return workspaceError("workspace_not_empty", "refusing to adopt an untracked repository directory", nil)
	}
	return nil
}

func validCommit(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, character := range value {
		if !strings.ContainsRune("0123456789abcdefABCDEF", character) {
			return false
		}
	}
	return true
}

func requireRepositoryDestinationAvailable(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return workspaceError("workspace_unavailable", "inspect the repository destination", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return workspaceError("workspace_unsafe", "the repository destination must be a real directory", nil)
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return workspaceError("workspace_unavailable", "inspect the repository destination", err)
	}
	if len(entries) != 0 {
		return workspaceError("workspace_not_empty", "refusing to replace an untracked repository directory", nil)
	}
	if err := os.Remove(path); err != nil {
		return workspaceError("workspace_initialize_failed", "prepare the repository destination", err)
	}
	return nil
}

func resetStaging(path string) error {
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			if err := os.Remove(path); err != nil {
				return workspaceError("workspace_unsafe", "remove an unsafe bootstrap staging link", err)
			}
		} else if err := os.RemoveAll(path); err != nil {
			return workspaceError("workspace_initialize_failed", "clear an interrupted bootstrap attempt", err)
		}
	} else if !os.IsNotExist(err) {
		return workspaceError("workspace_unavailable", "inspect bootstrap staging", err)
	}
	return nil
}

func gitEnvironment(options Options, paths workspacePaths) ([]string, error) {
	environment := make([]string, 0, len(options.Environment)+12)
	for _, value := range options.Environment {
		name := strings.SplitN(value, "=", 2)[0]
		if strings.HasPrefix(name, "GIT_") || name == "HOME" || name == "XDG_CONFIG_HOME" || name == "SSH_AUTH_SOCK" {
			continue
		}
		environment = append(environment, value)
	}
	environment = append(environment,
		"HOME="+paths.home,
		"XDG_CONFIG_HOME="+paths.home,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=core.hooksPath",
		"GIT_CONFIG_VALUE_0=/dev/null",
	)
	if options.CredentialFile != "" {
		if _, err := readCredential(options.CredentialFile); err != nil {
			return nil, err
		}
		helper := options.HelperBinary
		if helper == "" {
			var err error
			helper, err = os.Executable()
			if err != nil {
				return nil, invalidError("resolve the bootstrap credential helper executable")
			}
		}
		environment = append(environment,
			"GIT_ASKPASS="+helper,
			"SANDHERD_ASKPASS=1",
			"SANDHERD_GIT_CREDENTIAL_FILE="+options.CredentialFile,
		)
	}
	if options.SSHKeyFile != "" {
		for _, path := range []string{options.SSHKeyFile, options.KnownHostsFile} {
			if info, err := os.Stat(filepath.Clean(path)); err != nil || !info.Mode().IsRegular() {
				return nil, invalidError("SSH credential files must be readable regular files")
			}
		}
		helper := options.HelperBinary
		if helper == "" {
			var err error
			helper, err = os.Executable()
			if err != nil {
				return nil, invalidError("resolve the bootstrap SSH helper executable")
			}
		}
		environment = append(environment,
			"GIT_SSH="+helper,
			"SANDHERD_SSH_WRAPPER=1",
			"SANDHERD_GIT_SSH_KEY_FILE="+options.SSHKeyFile,
			"SANDHERD_GIT_KNOWN_HOSTS_FILE="+options.KnownHostsFile,
			"SANDHERD_SSH_BINARY="+options.SSHBinary,
		)
	}
	return environment, nil
}

func runGit(ctx context.Context, binary string, environment []string, arguments ...string) error {
	_, err := gitCommand(ctx, binary, environment, arguments...)
	return err
}

func outputGit(ctx context.Context, binary string, environment []string, arguments ...string) (string, error) {
	output, err := gitCommand(ctx, binary, environment, arguments...)
	return string(output), err
}

func gitCommand(ctx context.Context, binary string, environment []string, arguments ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, binary, arguments...)
	command.Env = environment
	var output limitedBuffer
	command.Stdout = &output
	command.Stderr = &output
	err := command.Run()
	if err != nil {
		return output.Bytes(), &gitError{err: err, output: output.String()}
	}
	return output.Bytes(), nil
}

type gitError struct {
	err    error
	output string
}

func (e *gitError) Error() string { return e.err.Error() }
func (e *gitError) Unwrap() error { return e.err }

func classifyGitError(ctx context.Context, err error) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return &Error{Code: "bootstrap_timeout", ExitCode: ExitBootstrapTimeout, Message: "repository bootstrap timed out"}
	}
	var detail *gitError
	text := ""
	if errors.As(err, &detail) {
		text = strings.ToLower(detail.output)
	}
	if strings.Contains(text, "no space left on device") || errors.Is(err, syscall.ENOSPC) {
		return &Error{Code: "workspace_full", ExitCode: ExitWorkspaceFull, Message: "the workspace is full"}
	}
	for _, marker := range []string{"authentication failed", "permission denied (publickey)", "could not read username", "http 401", "http 403"} {
		if strings.Contains(text, marker) {
			return &Error{Code: "repository_auth_failed", ExitCode: ExitRepositoryAuth, Message: "repository authentication failed"}
		}
	}
	return repositoryError("repository_bootstrap_failed", "repository bootstrap failed", err)
}

func readMetadata(path string) (Metadata, bool, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return Metadata{}, false, nil
	}
	if err != nil {
		return Metadata{}, false, workspaceError("workspace_unavailable", "inspect workspace metadata", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return Metadata{}, false, workspaceError("workspace_unsafe", "workspace metadata must be a regular file", nil)
	}
	contents, err := readSmallFile(path)
	if err != nil {
		return Metadata{}, false, workspaceError("workspace_unavailable", "read workspace metadata", err)
	}
	var metadata Metadata
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&metadata); err != nil || metadata.BootstrappedAt.IsZero() {
		return Metadata{}, false, workspaceError("workspace_metadata_invalid", "workspace metadata is invalid", err)
	}
	return metadata, true, nil
}

func readIntent(path string) (bootstrapIntent, bool, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return bootstrapIntent{}, false, nil
	}
	if err != nil {
		return bootstrapIntent{}, false, workspaceError("workspace_unavailable", "inspect workspace bootstrap intent", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return bootstrapIntent{}, false, workspaceError("workspace_unsafe", "workspace bootstrap intent must be a regular file", nil)
	}
	contents, err := readSmallFile(path)
	if err != nil {
		return bootstrapIntent{}, false, workspaceError("workspace_unavailable", "read workspace bootstrap intent", err)
	}
	var intent bootstrapIntent
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&intent); err != nil || intent.RepositoryURL == "" || intent.RequestedRevision == "" {
		return bootstrapIntent{}, false, workspaceError("workspace_metadata_invalid", "workspace bootstrap intent is invalid", err)
	}
	return intent, true, nil
}

func writeMetadata(path string, metadata Metadata) error {
	return writeJSONFile(path, metadata)
}

func writeIntent(path string, intent bootstrapIntent) error {
	return writeJSONFile(path, intent)
}

func writeJSONFile(path string, value any) error {
	contents, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return workspaceError("workspace_metadata_failed", "encode workspace metadata", err)
	}
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".bootstrap-*.tmp")
	if err != nil {
		return workspaceError("workspace_metadata_failed", "create workspace metadata", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return workspaceError("workspace_metadata_failed", "secure workspace metadata", err)
	}
	if _, err := temporary.Write(append(contents, '\n')); err != nil {
		_ = temporary.Close()
		return workspaceError("workspace_metadata_failed", "write workspace metadata", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return workspaceError("workspace_metadata_failed", "sync workspace metadata", err)
	}
	if err := temporary.Close(); err != nil {
		return workspaceError("workspace_metadata_failed", "close workspace metadata", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return workspaceError("workspace_metadata_failed", "commit workspace metadata", err)
	}
	return nil
}

func readCredential(path string) (credential, error) {
	contents, err := readSmallFile(filepath.Clean(path))
	if err != nil {
		return credential{}, invalidError("read the HTTPS credential file")
	}
	var result credential
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil || result.Username == "" || result.Password == "" {
		return credential{}, invalidError("HTTPS credential file must contain non-empty username and password fields")
	}
	return result, nil
}

func readSmallFile(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	const maximum = 64 * 1024
	contents, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil {
		return nil, err
	}
	if len(contents) > maximum {
		return nil, fmt.Errorf("file exceeds %d bytes", maximum)
	}
	return contents, nil
}

func AskPass(path, prompt string, output io.Writer) error {
	value, err := readCredential(path)
	if err != nil {
		return err
	}
	if strings.Contains(strings.ToLower(prompt), "username") {
		_, err = fmt.Fprintln(output, value.Username)
	} else {
		_, err = fmt.Fprintln(output, value.Password)
	}
	return err
}

func RunSSHWrapper(ctx context.Context, arguments []string, stdin io.Reader, stdout, stderr io.Writer) error {
	key := os.Getenv("SANDHERD_GIT_SSH_KEY_FILE")
	knownHosts := os.Getenv("SANDHERD_GIT_KNOWN_HOSTS_FILE")
	binary := os.Getenv("SANDHERD_SSH_BINARY")
	if binary == "" {
		binary = "ssh"
	}
	if key == "" || knownHosts == "" {
		return invalidError("SSH wrapper requires key and known-hosts files")
	}
	wrapped := []string{"-o", "BatchMode=yes", "-o", "IdentitiesOnly=yes", "-o", "StrictHostKeyChecking=yes", "-o", "UserKnownHostsFile=" + knownHosts, "-i", key}
	wrapped = append(wrapped, arguments...)
	command := exec.CommandContext(ctx, binary, wrapped...)
	command.Stdin, command.Stdout, command.Stderr = stdin, stdout, stderr
	return command.Run()
}

type limitedBuffer struct{ bytes.Buffer }

func (b *limitedBuffer) Write(value []byte) (int, error) {
	const limit = 64 * 1024
	written := len(value)
	if b.Len() < limit {
		remaining := limit - b.Len()
		if len(value) > remaining {
			value = value[:remaining]
		}
		_, _ = b.Buffer.Write(value)
	}
	return written, nil
}

func invalidError(message string) error {
	return &Error{Code: "bootstrap_invalid", ExitCode: ExitInvalidConfiguration, Message: message}
}

func workspaceError(code, message string, err error) error {
	exitCode := ExitUnsafeWorkspace
	if errors.Is(err, syscall.ENOSPC) {
		code, message, exitCode = "workspace_full", "the workspace is full", ExitWorkspaceFull
	}
	return &Error{Code: code, ExitCode: exitCode, Message: message, Err: err}
}

func repositoryError(code, message string, err error) error {
	return &Error{Code: code, ExitCode: ExitRepositoryBootstrap, Message: message, Err: err}
}
