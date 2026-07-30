package bootstrap

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/zjpiazza/sandherd/internal/buildinfo"
)

func Run(arguments []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if os.Getenv("SANDHERD_ASKPASS") == "1" {
		prompt := ""
		if len(arguments) > 0 {
			prompt = arguments[0]
		}
		if err := AskPass(os.Getenv("SANDHERD_GIT_CREDENTIAL_FILE"), prompt, stdout); err != nil {
			fmt.Fprintf(stderr, "workspace-bootstrap: %v\n", err)
			return ExitInvalidConfiguration
		}
		return 0
	}
	if os.Getenv("SANDHERD_SSH_WRAPPER") == "1" {
		if err := RunSSHWrapper(context.Background(), arguments, stdin, stdout, stderr); err != nil {
			return 255
		}
		return 0
	}

	flags := flag.NewFlagSet("workspace-bootstrap", flag.ContinueOnError)
	flags.SetOutput(stderr)
	showVersion := flags.Bool("version", false, "print version information and exit")
	workspace := flags.String("workspace", envOrDefault("SANDHERD_WORKSPACE", "/workspace"), "durable workspace mount")
	repositoryURL := flags.String("repository-url", os.Getenv("SANDHERD_REPOSITORY_URL"), "HTTPS or SSH repository URL")
	revision := flags.String("revision", envOrDefault("SANDHERD_REPOSITORY_REVISION", "HEAD"), "repository revision")
	credentialFile := flags.String("credential-file", os.Getenv("SANDHERD_GIT_CREDENTIAL_FILE"), "HTTPS credential JSON file")
	sshKeyFile := flags.String("ssh-key-file", os.Getenv("SANDHERD_GIT_SSH_KEY_FILE"), "SSH private key file")
	knownHostsFile := flags.String("known-hosts-file", os.Getenv("SANDHERD_GIT_KNOWN_HOSTS_FILE"), "SSH known-hosts file")
	gitBinary := flags.String("git", envOrDefault("SANDHERD_GIT_BINARY", "git"), "git executable")
	sshBinary := flags.String("ssh", envOrDefault("SANDHERD_SSH_BINARY", "ssh"), "ssh executable")
	timeout := flags.Duration("timeout", 5*time.Minute, "maximum bootstrap duration")
	flags.Usage = func() {
		fmt.Fprintln(stderr, "Usage: workspace-bootstrap [flags]")
		flags.PrintDefaults()
	}
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 0 {
		return 2
	}
	if *showVersion {
		buildinfo.Write(stdout, "workspace-bootstrap")
		return 0
	}
	metadata, err := Execute(context.Background(), Options{
		Workspace: *workspace, RepositoryURL: strings.TrimSpace(*repositoryURL), Revision: strings.TrimSpace(*revision),
		CredentialFile: *credentialFile, SSHKeyFile: *sshKeyFile, KnownHostsFile: *knownHostsFile,
		GitBinary: *gitBinary, SSHBinary: *sshBinary, Timeout: *timeout,
	})
	if err != nil {
		var typed *Error
		if errors.As(err, &typed) {
			fmt.Fprintf(stderr, "workspace-bootstrap: %s: %s\n", typed.Code, typed.Message)
			return typed.ExitCode
		}
		fmt.Fprintf(stderr, "workspace-bootstrap: bootstrap_internal: %v\n", err)
		return 1
	}
	if metadata.ResolvedCommit != "" {
		fmt.Fprintf(stdout, "workspace ready at %s (%s)\n", metadata.ResolvedCommit, metadata.RequestedRevision)
	} else {
		fmt.Fprintln(stdout, "empty workspace ready")
	}
	return 0
}

func envOrDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
