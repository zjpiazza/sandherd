package controlplane

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/zjpiazza/sandherd/internal/buildinfo"
	cluster "github.com/zjpiazza/sandherd/internal/kubernetes"
	"github.com/zjpiazza/sandherd/internal/lifecycle"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

type profileMap map[string]string

func (p profileMap) String() string {
	values := make([]string, 0, len(p))
	for profile, pool := range p {
		values = append(values, profile+"="+pool)
	}
	return strings.Join(values, ",")
}

func (p profileMap) Set(value string) error {
	profile, pool, ok := strings.Cut(value, "=")
	if !ok || profile == "" || pool == "" {
		return fmt.Errorf("profile must have the form public-name=warm-pool-name")
	}
	p[profile] = pool
	return nil
}

type runConfig struct {
	listen     string
	namespace  string
	kubeconfig string
	context    string
	tokenFile  string
	owner      string
	profiles   profileMap
}

func Run(args []string, stdout, stderr io.Writer) int {
	configuration, showVersion, err := parseRunConfig(args, stderr)
	if err != nil {
		return 2
	}
	if showVersion {
		buildinfo.Write(stdout, "control-plane")
		return 0
	}
	if err := execute(configuration, stderr); err != nil {
		fmt.Fprintf(stderr, "control-plane: %v\n", err)
		return 1
	}
	return 0
}

func parseRunConfig(args []string, stderr io.Writer) (runConfig, bool, error) {
	configuration := runConfig{profiles: profileMap{"standard": "sandherd-standard"}}
	flags := flag.NewFlagSet("control-plane", flag.ContinueOnError)
	flags.SetOutput(stderr)
	showVersion := flags.Bool("version", false, "print version information and exit")
	flags.StringVar(&configuration.listen, "listen", ":8080", "public API listen address")
	flags.StringVar(&configuration.namespace, "namespace", "sandherd-system", "managed Kubernetes namespace")
	flags.StringVar(&configuration.kubeconfig, "kubeconfig", "", "kubeconfig path (defaults to in-cluster or standard loading rules)")
	flags.StringVar(&configuration.context, "context", "", "kubeconfig context override")
	flags.StringVar(&configuration.tokenFile, "auth-token-file", "/var/run/secrets/sandherd/api-token", "file containing the API bearer token")
	flags.StringVar(&configuration.owner, "owner-id", "", "stable owner ID represented by the static credential")
	flags.Var(configuration.profiles, "sandbox-profile", "approved public-profile=warm-pool mapping (repeatable)")
	flags.Usage = func() {
		fmt.Fprintln(stderr, "Usage: control-plane [flags]")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		return runConfig{}, false, err
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(stderr, "control-plane: unexpected arguments: %v\n", flags.Args())
		return runConfig{}, false, fmt.Errorf("unexpected arguments")
	}
	if *showVersion {
		return runConfig{}, true, nil
	}
	if configuration.owner == "" || len(configuration.owner) > 256 {
		fmt.Fprintln(stderr, "control-plane: --owner-id is required and must not exceed 256 characters")
		return runConfig{}, false, fmt.Errorf("invalid owner ID")
	}
	if configuration.namespace == "" || len(configuration.profiles) == 0 {
		return runConfig{}, false, fmt.Errorf("namespace and at least one sandbox profile are required")
	}
	return configuration, false, nil
}

func execute(configuration runConfig, stderr io.Writer) error {
	token, err := readToken(configuration.tokenFile)
	if err != nil {
		return fmt.Errorf("read API token: %w", err)
	}
	restConfig, err := loadKubeConfig(configuration)
	if err != nil {
		return err
	}
	restConfig.UserAgent = "sandherd-control-plane/" + buildinfo.Version
	restConfig.QPS = 20
	restConfig.Burst = 40
	client, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("create Kubernetes client: %w", err)
	}
	listener, err := net.Listen("tcp", configuration.listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", configuration.listen, err)
	}
	defer listener.Close()
	logger := slog.New(slog.NewJSONHandler(stderr, nil)).With("component", "control-plane", "namespace", configuration.namespace)
	repository := cluster.NewRepository(client, configuration.namespace)
	events := lifecycle.NewEventBus(2048)
	reconciler := cluster.NewReconciler(client, repository, configuration.namespace, configuration.profiles, events, logger)
	controller := cluster.NewController(client, repository, reconciler, configuration.namespace, logger)
	var ready atomic.Bool
	if _, err := repository.ListAll(context.Background()); err != nil {
		return fmt.Errorf("verify Agent API access: %w", err)
	}
	ready.Store(true)
	api := NewServer(repository, controller, events, logger, token, configuration.owner, ready.Load)
	httpServer := &http.Server{
		Handler: api.Handler(), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 90 * time.Second,
		ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	controllerError := make(chan error, 1)
	go func() { controllerError <- controller.Run(ctx) }()
	serverError := make(chan error, 1)
	go func() { serverError <- httpServer.Serve(listener) }()
	logger.Info("control-plane API listening", "address", listener.Addr().String(), "owner", configuration.owner)
	select {
	case <-ctx.Done():
	case err := <-controllerError:
		if err != nil {
			return fmt.Errorf("run lifecycle controller: %w", err)
		}
	case err := <-serverError:
		if err != nil && err != http.ErrServerClosed {
			return fmt.Errorf("serve control-plane API: %w", err)
		}
	}
	ready.Store(false)
	shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownContext); err != nil {
		return fmt.Errorf("shut down control-plane API: %w", err)
	}
	return nil
}

func loadKubeConfig(configuration runConfig) (*rest.Config, error) {
	if configuration.kubeconfig == "" {
		if inCluster, err := rest.InClusterConfig(); err == nil {
			return inCluster, nil
		}
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if configuration.kubeconfig != "" {
		rules.ExplicitPath = configuration.kubeconfig
	}
	overrides := &clientcmd.ConfigOverrides{CurrentContext: configuration.context}
	result, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("load Kubernetes configuration: %w", err)
	}
	return result, nil
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
