# Durable workspaces and repository bootstrap

The initial homelab lifecycle and public-clone checks are recorded in the
[2026-07-30 durable workspace smoke report](platform/durable-workspace-smoke-2026-07-30.md).

Each Sandherd Agent gets one `ReadWriteOnce` PVC mounted at `/workspace`. The
agent process starts in `/workspace/repository`; Sandherd's private bootstrap
state lives in `/workspace/.sandherd` with mode `0700`.

For a source-checkout homelab deployment, first install Agent Sandbox, publish
the control-plane and `agent-runtime` images, create the API principals file and Ed25519
key files described in the [terminal gateway guide](terminal-gateway.md), then
run:

```sh
CONTROL_PLANE_IMAGE=registry.example/sandherd-control-plane:dev \
AGENT_RUNTIME_IMAGE=registry.example/sandherd-agent-runtime:dev \
SANDHERD_API_PRINCIPALS_FILE=/secure/path/principals.json \
SANDHERD_CAPABILITY_PRIVATE_KEY_FILE=/secure/path/capability-private-key.pem \
SANDHERD_CAPABILITY_PUBLIC_KEY_FILE=/secure/path/capability-public-key.pem \
./scripts/install-sandherd-homelab.sh
```

The script requires the explicit `admin@homelab` context by default, creates or
updates only the named Sandherd Secret/ConfigMap, replaces the two development
image references, applies the homelab overlay, and waits for readiness. Set
`DRY_RUN=client` or `DRY_RUN=server` to validate without persisting changes.

## Storage profiles

The public API accepts a policy name, not a Kubernetes StorageClass. The control
plane maps approved names to cluster configuration:

```text
--storage-profile=default=rook-ceph-block
```

Use `--storage-profile=default=-` to select the cluster's default StorageClass.
An unknown profile fails with the stable `storage_profile_not_found` reason.
The claim requests the Agent's validated workspace size and labels the PVC with
the logical Agent ID; no Kubernetes name is returned through the public API.

The standard runtime template in [`deploy/agent-runtime/base`](../deploy/agent-runtime/base)
allows a claim to inject this PVC but defines no host mount. The bootstrap and
runner containers use the same numeric user and `fsGroup` so workspace files do
not require a privileged ownership step. Both containers start in `/workspace`
so the runtime cannot pre-create `/workspace/repository` as root before the
unprivileged bootstrap initializes it.

## Bootstrap behavior

`workspace-bootstrap` receives the repository URL and requested revision only
in the init container. It:

1. validates the fixed workspace mount, URL, revision, and credential mode;
2. locks `/workspace/.sandherd/bootstrap.lock`;
3. records a secret-free bootstrap intent;
4. initializes and fetches into a private staging directory;
5. checks out `FETCH_HEAD` detached with system/global Git configuration and
   hooks disabled;
6. atomically renames the completed repository into place; and
7. records origin, requested revision, resolved commit, and completion time in
   `/workspace/.sandherd/bootstrap.json`.

No repository file is executed during bootstrap. Destination paths are fixed,
symlinks at trusted state boundaries are rejected, embedded URL credentials and
option-like revisions are rejected, and Git output is not copied into public
Agent status. A retry clears only Sandherd's staging path. Once the final marker
exists, later Pod starts preserve the repository and all agent changes. An
interruption after the atomic rename but before the marker write is recovered
from the matching intent without recloning.

An Agent without `spec.repository` gets an empty `/workspace/repository`.

## Private repositories

Credentials are selected through a public `secretProfile`; clients never send
secret material or Kubernetes Secret names. The control plane maps that profile
to an administrator-approved credentialed warm pool:

```text
--secret-profile=personal=sandherd-standard-https-private
```

The HTTPS and SSH examples under
[`deploy/agent-runtime/examples`](../deploy/agent-runtime/examples) demonstrate
the isolation boundary. Their Secret volume is mounted only into the
`workspace-bootstrap` init container—not the long-running `runner` container.
Each credentialed pool should represent one narrowly scoped repository policy.
The authenticated principal must also list that public profile in its
`secretProfiles` allowlist; otherwise create returns
`forbidden_secret_profile` before any cluster resource is created.

For HTTPS, create a JSON credential with a provider-specific username and token:

```json
{"username":"git-user","password":"repository-scoped-token"}
```

Create the example Secret without committing the file, apply the example
profile, and configure the mapping:

```sh
kubectl --context admin@homelab --namespace sandherd-system \
  create secret generic sandherd-repository-https-private \
  --from-file=credential.json=/secure/path/credential.json

kubectl --context admin@homelab apply --kustomize \
  deploy/agent-runtime/examples/https-credential-profile
```

The SSH example expects `identity` and a pinned `known_hosts` file. It forces
batch mode, `IdentitiesOnly`, and strict host-key checking:

```sh
kubectl --context admin@homelab --namespace sandherd-system \
  create secret generic sandherd-repository-ssh-private \
  --from-file=identity=/secure/path/id_ed25519 \
  --from-file=known_hosts=/secure/path/known_hosts

kubectl --context admin@homelab apply --kustomize \
  deploy/agent-runtime/examples/ssh-credential-profile
```

Rotating a failed credential lets Kubernetes retry the idempotent init
container. Making credentials available to the running agent is intentionally a
separate secret policy and is not implied by repository bootstrap.

## Retention and recovery

- `delete`: Agent deletion removes the claim, Sandbox, Pod, Service, and PVC.
- `retain`: before deleting the claim, the control plane clears the matching
  PVC's owner references. The PVC remains labelled with its logical Agent ID for
  explicit administrative recovery.
- Pod replacement, node rescheduling, stop/resume, and control-plane restart do
  not change the PVC or rerun a completed bootstrap.
- Public restore is not implicit: a retained volume is never attached to a
  different Agent merely because names match. An administrator must snapshot or
  clone it into a newly approved workspace. This prevents cross-owner data
  adoption; a future restore API can add an authenticated ownership proof.

Back up retained volumes before deleting them administratively.

## Stable failures and inspection

The bootstrap exits with codes that the reconciler maps to safe Agent reasons:

| Reason | Meaning |
| --- | --- |
| `bootstrap_invalid` | Invalid bootstrap or credential configuration |
| `workspace_unsafe` | Symlink or unsafe workspace state rejected |
| `repository_auth_failed` | HTTPS or SSH authentication failed |
| `repository_bootstrap_failed` | Fetch, checkout, or commit resolution failed |
| `workspace_full` | PVC or temporary storage exhausted |
| `bootstrap_timeout` | Bootstrap exceeded its configured deadline |

The standard template bounds bootstrap CPU, memory, duration, and temporary
storage. From an attached agent terminal, inspect safe repository state with:

```sh
git -C /workspace/repository remote get-url origin
git -C /workspace/repository rev-parse HEAD
git -C /workspace/repository status --short --branch
cat /workspace/.sandherd/bootstrap.json
```
