#!/bin/sh
set -eu

repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
kubectl_bin=${KUBECTL:-kubectl}
context=${KUBE_CONTEXT:-admin@homelab}
control_plane_image=${CONTROL_PLANE_IMAGE:-}
agent_runtime_image=${AGENT_RUNTIME_IMAGE:-}
api_token_file=${SANDHERD_API_TOKEN_FILE:-}
private_key_file=${SANDHERD_CAPABILITY_PRIVATE_KEY_FILE:-}
public_key_file=${SANDHERD_CAPABILITY_PUBLIC_KEY_FILE:-}
overlay=${SANDHERD_OVERLAY:-$repo_root/deploy/control-plane-homelab}
dry_run=${DRY_RUN:-}

for value_name in CONTROL_PLANE_IMAGE AGENT_RUNTIME_IMAGE SANDHERD_API_TOKEN_FILE SANDHERD_CAPABILITY_PRIVATE_KEY_FILE SANDHERD_CAPABILITY_PUBLIC_KEY_FILE; do
  eval "value=\${$value_name:-}"
  if [ -z "$value" ]; then
    printf '%s is required.\n' "$value_name" >&2
    exit 2
  fi
done

case "$control_plane_image$agent_runtime_image" in
  *[!A-Za-z0-9_./:@+-]*)
    printf '%s\n' 'Image references contain unsupported characters.' >&2
    exit 2
    ;;
esac

for required_file in "$api_token_file" "$private_key_file" "$public_key_file"; do
  if [ ! -f "$required_file" ]; then
    printf 'Required credential file does not exist: %s\n' "$required_file" >&2
    exit 2
  fi
done

if ! "$kubectl_bin" config get-contexts "$context" >/dev/null 2>&1; then
  printf 'Kubernetes context does not exist: %s\n' "$context" >&2
  exit 2
fi
if ! "$kubectl_bin" --context "$context" get namespace agent-sandbox-system >/dev/null 2>&1; then
  printf '%s\n' 'Agent Sandbox is not installed; run scripts/install-agent-sandbox.sh first.' >&2
  exit 2
fi

rendered=$(mktemp)
configured=$(mktemp)
cleanup() {
  rm -f "$rendered" "$configured"
}
trap cleanup EXIT HUP INT TERM

"$kubectl_bin" kustomize "$overlay" >"$rendered"
awk -v control_plane="$control_plane_image" -v agent_runtime="$agent_runtime_image" '
  $1 == "image:" && $2 == "sandherd/control-plane:dev" {
    print "        image: " control_plane
    next
  }
  $1 == "image:" && $2 == "sandherd/agent-runtime:dev" {
    prefix = substr($0, 1, index($0, "image:") - 1)
    print prefix "image: " agent_runtime
    next
  }
  { print }
' "$rendered" >"$configured"

if grep -Eq 'image: sandherd/(control-plane|agent-runtime):dev' "$configured"; then
  printf '%s\n' 'Failed to replace all development image references.' >&2
  exit 1
fi

if [ -n "$dry_run" ] && [ "$dry_run" != client ] && [ "$dry_run" != server ]; then
  printf '%s\n' 'DRY_RUN, when set, must be client or server.' >&2
  exit 2
fi

apply_args=""
if [ -n "$dry_run" ]; then
  apply_args="--dry-run=$dry_run"
fi

"$kubectl_bin" --context "$context" --namespace sandherd-system create secret generic sandherd-api-token \
  --from-file="token=$api_token_file" --dry-run=client --output=yaml |
  "$kubectl_bin" --context "$context" apply $apply_args --filename=-
"$kubectl_bin" --context "$context" --namespace sandherd-system create secret generic sandherd-runner-capability-signing-key \
  --from-file="private-key.pem=$private_key_file" --dry-run=client --output=yaml |
  "$kubectl_bin" --context "$context" apply $apply_args --filename=-
"$kubectl_bin" --context "$context" --namespace sandherd-system create configmap sandherd-runner-capability-key \
  --from-file="public-key.pem=$public_key_file" --dry-run=client --output=yaml |
  "$kubectl_bin" --context "$context" apply $apply_args --filename=-

if [ -n "$dry_run" ]; then
  "$kubectl_bin" --context "$context" apply --dry-run="$dry_run" --filename="$configured"
  printf 'Sandherd %s dry run passed on context %s.\n' "$dry_run" "$context"
  exit 0
fi

"$kubectl_bin" --context "$context" apply --server-side --field-manager=sandherd-control-plane --filename="$configured"
"$kubectl_bin" --context "$context" wait --for=condition=Established customresourcedefinition/agents.sandherd.dev --timeout=120s
"$kubectl_bin" --context "$context" --namespace sandherd-system rollout status deployment/sandherd-control-plane --timeout=180s
"$kubectl_bin" --context "$context" --namespace sandherd-system get sandboxtemplate/sandherd-standard sandboxwarmpool/sandherd-standard >/dev/null

printf 'Sandherd control plane and standard agent runtime are ready on context %s.\n' "$context"
