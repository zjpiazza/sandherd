# Terminal gateway

The control-plane process exposes `GET /v1alpha1/agents/{agentId}/terminal`
and proxies the negotiated `sandherd.terminal.v1alpha1` WebSocket to the
agent's current runner. Public clients use only the logical Agent ID; sandbox,
Pod, namespace, port, and router credentials stay inside the gateway.

```text
client bearer credential
        |
        v
Sandherd API + terminal gateway
        |  Kubernetes service-account token (router only)
        |  30-second Ed25519 capability (runner only)
        v
Agent Sandbox router
        |
        v
runner + persistent PTY
```

The gateway resolves a running Agent through its deterministic
`SandboxClaim`, sends the router's `X-Sandbox-*` headers, and authenticates to
the router with its projected Kubernetes service-account token. The router
consumes and strips that bearer token. A separate `X-Sandherd-Capability`
header reaches the runner. Its signed claims bind the connection to one Agent,
one public permission level, one request ID, and a short expiration time.

The private Ed25519 key exists only in the control-plane Pod. Runners receive
only the public key, so a compromised sandbox can verify capabilities but
cannot mint one for itself or another agent.

## Streaming behavior

The gateway validates only the initial attachment metadata needed for public
authorization. It then forwards terminal JSON messages unchanged; it never
decodes, stores, logs, or interprets terminal payload bytes. The runner remains
the authority for controller leases, ordered output, replay cursors, observer
fan-out, and atomic takeover.

Each direction has one reader and one writer with no unbounded intermediate
queue. The default limits are 512 concurrent connections globally, 16 per
Agent, 1 MiB per message, 10 seconds for the initial attach, 5 seconds per
write, 15 minutes idle, and a 12-hour maximum connection lifetime. Replay is
separately bounded by the runner's byte and frame limits. A gateway process
restart closes attachments but does not signal the runner, so clients reconnect
with their last acknowledged `afterSequence`.

An optional observe-only API credential may list and inspect agents, consume
events, and attach as an observer. It cannot create, mutate, delete, control, or
take over an agent. The runner independently enforces the same permission from
the signed capability.

## Key setup

Generate one signing key pair outside the repository:

```sh
umask 077
openssl genpkey -algorithm Ed25519 -out capability-private-key.pem
openssl pkey -in capability-private-key.pem -pubout -out capability-public-key.pem

kubectl --context admin@homelab --namespace sandherd-system \
  create secret generic sandherd-runner-capability-signing-key \
  --from-file=private-key.pem=capability-private-key.pem

kubectl --context admin@homelab --namespace sandherd-system \
  create configmap sandherd-runner-capability-key \
  --from-file=public-key.pem=capability-public-key.pem
```

Mount the ConfigMap's public key into every runner and start it with:

```text
--auth-token-file=
--capability-public-key-file=/var/run/sandherd/capability-public-key.pem
```

Static runner bearer tokens remain available for local development, but are
not required on the gateway path. The sandbox template integration owns the
public-key mount and runner arguments.

## Operations

`GET /metrics` exposes active and total connections, aggregate duration,
wire-byte counts, replay gaps, successful takeovers, and failures. Labels and
logs contain logical Agent IDs, roles, request IDs, and trace context, never
terminal content or credentials. A successful takeover also publishes an
`agent.controller_taken_over` lifecycle event.

The control-plane network policy permits egress only to DNS, the Kubernetes
API, and port 8080 on Agent Sandbox router Pods. Public ingress remains a
separate deployment concern; Tailscale should expose the control-plane service,
not individual sandboxes or runners.
