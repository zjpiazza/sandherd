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

	internalauth "github.com/zjpiazza/sandherd/internal/auth"
	"github.com/zjpiazza/sandherd/internal/buildinfo"
	"github.com/zjpiazza/sandherd/internal/gateway"
	cluster "github.com/zjpiazza/sandherd/internal/kubernetes"
	"github.com/zjpiazza/sandherd/internal/lifecycle"
	"github.com/zjpiazza/sandherd/internal/runtimeadapter"
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
	listen               string
	namespace            string
	kubeconfig           string
	context              string
	principalsFile       string
	adaptersFile         string
	capabilityPrivateKey string
	routerURL            string
	routerTokenFile      string
	runnerPort           int
	storageProfiles      profileMap
	secretProfiles       profileMap
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
	configuration := runConfig{
		storageProfiles: profileMap{"default": ""}, secretProfiles: profileMap{},
	}
	flags := flag.NewFlagSet("control-plane", flag.ContinueOnError)
	flags.SetOutput(stderr)
	showVersion := flags.Bool("version", false, "print version information and exit")
	flags.StringVar(&configuration.listen, "listen", ":8080", "public API listen address")
	flags.StringVar(&configuration.namespace, "namespace", "sandherd-system", "managed Kubernetes namespace")
	flags.StringVar(&configuration.kubeconfig, "kubeconfig", "", "kubeconfig path (defaults to in-cluster or standard loading rules)")
	flags.StringVar(&configuration.context, "context", "", "kubeconfig context override")
	flags.StringVar(&configuration.principalsFile, "auth-principals-file", "/var/run/secrets/sandherd/principals.json", "reloadable JSON file containing API principals and bearer credentials")
	flags.StringVar(&configuration.adaptersFile, "adapter-config-file", "/etc/sandherd/adapters.json", "versioned JSON file containing installed agent adapters and runtime profiles")
	flags.StringVar(&configuration.capabilityPrivateKey, "capability-private-key-file", "/var/run/secrets/sandherd/capability-private-key.pem", "Ed25519 key used to sign short-lived runner capabilities")
	flags.StringVar(&configuration.routerURL, "sandbox-router-url", "http://sandbox-router-svc.agent-sandbox-system.svc.cluster.local:8080", "Agent Sandbox router URL")
	flags.StringVar(&configuration.routerTokenFile, "sandbox-router-token-file", "/var/run/secrets/kubernetes.io/serviceaccount/token", "Kubernetes bearer token used with the sandbox router")
	flags.IntVar(&configuration.runnerPort, "runner-port", 8080, "runner port inside each sandbox")
	flags.Var(configuration.storageProfiles, "storage-profile", "approved public-profile=storage-class mapping; use '-' for the cluster default (repeatable)")
	flags.Var(configuration.secretProfiles, "secret-profile", "approved public-profile=credentialed-warm-pool mapping (repeatable)")
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
	if configuration.storageProfiles["default"] == "-" {
		configuration.storageProfiles["default"] = ""
	}
	for profile, storageClass := range configuration.storageProfiles {
		if storageClass == "-" {
			configuration.storageProfiles[profile] = ""
		}
	}
	if configuration.namespace == "" || configuration.principalsFile == "" || configuration.adaptersFile == "" || len(configuration.storageProfiles) == 0 || configuration.capabilityPrivateKey == "" || configuration.routerURL == "" || configuration.routerTokenFile == "" || configuration.runnerPort < 1 || configuration.runnerPort > 65535 {
		return runConfig{}, false, fmt.Errorf("namespace, adapter configuration, storage, and gateway routing are required")
	}
	return configuration, false, nil
}

func execute(configuration runConfig, stderr io.Writer) error {
	adapters, err := runtimeadapter.Load(configuration.adaptersFile)
	if err != nil {
		return fmt.Errorf("configure agent adapters: %w", err)
	}
	clientAuthenticator, err := internalauth.NewFileAuthenticator(configuration.principalsFile)
	if err != nil {
		return fmt.Errorf("configure API authentication: %w", err)
	}
	privateKeyContents, err := os.ReadFile(filepath.Clean(configuration.capabilityPrivateKey))
	if err != nil {
		return fmt.Errorf("read capability private key: %w", err)
	}
	privateKey, err := internalauth.ParsePrivateKeyPEM(privateKeyContents)
	if err != nil {
		return fmt.Errorf("parse capability private key: %w", err)
	}
	capabilitySigner, err := internalauth.NewSigner(privateKey, 30*time.Second)
	if err != nil {
		return err
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
	terminalGateway, err := gateway.New(gateway.Config{
		Resolver: repository, Signer: capabilitySigner, Events: events, Logger: logger,
		RouterURL: configuration.routerURL, RouterTokenFile: configuration.routerTokenFile,
		RunnerPort: configuration.runnerPort, Limits: gateway.DefaultLimits(),
	})
	if err != nil {
		return fmt.Errorf("configure terminal gateway: %w", err)
	}
	reconciler := cluster.NewReconciler(client, repository, configuration.namespace, adapters, events, logger)
	reconciler.ConfigureWorkspaceProfiles(configuration.storageProfiles, configuration.secretProfiles)
	controller := cluster.NewController(client, repository, reconciler, configuration.namespace, logger)
	var ready atomic.Bool
	if _, err := repository.ListAll(context.Background()); err != nil {
		return fmt.Errorf("verify Agent API access: %w", err)
	}
	ready.Store(true)
	api := NewServer(repository, controller, events, logger, clientAuthenticator, ready.Load, terminalGateway, adapters)
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
	logger.Info("control-plane API listening", "address", listener.Addr().String())
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
