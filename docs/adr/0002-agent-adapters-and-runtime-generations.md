# ADR 0002: Agent adapters and runtime generations

- Status: Accepted
- Date: 2026-07-30
- Owners: Sandherd maintainers
- Related issue: [#25](https://github.com/zjpiazza/sandherd/issues/25)

## Context

A durable Sandherd Agent may run Codex today and another coding-agent harness
later without losing its project files. Tool binaries, authentication, native
sessions, state paths, and network needs differ. Exposing those Kubernetes and
process details in the public API would make clients privileged and couple the
core lifecycle to every supported harness.

Workspace lifetime also differs from runtime lifetime. Replacing a binary
inside a live sandbox risks concurrent access and makes rollback ambiguous,
while replacing the whole Agent would discard its stable identity and API
history.

## Decision

### Adapters are trusted, installed policy

The control plane loads a strict versioned adapter registry. A stable adapter
ID and the Agent's sandbox and credential profiles resolve to one approved warm
pool, credential mode, start command, health check, and adapter version. The
warm pool owns the pinned image, mounts, egress, and runtime security policy.
Clients may select installed IDs and profiles but cannot submit these internal
values.

Public discovery returns only adapter identity, version, and capabilities. The
runner receives a resolved launch specification through trusted claim
environment injection and contains no harness-specific branches.

### Agent identity and runtime generation are separate

`spec.kind` is the desired adapter ID. `runtimeGeneration` identifies each
resolved runtime incarnation. Changing an adapter updates the same Agent,
increments the generation, and enters `reconfiguring`; it does not mutate the
process inside the existing sandbox.

Reconciliation suspends the old Sandbox, waits for suspension, orphan-deletes
the claim and deterministic Sandbox, then creates the target claim with the
same name. Agent Sandbox adopts the same labelled workspace PVC. Status records
the active generation, adapter ID, and adapter version only after the target
claim and runner are ready.

### Persistence domains are explicit

Repository data, trusted bootstrap metadata, adapter-native state, credentials,
and ephemeral cache have different policy. The standard runtime exposes the
repository plus only the selected adapter's HOME to the running process. Native
sessions are not portable between adapter IDs; retained state can be reused if
the Agent switches back.

Repository credentials and agent credentials use separate public profile
names and separate principal allowlists. Refreshable subscription state must
not live in the repository checkout.

## Consequences

- Herdr and other clients remain implementation-neutral.
- Adapter changes preserve Agent identity and physical workspace data.
- Unsupported adapter/profile combinations fail before cluster mutation.
- Operators, rather than API callers, control binaries, credentials, and
  network access.
- Runtime replacement briefly makes the Agent unavailable and requires the
  upstream deterministic claim/PVC adoption behavior pinned by Sandherd.
- Adapter-native session continuity across different harnesses is explicitly
  unsupported.

## Rejected alternatives

### Run a new command in the existing sandbox

Rejected because the old process, image, mounts, credential policy, and egress
remain in effect and because exclusive workspace ownership cannot be proven.

### Make each adapter a public arbitrary-command plugin

Rejected because it would let API callers bypass reviewed images, credential
bindings, and workload policy.

### Put all agent state in the repository workspace

Rejected because OAuth tokens and harness metadata would mix with user files,
be easy to commit, and become visible to every adapter.

### Replace the logical Agent when changing adapters

Rejected because clients would lose stable identity, ownership, lifecycle
history, and workspace continuity.
