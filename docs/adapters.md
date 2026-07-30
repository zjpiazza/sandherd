# Agent adapters

An **agent adapter** tells Sandherd how to run one coding-agent harness. Codex,
Claude Code, OpenCode, Pi, and Grok Build are agent harnesses; they are not LLM
providers. A harness may support several providers, while a **credential
profile** is an operator-defined authentication policy for one installed
adapter. Repository `secretProfile` credentials are a separate bootstrap-only
concern.

Clients discover enabled adapters with `GET /v1alpha1/adapters`. The response
contains only stable IDs, display names, versions, and capabilities. It never
contains commands, warm-pool names, credential bindings, images, Secret names,
or health checks.

## Installing an adapter

The control plane loads a versioned, strict JSON registry from
`--adapter-config-file`. The base deployment stores it in
[`adapter-config.yaml`](../deploy/control-plane/adapter-config.yaml). Unknown
fields, duplicate IDs or profile bindings, relative executables, unsupported
capabilities, and inconsistent credential modes prevent control-plane startup.

One adapter can have several approved `(sandboxProfile, credentialProfile)`
bindings:

```json
{
  "version": 1,
  "adapters": [
    {
      "id": "codex",
      "displayName": "Codex",
      "version": "1.0.0",
      "capabilities": ["interactive", "headless", "session-resume", "mcp", "subscription-auth"],
      "profiles": [
        {
          "sandboxProfile": "standard",
          "credentialProfile": "personal-subscription",
          "credentialMode": "mutable",
          "warmPool": "sandherd-codex-personal",
          "command": ["/usr/local/bin/codex"],
          "healthCheck": ["/usr/local/bin/codex", "--version"]
        }
      ]
    }
  ]
}
```

The adapter registry is trusted operator configuration, not a public plugin
API. The referenced `SandboxWarmPool` and `SandboxTemplate` own the pinned
runtime image, installed binary, resource defaults, credential mounts, and
egress policy. The registry selects that reviewed runtime and supplies its
trusted start and health commands. Public requests cannot override any of
those values.

Supported credential modes are:

| Mode | Meaning |
| --- | --- |
| `none` | The harness needs no injected agent credential. |
| `immutable` | A reviewed runtime mounts read-only API credentials. |
| `mutable` | Subscription/OAuth state is durable and refreshable. |
| `workload-identity` | The runtime uses operator-provided identity or an exec credential. |

The base registry enables two credential-free shell development adapters and a
pinned Codex adapter using the platform ChatGPT subscription coordinator. See
the [Codex operator guide](codex.md). Claude Code, OpenCode, Pi, and Grok Build
entries are delivered by their tracked integration issues rather than
pretending the generic runtime already contains those binaries.

To disable an adapter, remove its definition and roll the control-plane
Deployment. Existing Agents retain their desired adapter ID but cannot be
reprovisioned until that exact binding is installed again. To upgrade, publish
a new immutable runtime image/pool, update the adapter version and binding, and
roll the control plane. Reconciliation replaces outdated runtime claims while
preserving the Agent and workspace. An explicit adapter/profile selection
change also increments the public runtime generation.

## Selecting and changing an adapter

Create accepts the adapter ID in `spec.kind` and an optional
`spec.credentialProfile`. Both the adapter binding and the caller's independent
`credentialProfiles` allowlist are checked before any sandbox is created.

Change an existing Agent with:

```http
POST /v1alpha1/agents/{agentId}:change-adapter
Authorization: Bearer ...
If-Match: "..."
Content-Type: application/json

{"kind":"shell-minimal"}
```

A successful request returns `202`, increments `runtimeGeneration`, and enters
`reconfiguring` while the old runtime drains. Repeating the same selection is
idempotent. Native sessions are adapter-specific and are not translated.

The replacement keeps the logical Agent ID, repository data, workspace size,
storage profile, and retention policy. The controller suspends the old
Sandbox, orphan-deletes its claim and deterministic Sandbox, and recreates a
claim with the same name. Agent Sandbox then adopts the same workspace PVC.
If the new runtime fails, the durable workspace remains available for retry or
rollback.

## Runtime and state boundaries

The workspace PVC has three distinct areas:

```text
/workspace/repository                     shared project files
/workspace/.sandherd                      trusted bootstrap metadata
/workspace/.sandherd/adapters/<adapter>   adapter-native HOME and XDG state
```

The running process sees only the repository at `/workspace/repository` and
its selected adapter state at `/home/sandherd`. It cannot traverse to bootstrap
metadata or another adapter's retained state through those mounts. A future
credential-specific runtime may replace the adapter state mount with a
separately governed durable volume; it must not put refreshable OAuth state in
the repository.

Before starting the PTY, the runner executes the adapter health check with a
ten-second timeout and discards stdout and stderr. Failures use the fixed
`adapter health check failed` message, so tool output and credentials cannot
enter status or logs.

The physical PVC preservation and mount isolation were also exercised on the
homelab; see the [adapter rebind smoke report](platform/adapter-rebind-smoke-2026-07-30.md).

## Adding another adapter

Adding a harness should not add branches to the REST handlers, lifecycle state
machine, runner, or Herdr bridge:

1. Build an immutable runtime image containing the pinned harness binary and
   Sandherd runtime binaries.
2. Define a reviewed SandboxTemplate/WarmPool for mounts, egress, resources,
   and one credential strategy.
3. Add a registry definition with an absolute start command and a non-mutating,
   secret-free health check.
4. Declare only capabilities the integration contract tests demonstrate.
5. Add lifecycle tests for create, attach, detach, stop, resume, delete, and
   adapter rebind, including a workspace canary.
6. Verify health failures and API fixtures contain no credential material.

Use a new profile binding when authentication or egress differs. Do not accept
arbitrary image names, commands, paths, environment values, Secret names, or
PVC names from clients.
