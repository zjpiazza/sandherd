package chart

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	yamlutil "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"
)

func render(t *testing.T, overrides ...string) []*unstructured.Unstructured {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("Helm is required by make chart-check and CI")
	}
	args := []string{"template", "test", "../../charts/sandherd", "--namespace", "chart-verify", "--include-crds", "--set", "network.apiServerCIDRs={10.96.0.1/32,172.18.0.2/32}"}
	for _, value := range overrides {
		args = append(args, "--set", value)
	}
	output, err := exec.Command("helm", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("render: %v\n%s", err, output)
	}
	decoder := yamlutil.NewYAMLOrJSONDecoder(bytes.NewReader(output), 4096)
	var objects []*unstructured.Unstructured
	for {
		var object map[string]any
		err := decoder.Decode(&object)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if len(object) > 0 {
			objects = append(objects, &unstructured.Unstructured{Object: object})
		}
	}
	return objects
}

func TestChartPrivateDefaultsAndBindings(t *testing.T) {
	for _, codex := range []bool{false, true} {
		overrides := []string{"herdr.enabled=true"}
		if codex {
			overrides = append(overrides, "codex.enabled=true")
		}
		objects := render(t, overrides...)
		pools := map[string]bool{}
		var registry map[string]any
		for _, object := range objects {
			kind, name := object.GetKind(), object.GetName()
			if kind != "CustomResourceDefinition" && kind != "ClusterRoleBinding" && object.GetNamespace() != "chart-verify" {
				t.Fatalf("resource escaped namespace: %s %s", kind, name)
			}
			if kind == "Secret" {
				t.Fatal("chart must not put credentials in release state")
			}
			if kind == "Service" {
				serviceType, _, _ := unstructured.NestedString(object.Object, "spec", "type")
				if serviceType == "LoadBalancer" || serviceType == "NodePort" {
					t.Fatalf("public service by default: %s", name)
				}
				if name == "sandherd-codex-auth" {
					publish, _, _ := unstructured.NestedBool(object.Object, "spec", "publishNotReadyAddresses")
					if !publish {
						t.Fatal("credential failures would disappear from Service endpoints")
					}
				}
			}
			if kind == "SandboxWarmPool" {
				pools[name] = true
			}
			if kind == "Role" {
				rules, _, _ := unstructured.NestedSlice(object.Object, "rules")
				for _, raw := range rules {
					rule := raw.(map[string]any)
					resources := rule["resources"].([]any)
					for _, resource := range resources {
						if resource == "secrets" || resource == "*" {
							t.Fatal("excessive control-plane permissions")
						}
					}
				}
			}
			if kind == "PersistentVolumeClaim" && object.GetAnnotations()["helm.sh/resource-policy"] != "keep" {
				t.Fatalf("stateful PVC would be removed: %s", name)
			}
			if kind == "ConfigMap" && name == "sandherd-adapter-config" {
				data, _, _ := unstructured.NestedString(object.Object, "data", "adapters.json")
				if err := json.Unmarshal([]byte(data), &registry); err != nil {
					t.Fatal(err)
				}
				if strings.Contains(data, "openrouter") {
					t.Fatal("chart advertises an uninstalled credential binding")
				}
			}
			var pod map[string]any
			if kind == "Deployment" {
				pod, _, _ = unstructured.NestedMap(object.Object, "spec", "template", "spec")
			}
			if kind == "SandboxTemplate" {
				pod, _, _ = unstructured.NestedMap(object.Object, "spec", "podTemplate", "spec")
			}
			if pod == nil {
				continue
			}
			if kind == "SandboxTemplate" && pod["automountServiceAccountToken"] != false {
				t.Fatal("sandbox receives Kubernetes token")
			}
			containers, _, _ := unstructured.NestedSlice(pod, "containers")
			init, _, _ := unstructured.NestedSlice(pod, "initContainers")
			for _, raw := range append(containers, init...) {
				container := raw.(map[string]any)
				if !strings.Contains(container["image"].(string), "@sha256:") {
					t.Fatal("mutable image in chart")
				}
				readonly, _, _ := unstructured.NestedBool(container, "securityContext", "readOnlyRootFilesystem")
				escalation, _, _ := unstructured.NestedBool(container, "securityContext", "allowPrivilegeEscalation")
				if !readonly || escalation {
					t.Fatalf("unsafe container security: %s", name)
				}
			}
		}
		for _, raw := range registry["adapters"].([]any) {
			adapter := raw.(map[string]any)
			if !codex && adapter["id"] == "codex" {
				t.Fatal("disabled Codex advertised")
			}
			for _, raw := range adapter["profiles"].([]any) {
				profile := raw.(map[string]any)
				if !pools[profile["warmPool"].(string)] {
					t.Fatal("adapter references missing pool")
				}
			}
		}
	}
}

func TestChartRuntimeAndTailscaleRemainExplicit(t *testing.T) {
	objects := render(t, "codex.enabled=true", "runtime.className=gvisor", "runtime.nodeSelector.sandherd-runtime=gvisor", "tailscale.enabled=true")
	exposed := 0
	for _, object := range objects {
		if object.GetKind() == "SandboxTemplate" {
			name, _, _ := unstructured.NestedString(object.Object, "spec", "podTemplate", "spec", "runtimeClassName")
			if name != "gvisor" {
				t.Fatal("runtime profile silently fell back")
			}
		}
		if object.GetKind() == "Service" {
			class, _, _ := unstructured.NestedString(object.Object, "spec", "loadBalancerClass")
			if class != "" {
				exposed++
				if class != "tailscale" || object.GetName() != "sandherd-control-plane-tailnet" {
					t.Fatal("unexpected public endpoint")
				}
			}
		}
	}
	if exposed != 1 {
		t.Fatalf("tailnet endpoints=%d", exposed)
	}
}

func TestChartRejectsUnsafeOrUnavailableConfiguration(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("Helm is required by make chart-check and CI")
	}
	for _, override := range []string{"images.controlPlane=example/control-plane:latest", "runtime.className=gvisor", "herdr.enabled=true,herdr.agentKind=codex", "herdr.enabled=true,herdr.credentialProfile=chatgpt-subscription", "router.replicas=0"} {
		output, err := exec.Command("helm", "template", "test", "../../charts/sandherd", "--set", "network.cilium=true", "--set", override).CombinedOutput()
		if err == nil {
			t.Fatalf("invalid configuration accepted: %s\n%s", override, output)
		}
	}
	if err := exec.Command("helm", "template", "test", "../../charts/sandherd").Run(); err == nil {
		t.Fatal("missing API network policy configuration accepted")
	}
}

func TestChartMatchesReleaseImageLock(t *testing.T) {
	contents, err := os.ReadFile("../../release/images.lock.json")
	if err != nil {
		t.Fatal(err)
	}
	var lock struct {
		Version string `json:"version"`
		Images  []struct {
			Component string `json:"component"`
			Image     string `json:"image"`
		} `json:"images"`
	}
	if err := json.Unmarshal(contents, &lock); err != nil {
		t.Fatal(err)
	}
	names := map[string]string{"control-plane": "controlPlane", "codex-runtime": "codexRuntime", "agent-runtime": "agentRuntime", "herdr-gateway": "herdrGateway", "agent-sandbox-router": "router"}
	values, err := os.ReadFile("../../charts/sandherd/values.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Images map[string]string `json:"images"`
	}
	if err := yaml.Unmarshal(values, &parsed); err != nil {
		t.Fatal(err)
	}
	for _, image := range lock.Images {
		if parsed.Images[names[image.Component]] != image.Image {
			t.Fatalf("chart image differs from locked release: %s", image.Component)
		}
	}
	metadata, err := os.ReadFile("../../charts/sandherd/Chart.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var chart struct {
		Version    string `json:"version"`
		AppVersion string `json:"appVersion"`
	}
	if err := yaml.Unmarshal(metadata, &chart); err != nil {
		t.Fatal(err)
	}
	if chart.Version != lock.Version || chart.AppVersion != lock.Version {
		t.Fatal("chart version differs from release lock")
	}
}
