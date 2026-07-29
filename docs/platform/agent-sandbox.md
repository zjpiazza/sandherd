# Agent Sandbox cluster foundation

Sandherd pins Kubernetes SIGs Agent Sandbox v0.5.3 at commit
`f9cf22a0936390a120353380fed80392278de6b6`. The controller image is also
pinned by multi-platform digest. The installation enables the v1beta1
extensions API used by `SandboxTemplate`, `SandboxWarmPool`, and
`SandboxClaim`.

## What is installed

- The Agent Sandbox controller, conversion webhook, CRDs, and extensions.
- Two authenticated sandbox-router replicas in `agent-sandbox-system`.
- The `sandherd-system` namespace and least-privilege service accounts.
- Network policies for controller API access, router ingress, DNS, and the
  sandbox runner port.
- No public ingress and no Tailscale sidecar in any sandbox.

The router uses Kubernetes TokenReview and rejects missing or invalid bearer
tokens. TokenReview v1 authenticates a Kubernetes identity but does not perform
per-sandbox authorization. Sandherd therefore also restricts router ingress to
pods labelled `app.kubernetes.io/name=sandherd-router-client` in the labelled
`sandherd-system` namespace. The later security ticket will add Sandherd's
agent-level authorization policy.

## Router image

Agent Sandbox v0.5.3 publishes router source and deployment examples, but not a
router image. Build the pinned source and push it to a registry reachable by
the cluster:

```sh
export AGENT_SANDBOX_ROUTER_IMAGE=registry.example.com/sandherd/agent-sandbox-router:v0.5.3
make agent-sandbox-router-container
docker push "$AGENT_SANDBOX_ROUTER_IMAGE"
```

The build in `build/agent-sandbox-router` verifies the upstream source archive
checksum and produces a non-root scratch image. No registry credentials are
stored in this repository or in the Kubernetes manifests.

## Install

Every cluster-changing script uses an explicit context. The homelab default is
`admin@homelab`; override it with `KUBE_CONTEXT` for another cluster.

```sh
ROUTER_IMAGE="$AGENT_SANDBOX_ROUTER_IMAGE" \
  ./scripts/install-agent-sandbox.sh
```

Add `DRY_RUN=client` to validate a clean-cluster install without persisting it.
`DRY_RUN=server` provides stronger validation after the namespaces already
exist; Kubernetes does not make a dry-run Namespace visible to later objects in
the same first-install request.

The installation uses server-side apply, waits for all CRDs, then waits for the
controller and router rollouts. Re-running it with the same inputs is safe.

The base Kustomization is portable. The default homelab platform overlay adds
Cilium's `kube-apiserver` entity because service translation prevents a standard
namespace selector from matching Talos host-network API endpoints. Set
`PLATFORM_OVERLAY=deploy/agent-sandbox/base` on a conformant cluster that does not
need that Cilium-specific rule.

Only the smoke-test homelab overlay names `rook-ceph-block`; another cluster can
use the smoke base when it has a default StorageClass or add its own
storage-class patch.

## Smoke test

```sh
./scripts/smoke-agent-sandbox.sh
```

The test:

1. Creates a v1beta1 template, a single-capacity pool, and a router client.
2. Records cold provisioning and claim-acquisition latency.
3. Creates a claim and reaches its health endpoint through the authenticated
   router.
4. Proves missing and invalid bearer tokens receive HTTP 401 and a valid
   identity from an unauthorized namespace cannot connect.
5. Deletes the pool so it cannot replenish during lifecycle validation.
6. Writes a file, recreates the sandbox Pod, and reads the file from the same
   PVC.
7. Confirms DNS works while Kubernetes API and private-node connections fail.
8. Deletes the claim and verifies its Sandbox, Pod, Service, and PVC disappear.

Upstream v1beta1 requires every claim to reference a `SandboxWarmPool`; direct
cold claims no longer exist. The one-replica pool here is compatibility plumbing
for the smoke test, not Sandherd's future warm-pool optimization.

The script traps failures and removes resources labelled
`sandherd.dev/smoke-test=true`.

## Flux

`deploy/agent-sandbox/flux` demonstrates a Flux `GitRepository` pinned to the
upstream commit and a Helm release with CRD upgrades and extensions enabled.
Reconcile that directory for the controller, and reconcile
`deploy/agent-sandbox/platform-overlays/homelab` from the Sandherd source for
namespaces, router, RBAC, and Cilium policy without duplicating the controller.
Other clusters can reconcile `deploy/agent-sandbox/platform` and compose an
environment overlay. Patch the router image in that Sandherd Kustomization to
your published reference.

## Removal

Removal is deliberately guarded because it deletes cluster-wide CRDs and the
entire `sandherd-system` namespace. It refuses to proceed while any Agent
Sandbox custom resources exist.

```sh
CONFIRM_REMOVE_AGENT_SANDBOX=yes ./scripts/uninstall-agent-sandbox.sh
```

Deleting the platform is recoverable only by reinstalling it. Any workspaces
must be backed up before their claims are deleted.
