#!/bin/sh
set -eu

repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
kubectl_bin=${KUBECTL:-kubectl}
context=${KUBE_CONTEXT:-admin@homelab}
confirm=${CONFIRM_REMOVE_AGENT_SANDBOX:-}
platform_overlay=${PLATFORM_OVERLAY:-$repo_root/deploy/agent-sandbox/overlays/homelab}
rendered=$(mktemp)

cleanup() {
  rm -f "$rendered"
}
trap cleanup EXIT HUP INT TERM

if [ "$confirm" != "yes" ]; then
  printf '%s\n' 'Removal deletes all Agent Sandbox CRDs and the sandherd-system namespace.' >&2
  printf '%s\n' 'Set CONFIRM_REMOVE_AGENT_SANDBOX=yes to confirm.' >&2
  exit 2
fi

for resource in \
  sandboxes.agents.x-k8s.io \
  sandboxclaims.extensions.agents.x-k8s.io \
  sandboxtemplates.extensions.agents.x-k8s.io \
  sandboxwarmpools.extensions.agents.x-k8s.io; do
  if "$kubectl_bin" --context "$context" get "$resource" --all-namespaces \
    --output=name 2>/dev/null | grep -q .; then
    printf 'Refusing removal while %s resources exist. Delete them first.\n' "$resource" >&2
    exit 1
  fi
done

"$kubectl_bin" kustomize "$platform_overlay" >"$rendered"
"$kubectl_bin" --context "$context" delete \
  --ignore-not-found=true \
  --wait=true \
  --filename "$rendered"

printf 'Agent Sandbox platform was removed from context %s.\n' "$context"
