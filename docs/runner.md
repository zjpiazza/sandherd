# In-sandbox runner

The runner owns exactly one agent process and its pseudo-terminal. Client
attachments are intentionally disposable: closing every terminal connection
does not signal, stop, or otherwise change the process.

The runner API is internal to an Agent Sandbox. The terminal gateway is its
production caller; end-user clients use the public Sandherd API instead.

The standard SandboxTemplate starts the runner only after the unprivileged
[`workspace-bootstrap`](workspaces.md) init container has prepared the durable
repository. Bootstrap credentials are not mounted into the runner container.

## Starting a runner

Create a control token in a mode-restricted file and pass the agent command
after `--`:

```sh
go run ./cmd/runner \
  --agent-id 019c1234-1234-7123-8123-123456789abc \
  --listen 127.0.0.1:8080 \
  --auth-token-file /run/sandherd/control-token \
  --working-directory /workspace \
  -- codex
```

The most relevant limits are:

| Flag | Default | Purpose |
| --- | --- | --- |
| `--replay-bytes` | 4 MiB | Maximum retained terminal payload bytes |
| `--replay-frames` | 4096 | Maximum retained terminal output chunks |
| `--subscriber-buffer` | 256 | Maximum queued live frames per attachment |
| `--controller-lease` | 30 seconds | Controller liveness period |
| `--stop-grace` | 10 seconds | Delay between `SIGTERM` and `SIGKILL` |
| `--columns`, `--rows` | 120, 40 | Initial PTY dimensions |

An optional `--observe-token-file` creates a credential that can read metadata
and terminal output but cannot acquire control, take over, send input, resize,
signal, or stop the process. Tokens are read only from files and are never
written to logs.

For the production gateway path, mount the control plane's Ed25519 public key
and use `--capability-public-key-file`. The gateway sends a 30-second signed
capability in `X-Sandherd-Capability`; the runner verifies its Agent ID, role,
request ID, signature, and expiry. `--auth-token-file=` disables the static
token when capabilities are the only authentication method. A runner may accept
both mechanisms during migration.

## Internal API

The API is HTTP/1.1 with bearer authentication except for probes:

| Method and path | Authentication | Behavior |
| --- | --- | --- |
| `GET /healthz` | None | Reports that the runner server is alive |
| `GET /readyz` | None | Reports initialization complete; an exited process remains inspectable |
| `GET /v1alpha1/metadata` | Control or observe | Returns generation, PID, state, timestamps, replay bounds, and exit status |
| `GET /v1alpha1/terminal` | Control or observe | Upgrades to the `sandherd.terminal.v1alpha1` WebSocket protocol |
| `POST /v1alpha1/signal` | Control | Sends `SIGHUP`, `SIGINT`, `SIGQUIT`, `SIGTERM`, `SIGUSR1`, or `SIGUSR2` to the process group |
| `POST /v1alpha1/stop` | Control | Starts graceful termination and returns `202 Accepted` |

The terminal endpoint implements the published [terminal protocol](../api/terminal.md).
The first message must be `attach`. A control-capable caller may attach as an
observer and later request takeover; an observe-only credential receives
`forbidden_role`. A successful takeover atomically replaces the lease before
the old controller is notified, so two attachments can never write concurrently.

Each PTY read becomes one contiguous `output` sequence. Attaching with
`afterSequence` atomically snapshots replay and subscribes to live output, which
prevents gaps or duplicates at the handoff. An expired cursor receives
`replay_gap`. A client that fills its bounded live queue is closed with
`slow_consumer`; it cannot consume memory without limit or block the agent.

## Process and restart semantics

- An attachment disconnect only releases that attachment and its controller
  lease. The process remains running.
- When the agent exits, the runner remains available and delivers one `exit`
  frame per attachment with either its exit code or terminating signal. It does
  not restart the agent inside the same runner generation.
- A runner restart creates a new generation, starts the configured command once,
  resets terminal sequence numbers, and loses the old in-memory replay buffer.
- On runner `SIGINT` or `SIGTERM`, the entire agent process group first receives
  `SIGTERM`. It receives `SIGKILL` only after `--stop-grace` expires.

## Sandbox security requirements

The published runner image executes as numeric user and group `65532:65532` in a
scratch filesystem. Agent Sandbox workloads must also set
`automountServiceAccountToken: false`. As a defense in depth check, the runner
refuses to start if the standard Kubernetes service-account token path exists.

The runner API must remain cluster-internal and be restricted to the gateway by
network policy. The scoped capability authenticates that hop; it is not public
user authentication. Logs contain process IDs, attachment IDs, state changes,
request and trace IDs, and errors, but never terminal payloads or credentials.
