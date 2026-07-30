# Durable workspace homelab validation — 2026-07-30

Issue #8 was validated against the `admin@homelab` context before its pull
request was opened.

## Environment

- Three Talos v1.13.0 nodes running Kubernetes v1.36.0.
- Cilium network policy enforcement.
- Rook/Ceph `rook-ceph-block` ReadWriteOnce storage.
- Agent Sandbox v0.5.3 with the Sandherd controller, runtime, and authenticated
  router images pulled anonymously from the public GitHub Container Registry.

## Verified behavior

- The REST API created a logical Agent, one `SandboxClaim`, one Sandbox, and one
  bound workspace PVC without exposing Kubernetes identifiers to the client.
- An empty workspace was writable by the unprivileged UID/GID 65532 runner.
- Stopping an Agent set the v1beta1 Sandbox operating mode to `Suspended`,
  removed its Pod, and left the Ceph PVC bound.
- Resuming the Agent recreated its Pod and preserved a marker written under
  `/workspace/repository` on the same volume.
- A public repository bootstrap cloned
  `https://github.com/zjpiazza/sandherd.git`, checked out `main` at
  `4b28a21e1a36a131c19def875ebbefa1e57aa389`, and recorded secret-free metadata
  in `/workspace/.sandherd/bootstrap.json`.
- Deleting each disposable Agent removed its claim, Sandbox, Pod, Service, and
  PVC. Cleanup left no Agent or workspace resources in `sandherd-system`.

## Regressions caught by the live cluster

The real API server and container runtime exposed four contract gaps that the
in-process Kubernetes fakes could not:

- Agent Sandbox requires `sandherd.dev` in its propagated-label domain
  allowlist.
- The v1beta1 suspend/resume field is `spec.operatingMode`, not the removed
  alpha `spec.replicas` field.
- Empty workspace metadata must normalize the default revision to `HEAD` across
  Pod recreation.
- Runtime containers must start in `/workspace` so the container runtime does
  not pre-create `/workspace/repository` as root on a fresh PVC.

The platform renderer and Go tests now cover these contracts, and `make verify`
passed after the homelab fixes.
