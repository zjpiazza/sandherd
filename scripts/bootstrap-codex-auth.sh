#!/bin/sh
set -eu

kubectl_bin=${KUBECTL:-kubectl}
context=${KUBE_CONTEXT:-admin@homelab}
namespace=${SANDHERD_NAMESPACE:-sandherd-system}
auth_file=${CODEX_AUTH_FILE:-}
deployment=sandherd-codex-auth

if ! "$kubectl_bin" config get-contexts "$context" >/dev/null 2>&1; then
  printf 'Kubernetes context does not exist: %s\n' "$context" >&2
  exit 2
fi
if ! "$kubectl_bin" --context "$context" --namespace "$namespace" get deployment "$deployment" >/dev/null 2>&1; then
  printf '%s\n' 'The Codex credential coordinator is not installed.' >&2
  exit 2
fi

if [ -n "$auth_file" ]; then
  if [ ! -f "$auth_file" ]; then
    printf 'Codex credential file does not exist: %s\n' "$auth_file" >&2
    exit 2
  fi
  "$kubectl_bin" --context "$context" --namespace "$namespace" exec --stdin "deployment/$deployment" -- \
    /usr/local/bin/codex-auth import <"$auth_file"
else
  printf '%s\n' 'Starting one operator-authorized ChatGPT device login. No sandbox will receive the refresh token.'
  "$kubectl_bin" --context "$context" --namespace "$namespace" exec --stdin --tty "deployment/$deployment" -- \
    /usr/local/bin/codex login \
      -c 'cli_auth_credentials_store="file"' \
      -c 'forced_login_method="chatgpt"' \
      --device-auth
fi

"$kubectl_bin" --context "$context" --namespace "$namespace" rollout status "deployment/$deployment" --timeout=120s
"$kubectl_bin" --context "$context" --namespace "$namespace" exec "deployment/$deployment" -- \
  /usr/local/bin/codex-auth status
printf 'Codex subscription authentication is ready on context %s.\n' "$context"
