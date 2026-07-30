# Lifecycle control plane

The Sandherd control plane serves the public v1alpha1 lifecycle and event API
and reconciles every logical Agent to one Kubernetes Agent Sandbox
`SandboxClaim`. Kubernetes names, resource versions, and upstream status details
remain internal.

## Runtime model

The accepted [resource-model ADR](adr/0001-resource-and-api-model.md) is
implemented with a namespaced `agents.sandherd.dev` CRD. Its spec holds the
validated agent request and desired lifecycle state. Its status holds the stable
public state, observed generation, safe reason, and transition timestamps.

Each Agent ID deterministically maps to one claim name. Reconciliation is level
based, so restarting the control plane lists all Agents and adopts existing
healthy claims; it neither restarts nor duplicates them. Agent, claim, sandbox,
and runner-Pod watches trigger reconciliation, with a periodic resync as a
safety net. Repeated transient failures use bounded exponential retries and end
in `failed` after the retry budget is exhausted.

The running path waits for all of the following before publishing `running`:

1. an approved public sandbox profile maps to a configured warm pool;
2. the deterministic `SandboxClaim` exists and has an assigned Sandbox;
3. the claim reports `Ready`;
4. the runner Pod reports `Ready`.

Stop sets the adopted Sandbox operating mode to `Suspended` and does not report
`stopped` until the `Suspended` condition is observed. Resume restores `Running`
mode and waits through the same runner readiness gates. Delete uses the Agent finalizer and foreground
claim deletion. A `retain` workspace has its Kubernetes owner references removed
before claim deletion; a `delete` workspace follows normal cascading cleanup.
Managed claims without an Agent are detected and removed as orphans.

## API behavior

The service implements:

- `POST` and `GET /v1alpha1/agents`
- `GET` and `DELETE /v1alpha1/agents/{id}`
- `POST /v1alpha1/agents/{id}:stop`
- `POST /v1alpha1/agents/{id}:resume`
- `GET /v1alpha1/events` as server-sent events
- `GET /v1alpha1/agents/{id}/terminal` as a reconnectable WebSocket
- `GET /metrics` for content-free gateway telemetry
- `/healthz` and `/readyz`

Create requires `Idempotency-Key`. The owner, key, and canonical request hash
are persisted with the Agent, so a repeat returns the same UUID while a changed
request returns `idempotency_conflict`. Names are unique per owner. Responses
carry an ETag derived from Kubernetes `resourceVersion`; mutations with stale
`If-Match` return `precondition_failed`.

Lists are ordered by time-sortable UUIDv7 ID, support stable opaque cursors, and
filter by public state or exact name. Errors use the OpenAPI error envelope and
never expose Kubernetes messages. Lifecycle events are held in a bounded
in-memory replay buffer for SSE reconnects; terminal bytes are never events.

End-user identity-provider integration remains issue #9. The MVP process uses
one control bearer token, an optional observe-only token, and one stable owner
ID, supplied only through mounted files and command flags. Terminal streams use
the [gateway security and routing model](terminal-gateway.md):

```sh
go run ./cmd/control-plane \
  --context admin@homelab \
  --namespace sandherd-system \
  --owner-id homelab \
  --auth-token-file /run/sandherd/api-token \
  --capability-private-key-file /run/sandherd/capability-private-key.pem \
  --sandbox-router-token-file /var/run/secrets/kubernetes.io/serviceaccount/token \
  --sandbox-profile standard=sandherd-standard
```

The API never accepts a caller-supplied owner header.

## Kubernetes deployment

[`deploy/control-plane`](../deploy/control-plane) contains the Agent CRD,
single-replica deployment, service, network policy, and namespace-scoped RBAC.
The role can manage Sandherd Agents and claims, patch adopted Sandboxes, observe
runner Pods, and release retained PVCs. It cannot create Pods, Deployments,
Jobs, or workloads outside `sandherd-system`.

Before applying the deployment, publish the control-plane image, configure an
approved `SandboxWarmPool`, patch the example image/owner as needed, and create
the API token without committing it:

```sh
kubectl --context admin@homelab --namespace sandherd-system \
  create secret generic sandherd-api-token \
  --from-literal=token='replace-with-a-long-random-token'

# Create the signing key Secret described in docs/terminal-gateway.md too.

kubectl --context admin@homelab apply --kustomize deploy/control-plane-homelab
```

The control plane separately maps public sandbox, storage, and repository
secret profiles to administrator-approved Kubernetes resources:

```text
--sandbox-profile=standard=sandherd-standard
--storage-profile=default=rook-ceph-block
--secret-profile=personal=sandherd-standard-https-private
```

The portable deployment uses the cluster's default StorageClass. The homelab
overlay maps `default` to `rook-ceph-block`. See the [workspace guide](workspaces.md)
for the standard runtime, repository bootstrap, credential isolation, and
retention behavior.

Clusters whose CNI enforces the portable Kubernetes API namespace selector can
apply `deploy/control-plane` directly. The homelab overlay grants the Talos API
endpoint through Cilium's `kube-apiserver` entity after Service translation.

The Pod explicitly mounts its namespace-scoped service-account token because
the control plane is the sole Sandherd component authorized to call Kubernetes
and the authenticated Agent Sandbox router.
Sandbox service accounts continue to set `automountServiceAccountToken: false`.

## Restart and failure semantics

- API process restarts do not change Agent desired state or replace claims.
- Missing claims for live Agents are recreated with the same deterministic name.
- Failed runner Pods and terminal provisioning failures become stable `failed`
  states with safe reasons.
- Deletion stays observable as `deleting` until claim cleanup completes.
- The SSE replay buffer is intentionally process-local for the MVP; Kubernetes
  Agent state remains the durable source of truth.
