#!/usr/bin/env python3
"""Keep packaged manifest inputs identical to the reviewed Kustomize sources."""
import argparse
import pathlib
import sys

root = pathlib.Path(__file__).resolve().parents[1]
parser = argparse.ArgumentParser()
parser.add_argument('--check', action='store_true')
args = parser.parse_args()
files = {
    'crds/agents.yaml': 'deploy/control-plane/agent-crd.yaml',
    'files/manifests/control-plane.yaml': 'deploy/control-plane/deployment.yaml',
    'files/manifests/rbac.yaml': 'deploy/control-plane/rbac.yaml',
    'files/manifests/control-plane-network.yaml': 'deploy/control-plane/network-policy.yaml',
    'files/manifests/adapters.yaml': 'deploy/control-plane/adapter-config.yaml',
    'files/manifests/standard.yaml': 'deploy/agent-runtime/base/sandbox-template.yaml',
    'files/manifests/codex.yaml': 'deploy/agent-runtime/codex/sandbox-template.yaml',
    'files/manifests/codex-auth.yaml': 'deploy/codex-auth/coordinator.yaml',
    'files/manifests/codex-auth-network.yaml': 'deploy/codex-auth/network-policy.yaml',
    'files/manifests/router.yaml': 'deploy/agent-sandbox/platform/router.yaml',
    'files/manifests/router-rbac.yaml': 'deploy/agent-sandbox/platform/router-rbac.yaml',
    'files/manifests/platform-network.yaml': 'deploy/agent-sandbox/platform/network-policies.yaml',
    'files/manifests/herdr.yaml': 'deploy/herdr-gateway/deployment.yaml',
    'files/manifests/herdr-pvc.yaml': 'deploy/herdr-gateway/pvc.yaml',
    'files/manifests/herdr-service.yaml': 'deploy/herdr-gateway/service.yaml',
    'files/manifests/herdr-network.yaml': 'deploy/herdr-gateway/network-policy.yaml',
    'files/herdr-config.json': 'deploy/herdr-gateway/config.json',
}
failed = False
for destination, source in files.items():
    target = root / 'charts/sandherd' / destination
    content = (root / source).read_bytes()
    if args.check:
        if not target.exists() or target.read_bytes() != content:
            print(f'Chart asset differs from {source}: {destination}', file=sys.stderr)
            failed = True
    else:
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_bytes(content)
sys.exit(1 if failed else 0)
