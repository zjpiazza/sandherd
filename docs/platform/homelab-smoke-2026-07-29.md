# Homelab smoke baseline — 2026-07-29

Issue #3 was validated against the `admin@homelab` context before its pull
request was opened.

## Environment

- Three Talos v1.13.0 nodes.
- Kubernetes v1.36.0.
- Cilium network policy enforcement.
- Rook/Ceph `rook-ceph-block` ReadWriteOnce storage.
- Agent Sandbox v0.5.3 controller and router source at commit
  `f9cf22a0936390a120353380fed80392278de6b6`.

## Baseline

- Cold template/pool provisioning: 10 seconds.
- Ready pool to ready claim adoption: under 1 second (reported as 0 seconds by
  the whole-second smoke timer).

These are single observations for correctness, not performance claims. Warm
pool sizing and repeatable load measurements remain out of scope until the
scaling milestone.

## Verified behavior

- All four v1beta1 CRDs reached `Established` and stored v1beta1 objects.
- Controller, conversion webhook, and two router replicas became ready.
- Reapplying the platform was idempotent.
- The router accepted a valid in-cluster bearer token and rejected missing and
  invalid tokens.
- Router network policy rejected a valid identity outside the authorized
  namespace.
- A file on `rook-ceph-block` survived deletion and recreation of the sandbox
  Pod, including RWO detach and reattach on another node.
- Sandbox DNS resolution succeeded.
- Sandbox connections to the Kubernetes API and a private Talos node failed.
- Deleting the claim removed its Sandbox, Pod, headless Service, and PVC.
- The smoke cleanup left no labelled workload or storage resources.

The router test image was published temporarily and the platform was removed
after validation so the cluster did not retain a dependency on an expiring
artifact.
