#!/bin/sh
set -eu

kubectl_bin=${KUBECTL:-kubectl}
context=${KUBE_CONTEXT:-admin@homelab}
failures=0

can_i() {
  identity=$1
  verb=$2
  resource=$3
  namespace=$4
  if [ "$namespace" = cluster ]; then
    "$kubectl_bin" --context "$context" auth can-i "$verb" "$resource" --as="$identity" >/dev/null 2>&1
  else
    "$kubectl_bin" --context "$context" auth can-i "$verb" "$resource" --namespace="$namespace" --as="$identity" >/dev/null 2>&1
  fi
}

expect() {
  expected=$1
  identity=$2
  verb=$3
  resource=$4
  namespace=$5
  if can_i "$identity" "$verb" "$resource" "$namespace"; then
    actual=yes
  else
    actual=no
  fi
  if [ "$actual" != "$expected" ]; then
    printf 'RBAC mismatch: %s can %s %s in %s = %s, want %s\n' "$identity" "$verb" "$resource" "$namespace" "$actual" "$expected" >&2
    failures=$((failures + 1))
  fi
}

control_plane=system:serviceaccount:sandherd-system:sandherd-control-plane
sandbox=system:serviceaccount:sandherd-system:sandherd-sandbox
router=system:serviceaccount:agent-sandbox-system:sandbox-router

for permission in \
  'get agents.sandherd.dev sandherd-system' \
  'list agents.sandherd.dev sandherd-system' \
  'create agents.sandherd.dev sandherd-system' \
  'patch agents/status.sandherd.dev sandherd-system' \
  'create sandboxclaims.extensions.agents.x-k8s.io sandherd-system' \
  'patch sandboxes.agents.x-k8s.io sandherd-system' \
  'get pods sandherd-system' \
  'patch persistentvolumeclaims sandherd-system'; do
  set -- $permission
  expect yes "$control_plane" "$1" "$2" "$3"
done

for permission in \
  'get secrets sandherd-system' \
  'create pods sandherd-system' \
  'delete pods sandherd-system' \
  'create deployments.apps sandherd-system' \
  'get nodes cluster' \
  'get agents.sandherd.dev default'; do
  set -- $permission
  expect no "$control_plane" "$1" "$2" "$3"
done

for permission in \
  'get secrets sandherd-system' \
  'get pods sandherd-system' \
  'create pods sandherd-system' \
  'get nodes cluster' \
  'create tokenreviews.authentication.k8s.io cluster'; do
  set -- $permission
  expect no "$sandbox" "$1" "$2" "$3"
done

expect yes "$router" create tokenreviews.authentication.k8s.io cluster
for permission in \
  'get secrets agent-sandbox-system' \
  'get pods agent-sandbox-system' \
  'create pods agent-sandbox-system' \
  'get nodes cluster'; do
  set -- $permission
  expect no "$router" "$1" "$2" "$3"
done

if [ "$failures" -ne 0 ]; then
  printf '%s RBAC checks failed.\n' "$failures" >&2
  exit 1
fi

printf 'Sandherd service-account RBAC checks passed on context %s.\n' "$context"
