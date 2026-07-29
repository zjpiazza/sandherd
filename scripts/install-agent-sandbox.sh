#!/bin/sh
set -eu

repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
kubectl_bin=${KUBECTL:-kubectl}
context=${KUBE_CONTEXT:-admin@homelab}
router_image=${ROUTER_IMAGE:-}
dry_run=${DRY_RUN:-}
platform_overlay=${PLATFORM_OVERLAY:-$repo_root/deploy/agent-sandbox/overlays/homelab}
rendered=$(mktemp)
configured=$(mktemp)

cleanup() {
  rm -f "$rendered" "$configured"
}
trap cleanup EXIT HUP INT TERM

if [ -z "$router_image" ]; then
  printf '%s\n' 'ROUTER_IMAGE must name a router image reachable by the cluster.' >&2
  printf '%s\n' 'See build/agent-sandbox-router/README.md for the pinned build.' >&2
  exit 2
fi

case "$router_image" in
  *[!A-Za-z0-9_./:@+-]*)
    printf '%s\n' 'ROUTER_IMAGE contains unsupported characters.' >&2
    exit 2
    ;;
esac

if ! "$kubectl_bin" config get-contexts "$context" >/dev/null 2>&1; then
  printf 'Kubernetes context does not exist: %s\n' "$context" >&2
  exit 2
fi

"$kubectl_bin" kustomize "$platform_overlay" >"$rendered"
awk -v image="$router_image" '
  $1 == "image:" && $2 == "sandherd/agent-sandbox-router:v0.5.3" {
    print "        image: " image
    next
  }
  { print }
' "$rendered" >"$configured"

if grep -q 'image: sandherd/agent-sandbox-router:v0.5.3' "$configured"; then
  printf '%s\n' 'Failed to replace the router image in the rendered platform.' >&2
  exit 1
fi

if [ -n "$dry_run" ] && [ "$dry_run" != client ] && [ "$dry_run" != server ]; then
  printf '%s\n' 'DRY_RUN, when set, must be client or server.' >&2
  exit 2
fi

if [ "$dry_run" = client ]; then
  "$kubectl_bin" --context "$context" apply \
    --dry-run=client \
    --filename "$configured"
  printf 'Agent Sandbox client-side dry run passed on context %s.\n' "$context"
  exit 0
elif [ "$dry_run" = server ]; then
  "$kubectl_bin" --context "$context" apply \
    --server-side \
    --dry-run=server \
    --field-manager=sandherd-platform \
    --filename "$configured"
  printf 'Agent Sandbox server-side dry run passed on context %s.\n' "$context"
  exit 0
else
  "$kubectl_bin" --context "$context" apply \
    --server-side \
    --field-manager=sandherd-platform \
    --filename "$configured"
fi

for crd in \
  sandboxes.agents.x-k8s.io \
  sandboxclaims.extensions.agents.x-k8s.io \
  sandboxtemplates.extensions.agents.x-k8s.io \
  sandboxwarmpools.extensions.agents.x-k8s.io; do
  "$kubectl_bin" --context "$context" wait \
    --for=condition=Established \
    "customresourcedefinition/$crd" \
    --timeout=120s
done

"$kubectl_bin" --context "$context" --namespace agent-sandbox-system rollout status \
  deployment/agent-sandbox-controller --timeout=180s
"$kubectl_bin" --context "$context" --namespace agent-sandbox-system rollout status \
  deployment/sandbox-router --timeout=180s

printf 'Agent Sandbox platform is ready on context %s.\n' "$context"
