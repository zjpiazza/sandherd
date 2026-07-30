# Security model

Sandherd treats every agent process and its sandbox as hostile. The public API
is the only user-facing control boundary: Herdr and other clients never receive
a kubeconfig, Agent Sandbox router credential, Tailscale auth key, or runner
credential.

## Trust boundaries

| Boundary | Trust and authority |
| --- | --- |
| API client | Untrusted except for its bearer credential. A principal can see only Agents whose persisted owner equals its stable ID. |
| Tailscale | Private transport to the control-plane Service, not application identity. The API still requires a bearer credential on REST, SSE, and WebSocket requests. |
| Control plane | Trusted. It authenticates clients, enforces ownership and role, maps public profiles to cluster policy, and holds the namespace-scoped Kubernetes and capability-signing credentials. |
| Agent Sandbox router | Trusted only to authenticate its Kubernetes caller and route to the named sandbox. It cannot create workloads or read Secrets. |
| Runner and agent sandbox | Potentially compromised. They receive no service-account token and no client/control-plane credential. The runner has only the public capability verification key; a terminal request carries a single-use capability for one Agent and operation. |
| Kubernetes control plane | Trusted platform boundary. Namespace RBAC, admission, CNI policy, storage, and optional stronger runtimes enforce the sandbox boundary. |

The homelab overlay creates a Tailscale `LoadBalancer` Service for only the
control plane. Agents do not join the tailnet. The Tailscale proxy reaches port
8080 through an explicit ingress policy, while the normal bearer authentication
and ownership checks remain mandatory.

## Client identity and authorization

Authentication is an interface in the control plane, so an OIDC or external
identity implementation can replace the initial file backend without changing
the lifecycle handlers. The initial backend reloads a versioned JSON file for
every request. Each record has:

- a stable principal `id`, used as the private Agent owner;
- a random bearer `token` of 32 to 4096 non-whitespace characters;
- `observe` or `control` permission (`control` includes observation); and
- an allowlist of public repository `secretProfiles`; and
- an independent allowlist of agent `credentialProfiles`.

Start from [`examples/principals.json`](examples/principals.json), replace every
placeholder, and keep the result outside the repository with mode `0600`.
Principal IDs and bearer tokens must be unique. Missing and invalid credentials
return the same `401` response. A temporarily unreadable identity file returns a
retryable `503`; its parser errors never include token values.

An Agent lookup is always owner-filtered. A cross-owner ID is indistinguishable
from a missing Agent (`404`), including terminal attachment and lifecycle
mutation. Observe principals can list, inspect, follow events, and attach an
observe terminal; control is required to create, stop, resume, delete, request
a controller attachment, or take over a controller lease.

## Internal capabilities and secrets

The control plane mints an Ed25519-signed runner capability only after resolving
an owned Agent. It expires after 30 seconds and is bound to the runner audience,
one Agent ID, the terminal operation, the caller's role, a request ID, and a
random token ID. A runner consumes the token atomically on first successful
verification, so expiry, replay, wrong audience, wrong Agent, and wrong operation
all fail. Capabilities are passed in one internal request header and are never
stored in the workspace.

A public `secretProfile` is a policy name, never a Kubernetes Secret name or
secret value. Both the principal allowlist and the administrator's configured
profile mapping must approve it. Repository credentials are mounted only into
the bootstrap init container. They are absent from the long-running runner and
Agent process. Secret values and terminal frames are excluded from API errors,
events, metrics, traces, and structured logs.

An agent `credentialProfile` is separately allowlisted and must match an
installed adapter binding. The trusted adapter registry contains no secret
contents or public Kubernetes object names. It resolves an approved warm pool,
credential mode, command, and health check; these values never appear in the
adapter discovery response. The runner discards all health-check output and
returns a fixed failure message. Refreshable OAuth/subscription state belongs
in adapter-specific durable state, never in the repository checkout. See the
[adapter guide](adapters.md).

## Kubernetes and network isolation

The control-plane Role manages only Sandherd Agents, SandboxClaims, adopted
Sandboxes, runner status inputs, and retained PVC metadata in
`sandherd-system`. It cannot read Secrets, create arbitrary Pods or controllers,
read Nodes, or act in another namespace. The router has only delegated
authentication review. The sandbox service account has no RBAC binding and its
token is not mounted. Run `make rbac-check` to assert the allow and deny matrix
against `admin@homelab` (or set `KUBE_CONTEXT`).

Sandbox ingress and egress are default-deny. The standard profile permits:

- DNS to the selected `kube-dns` Pods on TCP/UDP 53;
- runner ingress on TCP 8080 only from the Agent Sandbox router; and
- public IPv4/IPv6 TCP 443 and 22 for provider, Git, and package access.

The public rule excludes loopback, cluster/private IPv4, Tailscale CGNAT,
link-local/metadata, multicast/reserved IPv4, and loopback/private/link-local
IPv6. Thus a hostname resolving to one of those ranges remains blocked. Build a
separate reviewed SandboxTemplate or overlay to change provider ports or
destinations; do not add an unrestricted `0.0.0.0/0` rule. The live smoke test
checks DNS while probing the Kubernetes API, a node, metadata, and tailnet
addresses from inside a sandbox.

NetworkPolicy does not replace a stronger runtime boundary. Use the separately
tracked gVisor or Kata profile when the node/container boundary must withstand a
kernel exploit.

## Audit and telemetry

Authentication failures, unavailable authentication state, denied role/profile
requests, and attempts to address an unowned Agent emit structured
`security audit` records. Records contain an event name, outcome, safe principal
ID when authenticated, Agent ID when supplied, request ID, operation/path, and a
fixed reason. They never contain Authorization headers, request bodies,
terminal frames, repository URLs, capability tokens, or secret values. Gateway
metrics expose only counts, durations, and byte totals.

## Credential rotation and incident response

- Client token: replace or remove its principal record and update the
  `sandherd-api-principals` Secret, then update the client credential file. The
  backend reloads the projected file without a process restart; the v1 file
  format intentionally permits only one token per principal.
- Capability key: replace the private Secret and matching public ConfigMap,
  then roll the control plane and sandbox workloads. Existing capabilities die
  within 30 seconds; this version intentionally has no multi-key overlap.
- Repository credential: rotate the profile's Kubernetes Secret and restart
  only failed/new bootstrap init containers. It never becomes a client token.
- Router credential: Kubernetes rotates the bound service-account token. Do not
  copy it outside the control-plane Pod.

For a compromised client token, revoke it, inspect audit records by principal
and request ID, terminate suspicious attachments, and rotate any repository
credential in a profile that principal was allowed to request. Its blast radius
is that principal's Agents and allowed profiles.

For a compromised Agent, stop/delete its sandbox, preserve the PVC only if it
is needed for forensics, rotate repository credentials used during bootstrap,
and inspect denied network/audit events. The expected boundary prevents access
to other Agents, Kubernetes, nodes, LAN, tailnet, and control credentials.

For a compromised control plane, assume all client tokens and the capability
signing key are exposed and all Sandherd Agents were controllable. Isolate the
Service, revoke/replace the principal Secret and signing pair, rebuild the image
from a trusted revision, inspect Kubernetes audit logs for the control-plane
service account, and review every Agent/PVC. Namespace RBAC limits direct
Kubernetes authority, but it does not make a compromised control plane safe.
