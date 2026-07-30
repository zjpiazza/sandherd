package codexauth

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/zjpiazza/sandherd/internal/buildinfo"
)

const (
	exitConfiguration            = 2
	exitCredentialUnavailable    = 41
	exitReauthenticationRequired = 42
)

func Run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 1 && args[0] == "--version" {
		buildinfo.Write(stdout, "codex-auth")
		return 0
	}
	if len(args) == 0 {
		fmt.Fprintln(stderr, "Usage: codex-auth <serve|sync|import|status> [flags]")
		return exitConfiguration
	}
	switch args[0] {
	case "serve":
		return runServe(args[1:], stderr)
	case "sync":
		return runSync(args[1:], stderr)
	case "import":
		return runImport(args[1:], stdin, stdout, stderr)
	case "status":
		return runStatus(args[1:], stdout, stderr)
	default:
		fmt.Fprintln(stderr, "codex-auth: unknown command")
		return exitConfiguration
	}
}

func runServe(args []string, stderr io.Writer) int {
	flags := flag.NewFlagSet("codex-auth serve", flag.ContinueOnError)
	flags.SetOutput(stderr)
	listen := flags.String("listen", ":8090", "internal credential API listen address")
	authFile := flags.String("auth-file", "/var/lib/sandherd/codex/auth.json", "master Codex auth.json path")
	codexBinary := flags.String("codex-binary", "/usr/local/bin/codex", "pinned Codex executable")
	refreshInterval := flags.Duration("refresh-interval", time.Minute, "credential maintenance interval")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return exitConfiguration
	}
	logger := slog.New(slog.NewJSONHandler(stderr, nil)).With("component", "codex-auth-coordinator")
	coordinator, err := NewCoordinator(*authFile, *codexBinary, *refreshInterval, logger)
	if err != nil {
		fmt.Fprintln(stderr, "codex-auth: invalid coordinator configuration")
		return exitConfiguration
	}
	listener := &http.Server{Addr: *listen, Handler: coordinator.Handler(), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go coordinator.Run(ctx)
	serveError := make(chan error, 1)
	go func() { serveError <- listener.ListenAndServe() }()
	select {
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = listener.Shutdown(shutdown)
		return 0
	case err := <-serveError:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintln(stderr, "codex-auth: coordinator server failed")
			return 1
		}
		return 0
	}
}

func runSync(args []string, stderr io.Writer) int {
	flags := flag.NewFlagSet("codex-auth sync", flag.ContinueOnError)
	flags.SetOutput(stderr)
	sourceURL := flags.String("source-url", "http://sandherd-codex-auth:8090/v1/auth", "internal coordinator credential URL")
	authFile := flags.String("auth-file", "/home/sandherd/.codex/auth.json", "sandbox auth.json destination")
	once := flags.Bool("once", false, "synchronize once and exit")
	listen := flags.String("listen", ":8091", "sidecar probe listen address")
	interval := flags.Duration("interval", 15*time.Second, "synchronization interval")
	maxStale := flags.Duration("max-stale", 2*time.Minute, "maximum age of the last successful sync")
	timeout := flags.Duration("timeout", 10*time.Second, "per-request timeout")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return exitConfiguration
	}
	logger := slog.New(slog.NewJSONHandler(stderr, nil)).With("component", "codex-auth-sync")
	client, err := NewSyncClient(*sourceURL, *authFile, *timeout, logger)
	if err != nil || *interval <= 0 || *maxStale <= 0 {
		fmt.Fprintln(stderr, "codex-auth: invalid sync configuration")
		return exitConfiguration
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if *once {
		return syncExitCode(client.Sync(ctx), stderr)
	}
	if err := client.Sync(ctx); err != nil {
		return syncExitCode(err, stderr)
	}
	server := &http.Server{Addr: *listen, Handler: client.Handler(*maxStale), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second}
	serveError := make(chan error, 1)
	go func() { serveError <- server.ListenAndServe() }()
	syncError := make(chan error, 1)
	go func() { syncError <- client.Run(ctx, *interval) }()
	select {
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
		return 0
	case err := <-syncError:
		return syncExitCode(err, stderr)
	case err := <-serveError:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintln(stderr, "codex-auth: sync probe server failed")
			return 1
		}
		return 0
	}
}

func syncExitCode(err error, stderr io.Writer) int {
	if err == nil {
		return 0
	}
	if errors.Is(err, ErrReauthenticationRequired) {
		fmt.Fprintln(stderr, "codex-auth: platform credential requires reauthentication")
		return exitReauthenticationRequired
	}
	fmt.Fprintln(stderr, "codex-auth: platform credential is unavailable")
	return exitCredentialUnavailable
}

func runImport(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("codex-auth import", flag.ContinueOnError)
	flags.SetOutput(stderr)
	authFile := flags.String("auth-file", "/var/lib/sandherd/codex/auth.json", "master Codex auth.json path")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || !filepath.IsAbs(*authFile) {
		return exitConfiguration
	}
	contents, err := io.ReadAll(io.LimitReader(stdin, maxAuthBytes+1))
	if err != nil || len(contents) > maxAuthBytes {
		fmt.Fprintln(stderr, "codex-auth: credential input is invalid")
		return exitConfiguration
	}
	if _, err := writeCredential(*authFile, contents, true); err != nil {
		fmt.Fprintln(stderr, "codex-auth: credential input is invalid")
		return exitConfiguration
	}
	fmt.Fprintln(stdout, "Codex ChatGPT credential imported.")
	return 0
}

func runStatus(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("codex-auth status", flag.ContinueOnError)
	flags.SetOutput(stderr)
	authFile := flags.String("auth-file", "/var/lib/sandherd/codex/auth.json", "master Codex auth.json path")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return exitConfiguration
	}
	_, metadata, err := readMaster(*authFile)
	if err != nil {
		fmt.Fprintln(stdout, `{"status":"credential_not_ready"}`)
		return exitCredentialUnavailable
	}
	fmt.Fprintf(stdout, "{\"status\":\"ready\",\"lastRefresh\":%q,\"expiresAt\":%q}\n", metadata.LastRefresh.Format(time.RFC3339), metadata.ExpiresAt.Format(time.RFC3339))
	return 0
}
