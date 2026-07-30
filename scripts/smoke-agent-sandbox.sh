#!/bin/sh
set -eu

repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
kubectl_bin=${KUBECTL:-kubectl}
context=${KUBE_CONTEXT:-admin@homelab}
namespace=${SANDBOX_NAMESPACE:-sandherd-system}
smoke_overlay=${SMOKE_OVERLAY:-$repo_root/deploy/agent-sandbox/smoke/overlays/homelab}
timeout_seconds=${SMOKE_TIMEOUT_SECONDS:-300}
claim_name="sandherd-smoke-$(date -u +%Y%m%d%H%M%S)"
sandbox_name=
pvc_name=

k() {
  "$kubectl_bin" --context "$context" "$@"
}

cleanup() {
  exit_code=$?
  trap - EXIT HUP INT TERM
  set +e
  if [ -n "$claim_name" ]; then
    k --namespace "$namespace" delete sandboxclaim "$claim_name" \
      --ignore-not-found=true --wait=true >/dev/null 2>&1
  fi
  k delete --kustomize "$smoke_overlay" --ignore-not-found=true --wait=true \
    >/dev/null 2>&1
  k --namespace "$namespace" delete sandbox,pod,service,persistentvolumeclaim \
    --selector=sandherd.dev/smoke-test=true \
    --ignore-not-found=true --wait=true >/dev/null 2>&1
  if [ "$exit_code" -ne 0 ]; then
    printf '%s\n' 'Smoke test failed; smoke resources were cleaned up.' >&2
  fi
  exit "$exit_code"
}
trap cleanup EXIT HUP INT TERM

wait_for_pool() {
  deadline=$(( $(date +%s) + timeout_seconds ))
  while [ "$(date +%s)" -lt "$deadline" ]; do
    ready=$(k --namespace "$namespace" get sandboxwarmpool sandherd-smoke \
      --output=jsonpath='{.status.readyReplicas}' 2>/dev/null || true)
    if [ "${ready:-0}" -ge 1 ] 2>/dev/null; then
      return 0
    fi
    sleep 2
  done
  printf '%s\n' 'Timed out waiting for the smoke warm pool.' >&2
  return 1
}

wait_for_recreated_pod() {
  old_uid=$1
  deadline=$(( $(date +%s) + timeout_seconds ))
  while [ "$(date +%s)" -lt "$deadline" ]; do
    new_uid=$(k --namespace "$namespace" get pod "$sandbox_name" \
      --output=jsonpath='{.metadata.uid}' 2>/dev/null || true)
    ready=$(k --namespace "$namespace" get pod "$sandbox_name" \
      --output=jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || true)
    if [ -n "$new_uid" ] && [ "$new_uid" != "$old_uid" ] && [ "$ready" = True ]; then
      return 0
    fi
    sleep 2
  done
  printf '%s\n' 'Timed out waiting for sandbox pod recreation.' >&2
  return 1
}

for deployment in agent-sandbox-controller sandbox-router; do
  k --namespace agent-sandbox-system rollout status "deployment/$deployment" --timeout=30s
done

cold_start=$(date +%s)
k apply --kustomize "$smoke_overlay"
k --namespace "$namespace" wait --for=condition=Ready pod/sandherd-router-client \
  --timeout="${timeout_seconds}s"
k --namespace default wait --for=condition=Ready pod/sandherd-unauthorized-router-client \
  --timeout="${timeout_seconds}s"
wait_for_pool
cold_seconds=$(( $(date +%s) - cold_start ))
printf 'Cold sandbox pool became ready in %ss.\n' "$cold_seconds"

claim_start=$(date +%s)
k apply --filename - <<EOF
apiVersion: extensions.agents.x-k8s.io/v1beta1
kind: SandboxClaim
metadata:
  name: $claim_name
  namespace: $namespace
  labels:
    app.kubernetes.io/name: sandherd-smoke
    app.kubernetes.io/part-of: sandherd
    sandherd.dev/smoke-test: "true"
spec:
  warmPoolRef:
    name: sandherd-smoke
  lifecycle:
    shutdownPolicy: DeleteForeground
EOF

k --namespace "$namespace" wait --for=condition=Ready \
  "sandboxclaim/$claim_name" --timeout="${timeout_seconds}s"
claim_seconds=$(( $(date +%s) - claim_start ))
sandbox_name=$(k --namespace "$namespace" get sandboxclaim "$claim_name" \
  --output=jsonpath='{.status.sandbox.name}')
if [ -z "$sandbox_name" ]; then
  printf '%s\n' 'Ready claim did not report a sandbox name.' >&2
  exit 1
fi
printf 'Claim %s acquired sandbox %s in %ss.\n' "$claim_name" "$sandbox_name" "$claim_seconds"

# Prevent the pool from replenishing after checkout. The adopted sandbox is now
# owned by the claim and remains available for the persistence test.
k --namespace "$namespace" delete sandboxwarmpool sandherd-smoke --wait=true

router_url=http://sandbox-router-svc.agent-sandbox-system.svc.cluster.local:8080/agent-health
missing_code=$(k --namespace "$namespace" exec sandherd-router-client -- \
  curl --silent --output /dev/null --write-out '%{http_code}' \
  --header "X-Sandbox-ID: $sandbox_name" \
  --header "X-Sandbox-Namespace: $namespace" \
  --header 'X-Sandbox-Port: 8888' "$router_url")
if [ "$missing_code" != 401 ]; then
  printf 'Router returned %s without a bearer token; expected 401.\n' "$missing_code" >&2
  exit 1
fi

invalid_code=$(k --namespace "$namespace" exec sandherd-router-client -- \
  curl --silent --output /dev/null --write-out '%{http_code}' \
  --header 'Authorization: Bearer invalid' \
  --header "X-Sandbox-ID: $sandbox_name" \
  --header "X-Sandbox-Namespace: $namespace" \
  --header 'X-Sandbox-Port: 8888' "$router_url")
if [ "$invalid_code" != 401 ]; then
  printf 'Router returned %s for an invalid bearer token; expected 401.\n' "$invalid_code" >&2
  exit 1
fi

# The single-quoted variables expand inside the client Pod.
# shellcheck disable=SC2016
health=$(k --namespace "$namespace" exec sandherd-router-client -- /bin/sh -ec '
  token=$(cat /var/run/secrets/kubernetes.io/serviceaccount/token)
  exec curl --silent --show-error --fail \
    --header "Authorization: Bearer ${token}" \
    --header "X-Sandbox-ID: $1" \
    --header "X-Sandbox-Namespace: $2" \
    --header "X-Sandbox-Port: 8888" "$3"
' sh "$sandbox_name" "$namespace" "$router_url")
if [ "$health" != ok ]; then
  printf 'Unexpected sandbox health response: %s\n' "$health" >&2
  exit 1
fi
printf '%s\n' 'Authenticated router request reached the sandbox.'

# The single-quoted variables expand inside the unauthorized client Pod.
# shellcheck disable=SC2016
if k --namespace default exec sandherd-unauthorized-router-client -- /bin/sh -ec '
  token=$(cat /var/run/secrets/kubernetes.io/serviceaccount/token)
  exec curl --silent --show-error --fail --connect-timeout 3 \
    --header "Authorization: Bearer ${token}" \
    --header "X-Sandbox-ID: $1" \
    --header "X-Sandbox-Namespace: $2" \
    --header "X-Sandbox-Port: 8888" "$3"
' sh "$sandbox_name" "$namespace" "$router_url" >/dev/null 2>&1; then
  printf '%s\n' 'A client in an unauthorized namespace reached the router.' >&2
  exit 1
fi
printf '%s\n' 'Router ingress rejected a valid identity from an unauthorized namespace.'

k --namespace "$namespace" exec "$sandbox_name" --container runner -- \
  /bin/sh -ec "printf '%s\\n' '$claim_name' >/workspace/persistence.txt"
pvc_name=$(k --namespace "$namespace" get pod "$sandbox_name" \
  --output=jsonpath='{.spec.volumes[?(@.name=="workspace")].persistentVolumeClaim.claimName}')
old_uid=$(k --namespace "$namespace" get pod "$sandbox_name" \
  --output=jsonpath='{.metadata.uid}')
k --namespace "$namespace" delete pod "$sandbox_name" --wait=true
wait_for_recreated_pod "$old_uid"
persisted=$(k --namespace "$namespace" exec "$sandbox_name" --container runner -- \
  cat /workspace/persistence.txt)
if [ "$persisted" != "$claim_name" ]; then
  printf 'PVC persistence mismatch: %s\n' "$persisted" >&2
  exit 1
fi
printf 'PVC %s survived sandbox pod recreation.\n' "$pvc_name"

k --namespace "$namespace" exec "$sandbox_name" --container runner -- \
  nslookup kubernetes.default.svc.cluster.local >/dev/null
if k --namespace "$namespace" exec "$sandbox_name" --container runner -- \
  nc -z -w 2 kubernetes.default.svc.cluster.local 443 >/dev/null 2>&1; then
  printf '%s\n' 'Sandbox unexpectedly reached the Kubernetes API service.' >&2
  exit 1
fi
node_ip=$(k get nodes --output=jsonpath='{.items[0].status.addresses[?(@.type=="InternalIP")].address}')
if k --namespace "$namespace" exec "$sandbox_name" --container runner -- \
  nc -z -w 2 "$node_ip" 6443 >/dev/null 2>&1; then
  printf 'Sandbox unexpectedly reached private node %s.\n' "$node_ip" >&2
  exit 1
fi
for blocked_target in 169.254.169.254 100.100.100.100; do
  if k --namespace "$namespace" exec "$sandbox_name" --container runner -- \
    nc -z -w 2 "$blocked_target" 443 >/dev/null 2>&1; then
    printf 'Sandbox unexpectedly reached protected address %s.\n' "$blocked_target" >&2
    exit 1
  fi
done
printf '%s\n' 'Sandbox DNS works while Kubernetes API, node, metadata, and tailnet access are blocked.'

k --namespace "$namespace" delete sandboxclaim "$claim_name" --wait=true
k --namespace "$namespace" wait --for=delete "sandbox/$sandbox_name" \
  --timeout="${timeout_seconds}s"
k --namespace "$namespace" wait --for=delete "pod/$sandbox_name" \
  --timeout="${timeout_seconds}s"
if [ -n "$pvc_name" ]; then
  k --namespace "$namespace" wait --for=delete "persistentvolumeclaim/$pvc_name" \
    --timeout="${timeout_seconds}s"
fi
claim_name=

remaining=$(k --namespace "$namespace" get sandbox,pod,service,persistentvolumeclaim \
  --selector=sandherd.dev/smoke-test=true --output=name | \
  grep -v '^pod/sandherd-router-client$' || true)
if [ -n "$remaining" ]; then
  printf '%s\n' 'Smoke workload resources remain after claim deletion.' >&2
  printf '%s\n' "$remaining" >&2
  exit 1
fi

printf '%s\n' 'Claim shutdown removed its sandbox, Pod, Service, and PVC.'
printf '%s\n' 'Agent Sandbox smoke test passed.'
