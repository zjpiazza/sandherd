# Adapter rebind homelab smoke test

- Date: 2026-07-30
- Context: `admin@homelab`
- Agent Sandbox: v0.5.3
- Control plane image: `ghcr.io/zjpiazza/sandherd-control-plane:issue-25-adapters-1`
- Runtime image: `ghcr.io/zjpiazza/sandherd-agent-runtime:issue-25-adapters-1`

The issue #25 branch was deployed over the existing Sandherd installation. The
additive Agent CRD update, adapter ConfigMap, runtime template, control-plane
RBAC, Deployment rollout, and authenticated adapter discovery all succeeded.
Discovery returned the two enabled development adapters without commands,
health checks, warm-pool names, credential profiles, or Kubernetes names.

A disposable Agent was created with adapter `shell`, runtime generation 1, a
1 GiB `delete` workspace, and no repository. After it reached `running`, a
canary was written to the repository and the PVC identity was recorded:

```text
PVC UID:    a75c744f-7163-4ff8-9a31-642f6a38fd66
PV name:    pvc-a75c744f-7163-4ff8-9a31-642f6a38fd66
Canary:     sandherd-adapter-rebind-canary
```

`POST /v1alpha1/agents/{id}:change-adapter` selected `shell-minimal`. The same
logical Agent entered `reconfiguring`, retained the unchanged WorkspaceSpec,
and reached `running` with adapter `shell-minimal`, adapter version 1, and
runtime generation 2.

The replacement claim carried the target adapter and generation annotations.
The PVC UID and PV name were unchanged, and the new runner read the original
canary. A mount-boundary check also proved that the runner saw its selected
adapter HOME and repository but not `/workspace/.sandherd`.

The disposable Agent was deleted afterward. Its `delete` retention policy
removed the Sandbox, claim, Pod, and test PVC. No test Agents remain.
