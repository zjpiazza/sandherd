// Package smoke exercises the public API against an installed Sandherd cluster.
package smoke

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/zjpiazza/sandherd/internal/buildinfo"
	"github.com/zjpiazza/sandherd/internal/herdrbridge"
	cluster "github.com/zjpiazza/sandherd/internal/kubernetes"
	"github.com/zjpiazza/sandherd/internal/lifecycle"
	"github.com/zjpiazza/sandherd/internal/runner"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
)

func Run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("smoke", flag.ContinueOnError)
	flags.SetOutput(stderr)
	baseURL := flags.String("base-url", "", "Sandherd public HTTP(S) API URL")
	tokenFile := flags.String("token-file", "", "file containing a control principal bearer token")
	profile := flags.String("sandbox-profile", "standard", "installed shell sandbox profile")
	storage := flags.String("storage-profile", "default", "approved durable storage profile")
	cpu := flags.String("cpu", "1", "runner CPU request and limit")
	memory := flags.String("memory", "2Gi", "runner memory request and limit")
	timeout := flags.Duration("timeout", 8*time.Minute, "overall test deadline")
	recoveryContext := flags.String("recovery-context", "", "opt in to completed-Pod fault injection using this Kubernetes context")
	namespace := flags.String("namespace", "sandherd-system", "namespace for optional recovery checks")
	version := flags.Bool("version", false, "print version and exit")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return 2
	}
	if *version {
		buildinfo.Write(stdout, "smoke")
		return 0
	}
	if *timeout <= 0 {
		fmt.Fprintln(stderr, "smoke: timeout must be positive")
		return 2
	}
	configuration := herdrbridge.Config{
		BaseURL: *baseURL, TokenFile: *tokenFile, PollInterval: time.Second, ReconnectLimit: time.Minute,
		Create: lifecycle.CreateRequest{Spec: lifecycle.AgentSpec{
			Kind: "shell", SandboxProfile: *profile,
			Resources: lifecycle.ResourceSpec{CPU: *cpu, Memory: *memory},
			Workspace: lifecycle.WorkspaceSpec{Size: "1Gi", StorageProfile: *storage, RetentionPolicy: "delete"},
		}},
	}
	api, err := herdrbridge.NewClient(configuration, nil)
	if err != nil {
		fmt.Fprintf(stderr, "smoke: %v\n", err)
		return 2
	}
	var kubernetes dynamic.Interface
	if *recoveryContext != "" {
		config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{CurrentContext: *recoveryContext}).ClientConfig()
		if err != nil {
			fmt.Fprintln(stderr, "smoke: Kubernetes recovery context is unavailable")
			return 2
		}
		kubernetes, err = dynamic.NewForConfig(config)
		if err != nil {
			fmt.Fprintln(stderr, "smoke: cannot configure recovery client")
			return 2
		}
	}
	interrupt, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(interrupt, *timeout)
	defer cancel()
	if err := verify(ctx, api, configuration, kubernetes, *namespace, stdout); err != nil {
		fmt.Fprintf(stderr, "smoke: %v\n", err)
		return 1
	}
	return 0
}

func verify(ctx context.Context, api *herdrbridge.Client, configuration herdrbridge.Config, kubernetes dynamic.Interface, namespace string, output io.Writer) (result error) {
	name := "smoke-" + strings.ReplaceAll(lifecycle.NewID(), "-", "")
	agent, err := api.Create(ctx, configuration.CreateRequest(name))
	if err != nil {
		return fmt.Errorf("create: %w", err)
	}
	fmt.Fprintf(output, "created %s (%s)\n", name, agent.ID)
	var sandboxName string
	defer func() {
		// Cancellation must not strand the test workspace. This context is
		// independent of the test deadline, but still bounds cleanup.
		cleanup, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		if _, err := api.Delete(cleanup, agent.ID); err != nil && !isNotFound(err) {
			result = errors.Join(result, fmt.Errorf("cleanup %s: %w", agent.ID, err))
			return
		}
		if err := poll(cleanup, func() (bool, error) {
			_, err := api.Get(cleanup, agent.ID)
			if isNotFound(err) {
				return true, nil
			}
			return false, err
		}); err != nil {
			result = errors.Join(result, fmt.Errorf("wait for deletion of %s: %w", agent.ID, err))
			return
		}
		if kubernetes != nil {
			if err := poll(cleanup, func() (bool, error) {
				for _, resource := range []dynamic.ResourceInterface{
					kubernetes.Resource(cluster.SandboxClaimGVR).Namespace(namespace),
					kubernetes.Resource(cluster.PodGVR).Namespace(namespace),
					kubernetes.Resource(cluster.PVCGVR).Namespace(namespace),
					kubernetes.Resource(cluster.SandboxTemplateGVR).Namespace(namespace),
					kubernetes.Resource(cluster.SandboxWarmPoolGVR).Namespace(namespace),
				} {
					list, err := resource.List(cleanup, metav1.ListOptions{LabelSelector: cluster.AgentIDLabel + "=" + agent.ID})
					if err != nil {
						return false, fmt.Errorf("read cleanup resources")
					}
					if len(list.Items) != 0 {
						return false, nil
					}
				}
				if sandboxName != "" {
					_, err := kubernetes.Resource(cluster.SandboxGVR).Namespace(namespace).Get(cleanup, sandboxName, metav1.GetOptions{})
					if err == nil {
						return false, nil
					}
					if !apierrors.IsNotFound(err) {
						return false, fmt.Errorf("read cleanup sandbox")
					}
				}
				return true, nil
			}); err != nil {
				result = errors.Join(result, fmt.Errorf("test resources remain for %s: %w", agent.ID, err))
				return
			}
		}
		fmt.Fprintln(output, "deleted test agent and verified cleanup")
	}()
	if err := waitState(ctx, api, agent.ID, lifecycle.StateRunning); err != nil {
		return err
	}
	if kubernetes != nil {
		sandbox, err := allocatedSandbox(ctx, kubernetes, namespace, agent.ID)
		if err != nil {
			return err
		}
		sandboxName = sandbox.GetName()
		if err := verifyResources(ctx, kubernetes, namespace, sandboxName, agent.Spec.Resources); err != nil {
			return err
		}
		fmt.Fprintln(output, "running Pod enforces requested CPU and memory")
	}
	terminal, err := attach(ctx, api, agent.ID, nil)
	if err != nil {
		return err
	}
	defer func() { terminal.close() }()
	generation := terminal.generation
	canary := lifecycle.NewID()
	// Build the response from separate literals so echoed input cannot pass
	// the output assertion without the shell actually executing the command.
	command := fmt.Sprintf("printf '%%s' '%s' > .sandherd-smoke-canary; printf '\\n%%s%%s\\n' 'created-' '%s'\n", canary, canary)
	if err := terminal.command(ctx, command, "created-"+canary); err != nil {
		return fmt.Errorf("interactive input: %w", err)
	}
	sequence := terminal.sequence
	if err := terminal.write(ctx, runner.Frame{Type: "input", LeaseID: terminal.lease, Data: []byte(fmt.Sprintf("sleep 2; printf '\\n%%s%%s\\n' 'detached-' '%s'\n", canary))}); err != nil {
		return err
	}
	terminal.close()
	if err := pause(ctx, 3*time.Second); err != nil {
		return err
	}
	terminal, err = attach(ctx, api, agent.ID, &sequence)
	if err != nil {
		return err
	}
	if terminal.generation != generation {
		return fmt.Errorf("disconnect restarted the runner")
	}
	if err := terminal.await(ctx, "detached-"+canary); err != nil {
		return fmt.Errorf("missed-output replay: %w", err)
	}
	fmt.Fprintln(output, "interactive input and replay across disconnect passed")
	terminal.close()
	if _, err := api.Stop(ctx, agent.ID); err != nil {
		return fmt.Errorf("stop: %w", err)
	}
	if err := waitState(ctx, api, agent.ID, lifecycle.StateStopped); err != nil {
		return err
	}
	var pvcUID types.UID
	if kubernetes != nil {
		pvcUID, err = workspaceUID(ctx, kubernetes, namespace, agent.ID)
		if err != nil {
			return err
		}
		if err := injectCompletion(ctx, kubernetes, namespace, agent.ID); err != nil {
			return err
		}
	}
	if _, err := api.Resume(ctx, agent.ID); err != nil {
		return fmt.Errorf("resume: %w", err)
	}
	if err := waitState(ctx, api, agent.ID, lifecycle.StateRunning); err != nil {
		return err
	}
	terminal, err = attach(ctx, api, agent.ID, nil)
	if err != nil {
		return err
	}
	if terminal.generation == generation {
		return fmt.Errorf("stop/resume did not replace the runner")
	}
	if err := terminal.command(ctx, "printf '\\n'; cat .sandherd-smoke-canary; printf '\\n'\n", canary); err != nil {
		return fmt.Errorf("workspace persistence: %w", err)
	}
	fmt.Fprintln(output, "stop/resume preserved the workspace and replaced the runner")
	if kubernetes != nil {
		if err := terminal.command(ctx, "printf '\\n'; cat .sandherd-smoke-recovery; printf '\\n'\n", "completed-pod-recovered"); err != nil {
			return fmt.Errorf("completed-Pod recovery: %w", err)
		}
		after, err := workspaceUID(ctx, kubernetes, namespace, agent.ID)
		if err != nil || after != pvcUID {
			return fmt.Errorf("completed-Pod recovery did not preserve the PVC identity")
		}
		fmt.Fprintln(output, "completed-Pod recovery preserved the same PVC")
		if err := verifyResources(ctx, kubernetes, namespace, sandboxName, agent.Spec.Resources); err != nil {
			return err
		}
		fmt.Fprintln(output, "recovered Pod retains requested CPU and memory")
	}
	return nil
}

func verifyResources(ctx context.Context, client dynamic.Interface, namespace, name string, requested lifecycle.ResourceSpec) error {
	pod, err := client.Resource(cluster.PodGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("cannot read running Pod resource budgets")
	}
	containers, _, _ := unstructured.NestedSlice(pod.Object, "spec", "containers")
	for _, value := range containers {
		container, ok := value.(map[string]any)
		if !ok || container["name"] != "runner" {
			continue
		}
		for _, category := range []string{"requests", "limits"} {
			for key, expected := range map[string]string{"cpu": requested.CPU, "memory": requested.Memory} {
				actual, _, _ := unstructured.NestedString(container, "resources", category, key)
				got, gotErr := resource.ParseQuantity(actual)
				want, wantErr := resource.ParseQuantity(expected)
				if gotErr != nil || wantErr != nil || got.Cmp(want) != 0 {
					return fmt.Errorf("runner %s.%s = %q, want %q", category, key, actual, expected)
				}
			}
		}
		return nil
	}
	return fmt.Errorf("running Pod has no runner container")
}

func waitState(ctx context.Context, api *herdrbridge.Client, id string, state lifecycle.State) error {
	return poll(ctx, func() (bool, error) {
		agent, err := api.Get(ctx, id)
		if err != nil {
			return false, err
		}
		if agent.Status.State == lifecycle.StateFailed {
			return false, fmt.Errorf("agent failed: %s", agent.Status.Reason)
		}
		return agent.Status.State == state, nil
	})
}

func isNotFound(err error) bool {
	var apiError *herdrbridge.APIError
	return errors.As(err, &apiError) && apiError.Status == http.StatusNotFound
}

func poll(ctx context.Context, check func() (bool, error)) error {
	for {
		if done, err := check(); done || err != nil {
			return err
		}
		if err := pause(ctx, 500*time.Millisecond); err != nil {
			return err
		}
	}
}

func pause(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type terminal struct {
	connection *websocket.Conn
	cancel     context.CancelFunc
	lease      string
	generation string
	sequence   uint64
	buffer     string
}

func attach(ctx context.Context, api *herdrbridge.Client, id string, sequence *uint64) (*terminal, error) {
	authorization, err := api.AuthorizationHeader()
	if err != nil {
		return nil, err
	}
	connection, response, err := websocket.Dial(ctx, api.TerminalURL(id), &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": []string{authorization}}, Subprotocols: []string{runner.Protocol}})
	if err != nil {
		if response != nil {
			return nil, fmt.Errorf("terminal upgrade returned HTTP %d", response.StatusCode)
		}
		return nil, fmt.Errorf("terminal connection failed")
	}
	connection.SetReadLimit(2 * 1024 * 1024)
	attachment, cancel := context.WithCancel(ctx)
	terminal := &terminal{connection: connection, cancel: cancel}
	if err := terminal.write(ctx, runner.Frame{Type: "attach", ProtocolVersion: runner.ProtocolVersion, Role: "control", Takeover: true, AfterSequence: sequence, TerminalSize: &runner.TerminalSize{Columns: 100, Rows: 30}}); err != nil {
		terminal.close()
		return nil, err
	}
	var frame runner.Frame
	if err := wsjson.Read(ctx, connection, &frame); err != nil || frame.Type != "attached" {
		terminal.close()
		return nil, fmt.Errorf("terminal did not acknowledge attachment")
	}
	terminal.lease, terminal.generation = frame.LeaseID, frame.RunnerGeneration
	go func() {
		for pause(attachment, 10*time.Second) == nil {
			if terminal.write(attachment, runner.Frame{Type: "ping", Nonce: lifecycle.NewID()}) != nil {
				return
			}
		}
	}()
	return terminal, nil
}

func (t *terminal) close() {
	if t != nil {
		t.cancel()
		_ = t.connection.CloseNow()
	}
}

func (t *terminal) write(ctx context.Context, frame runner.Frame) error {
	write, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return wsjson.Write(write, t.connection, frame)
}

func (t *terminal) command(ctx context.Context, command, expected string) error {
	t.buffer = ""
	if err := t.write(ctx, runner.Frame{Type: "input", LeaseID: t.lease, Data: []byte(command)}); err != nil {
		return err
	}
	return t.await(ctx, expected)
}

func (t *terminal) await(ctx context.Context, expected string) error {
	read, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for {
		var frame runner.Frame
		if err := wsjson.Read(read, t.connection, &frame); err != nil {
			return fmt.Errorf("terminal output deadline or connection failure")
		}
		switch frame.Type {
		case "output":
			if frame.Sequence != nil {
				t.sequence = *frame.Sequence
				if err := t.write(ctx, runner.Frame{Type: "ack", Sequence: frame.Sequence}); err != nil {
					return err
				}
			}
			t.buffer += strings.ReplaceAll(string(frame.Data), "\r", "")
			if strings.Contains(t.buffer, "\n"+expected+"\n") {
				return nil
			}
			if len(t.buffer) > 128*1024 {
				t.buffer = t.buffer[len(t.buffer)-64*1024:]
			}
		case "ping":
			if err := t.write(ctx, runner.Frame{Type: "pong", Nonce: frame.Nonce}); err != nil {
				return err
			}
		case "error", "exit", "controller_revoked", "replay_gap":
			return fmt.Errorf("unexpected terminal frame: %s", frame.Type)
		}
	}
}

func workspaceUID(ctx context.Context, client dynamic.Interface, namespace, id string) (types.UID, error) {
	list, err := client.Resource(cluster.PVCGVR).Namespace(namespace).List(ctx, metav1.ListOptions{LabelSelector: cluster.AgentIDLabel + "=" + id})
	if err != nil || len(list.Items) != 1 {
		return "", fmt.Errorf("test agent must have exactly one workspace PVC")
	}
	return list.Items[0].GetUID(), nil
}

// injectCompletion modifies only the disposable Agent's adopted Sandbox while
// stopped. The first new Pod exits successfully; the next starts the real runner.
// Unlike status patching, this exercises actual kubelet/controller behavior.
func injectCompletion(ctx context.Context, client dynamic.Interface, namespace, id string) error {
	resource := client.Resource(cluster.SandboxGVR).Namespace(namespace)
	sandbox, err := allocatedSandbox(ctx, client, namespace, id)
	if err != nil {
		return err
	}
	mode, _, _ := unstructured.NestedString(sandbox.Object, "spec", "operatingMode")
	containers, _, _ := unstructured.NestedSlice(sandbox.Object, "spec", "podTemplate", "spec", "containers")
	if mode != "Suspended" || len(containers) != 1 {
		return fmt.Errorf("recovery fixture requires a stopped, single-container shell sandbox")
	}
	container := containers[0].(map[string]any)
	command, _, _ := unstructured.NestedSlice(container, "command")
	arguments, _, _ := unstructured.NestedSlice(container, "args")
	if len(command) == 0 || command[0] != "/usr/local/bin/sandherd-runner" {
		return fmt.Errorf("recovery fixture requires the standard runner")
	}
	script := "if [ ! -f /workspace/repository/.sandherd-smoke-recovery ]; then sleep 3; printf '%s' completed-pod-recovered > /workspace/repository/.sandherd-smoke-recovery; exit 0; fi; exec \"$@\""
	container["command"] = []any{"/bin/sh"}
	container["args"] = append(append([]any{"-c", script, "smoke-recovery"}, command...), arguments...)
	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"resourceVersion": sandbox.GetResourceVersion()},
		"spec":     map[string]any{"podTemplate": map[string]any{"spec": map[string]any{"restartPolicy": "Never", "containers": containers}}},
	})
	if err != nil {
		return err
	}
	if _, err := resource.Patch(ctx, sandbox.GetName(), types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("cannot inject completed-Pod fixture")
	}
	return nil
}

func allocatedSandbox(ctx context.Context, client dynamic.Interface, namespace, id string) (*unstructured.Unstructured, error) {
	claimName := "sandbox-" + strings.ReplaceAll(id, "-", "")
	claim, err := client.Resource(cluster.SandboxClaimGVR).Namespace(namespace).Get(ctx, claimName, metav1.GetOptions{})
	if err != nil || claim.GetLabels()[cluster.AgentIDLabel] != id || claim.GetUID() == "" {
		return nil, fmt.Errorf("cannot identify the disposable claim")
	}
	name, _, _ := unstructured.NestedString(claim.Object, "status", "sandbox", "name")
	if name == "" {
		return nil, fmt.Errorf("disposable claim has no sandbox")
	}
	sandbox, err := client.Resource(cluster.SandboxGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("cannot read the disposable sandbox")
	}
	// additionalPodMetadata labels the Pod, not the Sandbox. Verify the
	// controller owner reference back to the Agent's claim instead.
	for _, owner := range sandbox.GetOwnerReferences() {
		if owner.APIVersion == "extensions.agents.x-k8s.io/v1beta1" && owner.Kind == "SandboxClaim" && owner.Name == claim.GetName() && owner.UID == claim.GetUID() && owner.Controller != nil && *owner.Controller {
			return sandbox, nil
		}
	}
	return nil, fmt.Errorf("sandbox is not owned by the disposable claim")
}
