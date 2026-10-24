# Changelog

## 0.1.0-alpha.1 — unreleased

- License Sandherd under Apache-2.0.
- Package the private control plane, resource-bound runtimes, router, and optional
  Codex authentication and Herdr gateway in a versioned Helm chart.
- Enforce requested runner CPU and memory before scheduling. Preserve approved
  runtime and network settings in immutable per-Agent templates.
- Recover completed runner Pods and allow operators to stop stalled Agents.
- Preserve terminal replay, durable workspaces, and native Codex session resume.
- Activate the prebuilt Herdr plugin in the gateway; reset stale replay cursors
  when a resumed runner rejects them during attachment.
- Centralize subscription credential refresh; reject expired snapshots and
  surface redacted credential failures with recovery after reauthentication.
- Publish digest-pinned amd64/arm64 images with SBOM and provenance metadata;
  patch reachable Go and image vulnerabilities and add CI checks.
- Add installed lifecycle, recovery, release upgrade/rollback, isolation, and
  repository backup/restore tests, plus full Herdr SSH/plugin lifecycle acceptance.

This pre-release remains under acceptance testing. gVisor rollout, warm-pool
performance, idle policy enforcement, and additional coding-agent adapters are
tracked separately. The release is not published until its remaining acceptance
requirements are resolved.
