# Codex adapter and ChatGPT subscription authentication

Sandherd packages OpenAI Codex CLI `0.146.0` in a dedicated runtime and uses a
single platform-level ChatGPT login. After that one operator-authorized login,
Codex sandboxes can be created and destroyed autonomously without API keys or
per-agent authentication.

The design follows OpenAI's file-auth guidance while avoiding concurrent
writes to one `auth.json`:

```text
operator login/import
        |
        v
codex-auth coordinator --- master auth.json + refresh token (RWO PVC)
        |
        +--- refresh-token-free snapshot ---> agent A CODEX_HOME
        +--- refresh-token-free snapshot ---> agent B CODEX_HOME
        +--- refresh-token-free snapshot ---> new agents
```

The coordinator is the only refresh authority. It runs the pinned Codex CLI to
perform normal built-in token maintenance and atomically retains the refreshed
master file. An init container seeds each sandbox; a sidecar follows later
access-token updates. Sandboxes receive the access and identity tokens needed
to run Codex, but never receive the platform refresh token or Kubernetes
credentials.

OpenAI documents that file auth lives at `$CODEX_HOME/auth.json`, is writable
because Codex refreshes it, and should be treated like a password. It also says
not to share one writable file across concurrent machines. The coordinator is
Sandherd's single-writer implementation of that constraint. See the upstream
[authentication guide](https://developers.openai.com/codex/auth) and
[trusted private automation guide](https://developers.openai.com/codex/auth/ci-cd-auth).

## Install and bootstrap

Build and publish the Codex image separately from the generic runtime:

```sh
docker build --target codex-runtime \
  --tag registry.example/sandherd-codex-runtime:0.146.0 .
docker push registry.example/sandherd-codex-runtime:0.146.0
```

Pass that immutable image reference as `CODEX_RUNTIME_IMAGE` when running the
homelab installer. Installation creates:

- the `sandherd-codex` SandboxTemplate and zero-capacity compatibility pool;
- the private `sandherd-codex-auth` Service and NetworkPolicy;
- one `Recreate` coordinator Deployment; and
- a dedicated ReadWriteOnce PVC for the master credential.

The coordinator intentionally stays unready until it has a valid credential.
Bootstrap it once with the preferred device-code flow:

```sh
./scripts/bootstrap-codex-auth.sh
```

The script starts `codex login --device-auth` inside the coordinator. Open the
shown URL, authorize the ChatGPT subscription account, and enter the one-time
code. Device-code authentication must be enabled in the account or workspace.

As an explicit alternative, import an existing trusted ChatGPT file over
stdin. The script does not print or store the local path in Kubernetes:

```sh
CODEX_AUTH_FILE=/secure/path/auth.json ./scripts/bootstrap-codex-auth.sh
```

The import rejects API-key mode, missing refresh authority, malformed tokens,
oversized files, and non-ChatGPT credentials. It writes the master file with
mode `0600`. Do not commit either the imported file or the coordinator volume.

## Runtime and lifecycle behavior

Create with `kind: "codex"` and
`credentialProfile: "chatgpt-subscription"`. Both the adapter binding and the
caller's credential-profile allowlist must permit that exact public policy.

Each logical Agent has an isolated adapter home at
`/workspace/.sandherd/adapters/codex`, mounted as `/home/sandherd`. Codex sees:

- its own writable `$CODEX_HOME` at `/home/sandherd/.codex`;
- its own config, history, MCP configuration, and session JSONL files;
- only that Agent's repository at `/workspace/repository`; and
- a locally written auth snapshot with an empty refresh-token field.

Attach and detach retain the same PTY and Codex process. Stop, resume, Pod
replacement, node movement, and Sandbox recreation retain the workspace PVC.
On a fresh runtime process, `codex-launcher` starts a new Codex session when no
session exists and otherwise invokes Codex's native `resume --last`. Changing
away from Codex retains its isolated Codex state; changing back resumes the
most recent Codex session. Deleting an Agent with normal `delete` retention
removes its workspace and sessions. `retain` follows the normal administrator
recovery rules.

The platform credential PVC is not agent workspace data. Never copy it into an
Agent PVC, expose it through Herdr or the public API, or include it in routine
workspace backups. If infrastructure snapshots include the coordinator PVC,
encrypt them, restrict them like a password vault, and set a short retention
period. Losing it requires another operator login; it does not destroy Agent
sessions or repositories.

## Health, reauthentication, and revocation

The public Agent API exposes only stable, content-free reasons:

| Reason | Operator action |
| --- | --- |
| `credential_unavailable` | Check the coordinator Pod, Service, PVC, and network policy. |
| `credential_reauthentication_required` | Run `scripts/bootstrap-codex-auth.sh` again. |
| `credential_refresh_failed` | Inspect coordinator health and retry; reauthenticate if it persists. |

Raw Codex output, token bodies, the master file, and internal file paths are
never copied into Agent status or coordinator logs. The coordinator status
command reports only timestamps:

```sh
kubectl --context admin@homelab --namespace sandherd-system exec \
  deployment/sandherd-codex-auth -- /usr/local/bin/codex-auth status
```

To stop cluster use immediately, scale the coordinator to zero and stop active
Codex Agents. To clear its cached credential, run `codex logout` in the
coordinator Pod before scaling it down. Complete account-level revocation in
ChatGPT security settings when compromise is suspected, then reauthenticate
the coordinator. A repaired credential is picked up by failed init containers,
running sidecars, and newly created sandboxes without per-agent login.

## Verification

`build/codex/checksums.txt` pins the amd64 and arm64 release assets. CI verifies
both upstream Sigstore bundles against OpenAI's tagged release workflow,
builds both runtime architectures, and fails on fixed high or critical image
vulnerabilities. Run the provenance check locally with Cosign and zstd present:

```sh
./build/codex/verify-release.sh
```

The live homelab smoke report records concurrent agents, refresh propagation,
session isolation and resume, stable reauthentication behavior, and cleanup.
