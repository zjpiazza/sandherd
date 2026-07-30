package runner

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	internalauth "github.com/zjpiazza/sandherd/internal/auth"
	"github.com/zjpiazza/sandherd/internal/buildinfo"
)

const kubernetesServiceAccountToken = "/var/run/secrets/kubernetes.io/serviceaccount/token"

type config struct {
	agentID             string
	listenAddress       string
	authTokenFile       string
	observeTokenFile    string
	capabilityPublicKey string
	workingDirectory    string
	replayBytes         int
	replayFrames        int
	subscriberBuffer    int
	controllerLease     time.Duration
	stopGrace           time.Duration
	attachTimeout       time.Duration
	writeTimeout        time.Duration
	initialTerminalSize TerminalSize
	command             []string
	healthCheck         []string
}

// Run parses runner flags, owns one agent process, and serves its internal API.
func Run(args []string, stdout, stderr io.Writer) int {
	configuration, showVersion, err := parseConfig(args, stderr)
	if err != nil {
		return 2
	}
	if showVersion {
		buildinfo.Write(stdout, "runner")
		return 0
	}
	if err := execute(configuration, stderr); err != nil {
		fmt.Fprintf(stderr, "runner: %v\n", err)
		return 1
	}
	return 0
}

func parseConfig(args []string, stderr io.Writer) (config, bool, error) {
	var result config
	flags := flag.NewFlagSet("runner", flag.ContinueOnError)
	flags.SetOutput(stderr)
	showVersion := flags.Bool("version", false, "print version information and exit")
	flags.StringVar(&result.agentID, "agent-id", "", "logical agent UUID (required)")
	flags.StringVar(&result.listenAddress, "listen", ":8080", "internal API listen address")
	flags.StringVar(&result.authTokenFile, "auth-token-file", "/var/run/secrets/sandherd/token", "file containing the control bearer token")
	flags.StringVar(&result.observeTokenFile, "observe-token-file", "", "optional file containing an observe-only bearer token")
	flags.StringVar(&result.capabilityPublicKey, "capability-public-key-file", "", "optional Ed25519 public key for scoped gateway capabilities")
	flags.StringVar(&result.workingDirectory, "working-directory", ".", "agent process working directory")
	flags.IntVar(&result.replayBytes, "replay-bytes", 4*1024*1024, "maximum retained terminal output bytes")
	flags.IntVar(&result.replayFrames, "replay-frames", 4096, "maximum retained terminal output frames")
	flags.IntVar(&result.subscriberBuffer, "subscriber-buffer", 256, "maximum queued live frames per attachment")
	flags.DurationVar(&result.controllerLease, "controller-lease", 30*time.Second, "controller liveness lease")
	flags.DurationVar(&result.stopGrace, "stop-grace", 10*time.Second, "grace period before SIGKILL")
	flags.DurationVar(&result.attachTimeout, "attach-timeout", 10*time.Second, "time allowed for the first attach frame")
	flags.DurationVar(&result.writeTimeout, "write-timeout", 5*time.Second, "per-frame WebSocket write timeout")
	columns := flags.Uint("columns", 120, "initial PTY columns")
	rows := flags.Uint("rows", 40, "initial PTY rows")
	flags.Usage = func() {
		fmt.Fprintln(stderr, "Usage: runner [flags] -- command [args...]")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		return config{}, false, err
	}
	if *showVersion {
		if flags.NArg() != 0 {
			fmt.Fprintln(stderr, "runner: --version does not accept a command")
			return config{}, false, fmt.Errorf("unexpected arguments")
		}
		return config{}, true, nil
	}
	result.command = flags.Args()
	if len(result.command) == 0 && os.Getenv("SANDHERD_AGENT_COMMAND_JSON") != "" {
		if err := json.Unmarshal([]byte(os.Getenv("SANDHERD_AGENT_COMMAND_JSON")), &result.command); err != nil {
			fmt.Fprintln(stderr, "runner: SANDHERD_AGENT_COMMAND_JSON must be a JSON command array")
			return config{}, false, fmt.Errorf("invalid agent command")
		}
	}
	if os.Getenv("SANDHERD_AGENT_HEALTH_CHECK_JSON") != "" {
		if err := json.Unmarshal([]byte(os.Getenv("SANDHERD_AGENT_HEALTH_CHECK_JSON")), &result.healthCheck); err != nil {
			fmt.Fprintln(stderr, "runner: SANDHERD_AGENT_HEALTH_CHECK_JSON must be a JSON command array")
			return config{}, false, fmt.Errorf("invalid adapter health check")
		}
	}
	if result.agentID == "" || !looksLikeUUID(result.agentID) {
		fmt.Fprintln(stderr, "runner: --agent-id must be a UUID")
		flags.Usage()
		return config{}, false, fmt.Errorf("invalid agent ID")
	}
	if len(result.command) == 0 {
		fmt.Fprintln(stderr, "runner: an agent command is required after -- or through SANDHERD_AGENT_COMMAND_JSON")
		flags.Usage()
		return config{}, false, fmt.Errorf("missing agent command")
	}
	if len(result.command) > 64 || !filepath.IsAbs(result.command[0]) {
		fmt.Fprintln(stderr, "runner: the agent command must contain an absolute executable and at most 64 arguments")
		return config{}, false, fmt.Errorf("invalid agent command")
	}
	for _, argument := range result.command {
		if len(argument) > 4096 || strings.IndexByte(argument, 0) >= 0 {
			fmt.Fprintln(stderr, "runner: the agent command contains an invalid argument")
			return config{}, false, fmt.Errorf("invalid agent command")
		}
	}
	if len(result.healthCheck) == 0 || len(result.healthCheck) > 64 || !filepath.IsAbs(result.healthCheck[0]) {
		fmt.Fprintln(stderr, "runner: the adapter health check must contain an absolute executable and at most 64 arguments")
		return config{}, false, fmt.Errorf("invalid adapter health check")
	}
	for _, argument := range result.healthCheck {
		if len(argument) > 4096 || strings.IndexByte(argument, 0) >= 0 {
			fmt.Fprintln(stderr, "runner: the adapter health check contains an invalid argument")
			return config{}, false, fmt.Errorf("invalid adapter health check")
		}
	}
	if result.authTokenFile == "" && result.capabilityPublicKey == "" {
		fmt.Fprintln(stderr, "runner: --auth-token-file or --capability-public-key-file is required")
		return config{}, false, fmt.Errorf("missing authentication method")
	}
	if result.authTokenFile == "" && result.observeTokenFile != "" {
		fmt.Fprintln(stderr, "runner: --observe-token-file requires --auth-token-file")
		return config{}, false, fmt.Errorf("observe token without static control token")
	}
	if *columns == 0 || *columns > 1000 || *rows == 0 || *rows > 1000 {
		fmt.Fprintln(stderr, "runner: columns and rows must be between 1 and 1000")
		return config{}, false, fmt.Errorf("invalid terminal size")
	}
	result.initialTerminalSize = TerminalSize{Columns: uint16(*columns), Rows: uint16(*rows)}
	if result.replayBytes < 1 || result.replayFrames < 1 || result.subscriberBuffer < 1 {
		fmt.Fprintln(stderr, "runner: replay and subscriber limits must be positive")
		return config{}, false, fmt.Errorf("invalid buffer limit")
	}
	if result.controllerLease <= 0 || result.stopGrace <= 0 || result.attachTimeout <= 0 || result.writeTimeout <= 0 {
		fmt.Fprintln(stderr, "runner: timeout and grace durations must be positive")
		return config{}, false, fmt.Errorf("invalid duration")
	}
	return result, false, nil
}

func execute(configuration config, stderr io.Writer) error {
	if _, err := os.Stat(kubernetesServiceAccountToken); err == nil {
		return fmt.Errorf("refusing to start with a Kubernetes service-account token mounted at %s", kubernetesServiceAccountToken)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect Kubernetes service-account token: %w", err)
	}
	var authenticators multipleAuthenticators
	var controlToken []byte
	var err error
	if configuration.authTokenFile != "" {
		controlToken, err = readToken(configuration.authTokenFile)
		if err != nil {
			return fmt.Errorf("read control token: %w", err)
		}
	}
	var observeToken []byte
	if configuration.observeTokenFile != "" {
		observeToken, err = readToken(configuration.observeTokenFile)
		if err != nil {
			return fmt.Errorf("read observe token: %w", err)
		}
		if constantTimeEqual(controlToken, observeToken) {
			return fmt.Errorf("control and observe tokens must differ")
		}
	}
	if len(controlToken) > 0 {
		authenticators = append(authenticators, staticAuthenticator{controlToken: controlToken, observeToken: observeToken})
	}
	if configuration.capabilityPublicKey != "" {
		contents, readErr := os.ReadFile(filepath.Clean(configuration.capabilityPublicKey))
		if readErr != nil {
			return fmt.Errorf("read capability public key: %w", readErr)
		}
		publicKey, parseErr := internalauth.ParsePublicKeyPEM(contents)
		if parseErr != nil {
			return fmt.Errorf("parse capability public key: %w", parseErr)
		}
		verifier, verifyErr := internalauth.NewVerifier(publicKey)
		if verifyErr != nil {
			return verifyErr
		}
		authenticators = append(authenticators, capabilityAuthenticator{verifier: verifier, agentID: configuration.agentID})
	}
	if _, err := os.Stat(configuration.workingDirectory); err != nil {
		return fmt.Errorf("inspect working directory: %w", err)
	}
	if err := runAdapterHealthCheck(configuration.healthCheck, configuration.workingDirectory, os.Environ()); err != nil {
		return err
	}

	listener, err := net.Listen("tcp", configuration.listenAddress)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", configuration.listenAddress, err)
	}
	defer listener.Close()

	logger := slog.New(slog.NewJSONHandler(stderr, nil)).With(
		"component", "runner",
		"agent_id", configuration.agentID,
	)
	hub := newHub(
		configuration.agentID,
		configuration.replayBytes,
		configuration.replayFrames,
		configuration.subscriberBuffer,
		configuration.controllerLease,
	)
	defer hub.close()
	process := newAgentProcess(configuration.command, configuration.workingDirectory, os.Environ(), logger, hub.publishOutput, hub.publishExit)
	if err := process.start(configuration.initialTerminalSize); err != nil {
		return err
	}
	hub.setProcess(process)
	go hub.runLeaseReaper()

	api := &server{
		hub:           hub,
		process:       process,
		authenticator: authenticators,
		logger:        logger,
		stopGrace:     configuration.stopGrace,
		attachTimeout: configuration.attachTimeout,
		writeTimeout:  configuration.writeTimeout,
	}
	httpServer := &http.Server{
		Handler:           api.handler(),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       90 * time.Second,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}
	serveError := make(chan error, 1)
	go func() { serveError <- httpServer.Serve(listener) }()
	logger.Info("runner API listening", "address", listener.Addr().String(), "runner_generation", hub.generation)

	shutdownContext, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()
	select {
	case <-shutdownContext.Done():
		logger.Info("runner shutdown requested")
	case err := <-serveError:
		if err != nil && err != http.ErrServerClosed {
			process.terminate(configuration.stopGrace)
			return fmt.Errorf("serve runner API: %w", err)
		}
		return nil
	}

	process.terminate(configuration.stopGrace)
	serverShutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(serverShutdownContext); err != nil {
		return fmt.Errorf("shut down runner API: %w", err)
	}
	return nil
}

func runAdapterHealthCheck(command []string, workingDirectory string, environment []string) error {
	checkContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	check := exec.CommandContext(checkContext, command[0], command[1:]...)
	check.Dir = workingDirectory
	check.Env = environment
	check.Stdout = io.Discard
	check.Stderr = io.Discard
	if err := check.Run(); err != nil {
		return fmt.Errorf("adapter health check failed")
	}
	return nil
}

func readToken(path string) ([]byte, error) {
	contents, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, err
	}
	token := []byte(strings.TrimSpace(string(contents)))
	if len(token) < 16 {
		return nil, fmt.Errorf("token must contain at least 16 non-whitespace bytes")
	}
	return token, nil
}

func looksLikeUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for index, character := range value {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			if character != '-' {
				return false
			}
			continue
		}
		if !strings.ContainsRune("0123456789abcdefABCDEF", character) {
			return false
		}
	}
	return true
}
