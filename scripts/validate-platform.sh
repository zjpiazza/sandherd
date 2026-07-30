#!/bin/sh
set -eu

repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
kustomize_version=${KUSTOMIZE_VERSION:-v5.8.1}
rendered=$(mktemp)

cleanup() {
  rm -f "$rendered"
}
trap cleanup EXIT HUP INT TERM

kustomize() {
  go run "sigs.k8s.io/kustomize/kustomize/v5@${kustomize_version}" build "$1"
}

kustomize "$repo_root/deploy/agent-sandbox/base" >"$rendered"
kustomize "$repo_root/deploy/agent-sandbox/overlays/homelab" >/dev/null
kustomize "$repo_root/deploy/agent-sandbox/platform-overlays/homelab" >/dev/null

if grep -q 'ko://' "$rendered"; then
  printf '%s\n' 'controller image replacement left an unresolved ko:// image' >&2
  exit 1
fi

for required in \
  'kind: CustomResourceDefinition' \
  'name: agent-sandbox-controller' \
  'name: agent-sandbox-config' \
  'allowed-label-domains:' \
  'sandherd.dev' \
  'name: sandbox-router' \
  --authz-mode=tokenreview \
  --authz-tokenreview-require-token=true \
  'name: sandherd-system'; do
  if ! grep -q -- "$required" "$rendered"; then
    printf 'rendered platform is missing: %s\n' "$required" >&2
    exit 1
  fi
done

if ! grep -q 'agent-sandbox-controller@sha256:ba381b4e0c86cca597d5c5a31860e38d30ec1c45e0a7a8328bb2799c87d059c0' "$rendered"; then
  printf '%s\n' 'controller image is not pinned to the expected v0.5.3 digest' >&2
  exit 1
fi

kustomize "$repo_root/deploy/agent-sandbox/smoke/base" >/dev/null
kustomize "$repo_root/deploy/agent-sandbox/smoke/overlays/homelab" >/dev/null
kustomize "$repo_root/deploy/agent-sandbox/flux" >/dev/null
kustomize "$repo_root/deploy/agent-runtime/base" >"$rendered"
for required in \
  'name: sandherd-standard' \
  'name: workspace-bootstrap' \
  'workingDir: /workspace' \
  'volumeClaimTemplatesPolicy: Allowed' \
  'automountServiceAccountToken: false' \
  'name: capability-public-key' \
  'cidr: 0.0.0.0/0' \
  '10.0.0.0/8' \
  '100.64.0.0/10' \
  '169.254.0.0/16' \
  '192.168.0.0/16'; do
  if ! grep -q -- "$required" "$rendered"; then
    printf 'rendered agent runtime is missing: %s\n' "$required" >&2
    exit 1
  fi
done
if grep -q 'hostPath:' "$rendered"; then
  printf '%s\n' 'agent runtime must not mount host paths' >&2
  exit 1
fi
kustomize "$repo_root/deploy/agent-runtime/examples/https-credential-profile" >"$rendered"
if awk '/^      containers:/{inside=1} /^      initContainers:/{inside=0} inside' "$rendered" | grep -q 'repository-credential'; then
  printf '%s\n' 'repository bootstrap credentials must not be mounted into the runner container' >&2
  exit 1
fi
kustomize "$repo_root/deploy/agent-runtime/examples/ssh-credential-profile" >/dev/null
kustomize "$repo_root/deploy/agent-runtime/codex" >"$rendered"
for required in \
  'name: sandherd-codex' \
  'sandherd.dev/adapter-id: codex' \
  'name: credential-bootstrap' \
  'name: credential-sync' \
  'name: CODEX_HOME' \
  'value: /home/sandherd/.codex' \
  'name: sandherd-codex-auth' \
  'port: 8090' \
  'automountServiceAccountToken: false'; do
  if ! grep -q -- "$required" "$rendered"; then
    printf 'rendered Codex runtime is missing: %s\n' "$required" >&2
    exit 1
  fi
done
if grep -Eq 'claimName: sandherd-codex-auth|kind: Secret|refresh_token' "$rendered"; then
  printf '%s\n' 'Codex sandbox runtime exposes coordinator credential authority' >&2
  exit 1
fi
kustomize "$repo_root/deploy/control-plane" >"$rendered"
kustomize "$repo_root/deploy/control-plane-homelab" >"$rendered"

for required in \
  'name: agents.sandherd.dev' \
  'name: sandherd-control-plane' \
  'resources:' \
  'sandboxclaims' \
  'name: sandherd-api-principals' \
  'path: principals.json' \
  'name: sandherd-control-plane-tailnet' \
  'loadBalancerClass: tailscale' \
  'kubernetes.io/metadata.name: tailscale' \
  'name: sandherd-codex-auth' \
  'storageClassName: rook-ceph-block' \
  'accessModes:' \
  'ReadWriteOnce' \
  'chatgpt-subscription' \
  '0.146.0'; do
  if ! grep -q -- "$required" "$rendered"; then
    printf 'rendered control plane is missing: %s\n' "$required" >&2
    exit 1
  fi
done

if grep -q -- '--owner-id\|--auth-token-file=/var/run/secrets/sandherd/api-token\|name: sandherd-api-token' "$rendered"; then
  printf '%s\n' 'rendered control plane still uses the legacy shared owner credential' >&2
  exit 1
fi

if grep -A20 'name: sandherd-control-plane$' "$rendered" | grep -q 'resources:.*secrets'; then
  printf '%s\n' 'control-plane Role must not read Kubernetes Secrets' >&2
  exit 1
fi

if grep -A80 'kind: Role' "$rendered" | grep -Eq 'resources: \[(deployments|statefulsets|daemonsets|jobs|pods)\].*create'; then
  printf '%s\n' 'control-plane Role can create arbitrary workloads' >&2
  exit 1
fi

printf '%s\n' 'Agent Sandbox platform manifests are valid.'
