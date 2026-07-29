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

printf '%s\n' 'Agent Sandbox platform manifests are valid.'
