# Install and operate the pre-release

The `0.1.0-alpha.1` bundle is under acceptance testing and is not yet a published
release. The commands below consume the bundle, without requiring source files.
For development, produce that bundle with `python3 scripts/package-release.py`.
The publisher must select a project license before publishing.

## Supported prerequisites

Use Kubernetes 1.32 or newer, a working default StorageClass (or configure
`storage.className`), a CNI that enforces NetworkPolicy, Helm 3, and kubectl.
The recorded clean-cluster test uses Kubernetes 1.36.4 with kind 0.33.0. The
homelab uses Kubernetes 1.36.4, Talos, Cilium and Ceph. These are tested targets;
the minimum version constraint is not a claim that every intermediate version
has been tested. The local kind deployment is a disposable test environment.
Use separate control-plane and worker nodes for network isolation verification:
standard NetworkPolicy permits traffic to an agent Pod's own node. The release
test requires a successful DNS/TCP control probe and a denied Kubernetes API
connection from the actual sandbox. A single-node kind cluster cannot establish
that API-isolation property.

Provision a dedicated namespace. Install only once per namespace; resource names
are intentionally stable. The chart refuses to take ownership of existing
unmanaged resources, so an existing Kustomize deployment requires a separately
planned migration. Do not use Helm ownership overrides on the live homelab.

## Obtain and verify the bundle

After the pre-release is published:

```sh
release=v0.1.0-alpha.1
mkdir sandherd-release
cd sandherd-release
gh release download "$release" --repo zjpiazza/sandherd
sha256sum -c SHA256SUMS
```

The bundle contains the chart, pinned prerequisite manifests, standalone CLI
binaries for amd64/arm64, a credential bootstrap helper, image digest lock,
per-image SBOM/provenance documents, changelog and checksums. Registry tags are
not used as installation inputs. Extract the archive for your architecture:

```sh
tar -xzf sandherd-0.1.0-alpha.1-linux-amd64.tar.gz
```

## Install the Agent Sandbox prerequisite

Choose an explicit context. Review `agent-sandbox-v0.5.3.yaml` before installing:
it owns its controller, CRDs and cluster-scoped RBAC separately from Sandherd.
If you already operate Agent Sandbox, validate compatibility and add
`sandherd.dev` to its allowed label domains instead of applying this bundle over
that installation. Restart its controller after changing a startup-only config.

```sh
export KUBE_CONTEXT=your-cluster
kubectl --context "$KUBE_CONTEXT" apply --server-side -f agent-sandbox-v0.5.3.yaml
kubectl --context "$KUBE_CONTEXT" -n agent-sandbox-system rollout status deployment/agent-sandbox-controller
```

The chart owns a router inside the Sandherd namespace; it does not change or
claim an existing router in another namespace. Its router ServiceAccount has a
namespace-specific ClusterRoleBinding to delegated authentication review.

## Create private credentials and install

The helper requires Python 3 and OpenSSL. It generates a new API token and
Ed25519 signing key, puts credentials directly in Kubernetes Secrets, and saves
only the client token to the specified local file with mode 0600. Credentials
are never Helm values. It refuses to overwrite existing credentials.

```sh
export SANDHERD_NAMESPACE=sandherd-system
export SANDHERD_TOKEN_FILE="$PWD/api-token"
kubectl --context "$KUBE_CONTEXT" create namespace "$SANDHERD_NAMESPACE"
sh ./bootstrap-credentials.sh
```

Copy `values.yaml` to `site-values.yaml` and configure Kubernetes API egress.
For Cilium, set `network.cilium: true`. For other CNIs, put the API Service IP
and actual API endpoint IPs in `network.apiServerCIDRs`, using `/32` for IPv4 or
`/128` for IPv6. Network policies can see destinations before or after Service
translation, depending on the CNI. Obtain the addresses with:

```sh
kubectl --context "$KUBE_CONTEXT" -n default get service kubernetes -o wide
kubectl --context "$KUBE_CONTEXT" -n default get endpointslices -l kubernetes.io/service-name=kubernetes -o yaml
```

An empty API egress configuration is rejected. Do not broaden sandbox policies
or add an unrestricted egress rule to make installation pass.

```sh
helm install sandherd ./sandherd-0.1.0-alpha.1.tgz \
  --kube-context "$KUBE_CONTEXT" --namespace "$SANDHERD_NAMESPACE" \
  -f site-values.yaml --wait --timeout 5m
kubectl --context "$KUBE_CONTEXT" -n "$SANDHERD_NAMESPACE" \
  port-forward service/sandherd-control-plane 8080:8080
```

In another terminal, run the standalone first-agent check. It creates an Agent,
executes a terminal command, reconnects, checks workspace persistence across
stop/resume, and deletes its test resources:

```sh
./smoke --base-url http://127.0.0.1:8080 --token-file ./api-token
```

The API remains private and always requires bearer authentication. Use a private
remote transport or enable the optional Tailscale Service. Individual agent Pods
never join the tailnet.

## Enable Codex and Herdr

Codex is disabled by default so the initial install does not wait for an
unbootstrapped credential. Set `codex.enabled: true` and upgrade without `--wait`
for this bootstrap step, then run the operator login:

```sh
helm upgrade sandherd ./sandherd-0.1.0-alpha.1.tgz \
  --kube-context "$KUBE_CONTEXT" -n "$SANDHERD_NAMESPACE" -f site-values.yaml
kubectl --context "$KUBE_CONTEXT" -n "$SANDHERD_NAMESPACE" exec -it \
  deployment/sandherd-codex-auth -- /usr/local/bin/codex \
  -c 'cli_auth_credentials_store="file"' login --device-auth
kubectl --context "$KUBE_CONTEXT" -n "$SANDHERD_NAMESPACE" \
  rollout status deployment/sandherd-codex-auth
```

Only the coordinator holds the subscription refresh credential. It remains
unready until bootstrap succeeds. Its private Service stays reachable while
unready so clients receive fixed remediation reasons. Complete account-level
login in your browser; never send a credential file through chat or release
artifacts. Provider authentication acceptance is tracked separately from Helm
installation readiness.

For Herdr, create the Secret named by `herdr.accessSecret` with `authorized_keys`,
`ssh_host_ed25519_key`, and `api-token`, using files with restrictive permissions.
Set `herdr.enabled: true`. Its default Agent is shell, or Codex when enabled.
The SSH host key must be provisioned and verified by the operator. Keep it stable
across upgrades; never disable host-key checking. The Herdr home PVC is retained.
The gateway activates its prebuilt Sandherd plugin at startup. Packaged shell
acceptance covers the remote SSH TUI, manager creation, terminal input, pane
replacement with replay, stop/resume and retained-workspace deletion. Codex
subscription and native-session acceptance remains a separate release gate.

With the Tailscale operator installed, set `tailscale.enabled: true` and choose
`tailscale.apiHostname`. This exposes only the authenticated API. The optional
Herdr SSH endpoint additionally requires an explicit `tailscale.herdrHostname`.
Tailnet ACLs remain an operator responsibility. On the homelab, also configure
`network.cilium: true` and `storage.className: rook-ceph-block`.

A requested `runtime.className` also requires a verified `runtime.nodeSelector`.
The chart does not install node runtimes or silently fall back to containers.
Do not advertise a gVisor profile until the node/runtime acceptance tests pass.

## Upgrade and rollback

Back up configuration and quiescent repository data before an upgrade. Retain the
previous bundle and its image digests. Helm does not automatically upgrade CRDs:
review and apply the bundled `agents.sandherd.dev.yaml` before a compatible chart
upgrade. An incompatible CRD migration needs its own data migration plan.

```sh
kubectl --context "$KUBE_CONTEXT" apply --server-side -f agents.sandherd.dev.yaml
helm upgrade sandherd ./sandherd-NEW-VERSION.tgz \
  --kube-context "$KUBE_CONTEXT" -n "$SANDHERD_NAMESPACE" \
  -f site-values.yaml --wait --timeout 5m
helm history sandherd --kube-context "$KUBE_CONTEXT" -n "$SANDHERD_NAMESPACE"
helm rollback sandherd PREVIOUS_REVISION \
  --kube-context "$KUBE_CONTEXT" -n "$SANDHERD_NAMESPACE" --wait --timeout 5m
```

The single control plane uses `Recreate`, because it has no leader election.
Upgrades briefly interrupt API and terminal connections. Existing agent processes
and PVCs survive control-plane-only changes. Changes to runtime templates,
images, resource budgets or isolation settings can recreate agent Pods; workspace
identity is preserved, but in-memory processes and terminal replay are not.
Codex uses native session resume on recreation. A rollback can recreate runtimes
again. Helm rollback does not undo CRD or external credential changes.

## Backups and restore

Keep a protected copy of operator values, the image lock, Agent specifications,
and the credential/signing resources needed for recovery. Store credentials in
an encrypted secret manager, separately from ordinary workspace archives.

For a quiescent repository, archive `/workspace/repository` from the runner and
record a SHA-256 checksum. Restore the archive into a fresh, isolated Agent's
repository and verify its contents before use. The release lifecycle test
exercises this path. It does not prove application-consistent backup of an
actively changing repository. Pause application writes or use a documented CSI
snapshot/quiescing procedure for that requirement.

Do not include `/home/sandherd`, `.sandherd`, auth files, or the coordinator PVC in
ordinary repository backups. Full-volume snapshots can include access tokens and
must be encrypted, access-controlled and governed as credentials. Native session
recovery and full-cluster disaster recovery require their own protected backup
plan; a repository archive alone does not preserve native sessions.

## Uninstall and data retention

Stop or delete Agents through the API before uninstalling the control plane.
Deleting an Agent with `retentionPolicy: delete` deletes its workspace; `retain`
preserves it for administrator recovery. Uninstalling the chart does not delete
Agent CRDs or their instances, retained workspaces, externally provisioned
credentials, or the retained Codex/Herdr state PVCs. Running Agents can keep
consuming resources after the control plane is removed.

```sh
helm uninstall sandherd --kube-context "$KUBE_CONTEXT" -n "$SANDHERD_NAMESPACE"
kubectl --context "$KUBE_CONTEXT" -n "$SANDHERD_NAMESPACE" get agents,pvc
```

Dispose of retained objects only after explicitly deciding their data is no
longer needed. Deleting the namespace destroys retained data and credentials.
Do not delete upstream Agent Sandbox CRDs while any installation uses them.

## Flux and troubleshooting

Publish the chart bundle to your reviewed Helm/OCI source, then reference that
exact version from a Flux HelmRelease with a dedicated target namespace and
`site-values.yaml` settings. Reconcile the prerequisite controller separately;
use dependency ordering so its CRDs are established first. Keep credential
Secrets outside Helm values, using the organization's secret delivery mechanism.
Do not convert an existing unmanaged installation by changing ownership labels.

Inspect Deployments, Agent status, claims, Pod events, and PVC bindings in that
order. Missing API egress usually leaves the control plane unready. An
`InvalidMetadata` claim requires the controller's `sandherd.dev` label allowlist
and a controller restart if the config is read only at startup. Pending PVCs
require a working StorageClass. `credential_reauthentication_required` requires
operator login; `credential_refresh_failed` requires coordinator inspection and
possibly reauthentication. Never paste raw provider output or auth files into
issue reports. Use public reason codes and redacted metadata.
