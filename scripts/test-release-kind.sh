#!/bin/bash
set -euo pipefail
repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
cd "$repo_root"
cluster_name=sandherd-release-acceptance
context="kind-$cluster_name"
namespace=sandherd-test
for dependency in kind kubectl helm python3 go openssl ssh ssh-keygen; do
  command -v "$dependency" >/dev/null || { printf 'Required command missing: %s\n' "$dependency" >&2; exit 2; }
done
existing_clusters=$(kind get clusters)
while IFS= read -r existing_cluster; do
  if [ "$existing_cluster" = "$cluster_name" ]; then
    printf '%s\n' 'Refusing to reuse or delete an existing acceptance cluster.' >&2
    exit 2
  fi
done <<< "$existing_clusters"
scratch=$(mktemp -d)
forward_pid=
created=false
cleanup() {
  result=$?
  if [ -n "$forward_pid" ]; then kill "$forward_pid" 2>/dev/null || true; fi
  if [ "$created" = true ]; then
    if [ "$result" -ne 0 ]; then
      mkdir -p dist/test-results
      kubectl --context "$context" get pods -A -o wide > dist/test-results/pods.txt 2>&1 || true
      kubectl --context "$context" get events -A --sort-by=.metadata.creationTimestamp > dist/test-results/events.txt 2>&1 || true
    fi
    kind delete cluster --name "$cluster_name"
  fi
  rm -rf "$scratch"
  exit "$result"
}
trap cleanup EXIT
export KUBECONFIG="$scratch/kubeconfig"
created=true
kind create cluster --name "$cluster_name" --kubeconfig "$KUBECONFIG" \
  --image kindest/node:v1.36.4@sha256:099e049362a1526b2db71494e1947aae99bd16290d7c895f2b7ea312e3cbfaed \
  --config deploy/test/kind.yaml --wait 120s
go run sigs.k8s.io/kustomize/kustomize/v5@v5.8.1 build deploy/agent-sandbox/release > "$scratch/foundation.yaml"
kubectl --context "$context" apply --server-side -f "$scratch/foundation.yaml"
kubectl --context "$context" -n agent-sandbox-system rollout status deployment/agent-sandbox-controller --timeout=180s
kubectl --context "$context" create namespace "$namespace"
export KUBE_CONTEXT="$context" SANDHERD_NAMESPACE="$namespace" SANDHERD_TOKEN_FILE="$scratch/api-token"
./scripts/bootstrap-release-credentials.sh
python3 - "$context" "$scratch/values.json" <<'PY'
import json,subprocess,sys
kube=['kubectl','--context',sys.argv[1],'-n','default']
service=json.loads(subprocess.check_output(kube+['get','service','kubernetes','-o','json']))
slices=json.loads(subprocess.check_output(kube+['get','endpointslices','-l','kubernetes.io/service-name=kubernetes','-o','json']))
addresses={service['spec']['clusterIP']}
addresses.update(ip for s in slices['items'] for e in s['endpoints'] for ip in e['addresses'])
with open(sys.argv[2],'w') as out:
 json.dump({'network':{'apiServerCIDRs':[ip+('/128' if ':' in ip else '/32') for ip in sorted(addresses)]},'router':{'replicas':1}},out)
PY
helm package charts/sandherd --destination "$scratch"
shopt -s nullglob
charts=("$scratch"/sandherd-*.tgz)
[ "${#charts[@]}" -eq 1 ] || { printf '%s\n' 'Expected exactly one packaged chart.' >&2; exit 2; }
chart=${charts[0]}
helm install sandherd "$chart" --kube-context "$context" -n "$namespace" -f "$scratch/values.json" --wait --timeout 4m
kubectl --context "$context" -n "$namespace" port-forward service/sandherd-control-plane 18080:8080 > "$scratch/forward.log" 2>&1 &
forward_pid=$!
python3 - <<'PY'
import time,urllib.request
for _ in range(100):
 try:
  urllib.request.urlopen('http://127.0.0.1:18080/healthz',timeout=1).close()
  break
 except OSError:time.sleep(.1)
else:raise SystemExit('API port forward did not start')
PY
mkdir -p dist/test-results
./dist/smoke --base-url http://127.0.0.1:18080 --token-file "$scratch/api-token" \
  --recovery-context "$context" --namespace "$namespace" --cpu 500m --memory 512Mi --timeout 6m | tee dist/test-results/installed-smoke.log
kill "$forward_pid"
wait "$forward_pid" 2>/dev/null || true
forward_pid=
python3 scripts/test-installed-release.py --context "$context" --namespace "$namespace" \
  --chart "$chart" --values "$scratch/values.json" --token-file "$scratch/api-token" \
  --report dist/test-results/release-lifecycle.json --disposable
python3 scripts/test-herdr-release.py --context "$context" --namespace "$namespace" \
  --chart "$chart" --values "$scratch/values.json" --token-file "$scratch/api-token" \
  --report dist/test-results/herdr-lifecycle.json --disposable
