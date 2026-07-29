# Terminal protocol v1alpha1

Sandherd terminal attachments use WebSockets with the negotiated subprotocol:

```text
sandherd.terminal.v1alpha1
```

Every text WebSocket message contains exactly one JSON frame validated by [`terminal.schema.json`](terminal.schema.json). Terminal bytes inside `input` and `output` frames are base64 so the protocol can carry arbitrary terminal data without assuming UTF-8.

## Connection sequence

1. The client opens `GET /v1alpha1/agents/{agentId}/terminal` with its Sandherd bearer credential and offers the required WebSocket subprotocol.
2. After the HTTP 101 response, the client sends `attach` as its first frame.
3. The server authorizes the requested `control` or `observe` role and responds with `attached`.
4. If `afterSequence` is present, buffered `output` frames after that cursor are replayed before live output.
5. If the cursor predates the buffer, the server sends `replay_gap`, begins at `earliestSequence`, and then continues live.
6. Client acknowledgements report the highest contiguous sequence fully processed. They do not make output durable.

Disconnecting at any point releases only the attachment. It never stops the process, suspends the sandbox, or deletes the Agent.

## Roles and leases

An agent permits one controller and multiple observers.

- A `control` attachment receives a `leaseId` and may send `input` and `resize` frames containing that lease.
- An `observe` attachment never receives a lease and may send only `ack`, `ping`, `pong`, or a `takeover` request.
- A `takeover` request is re-authorized by the server. On success it atomically creates a new lease and sends a new `attached` frame to the requester. The prior controller receives `controller_revoked`.
- Frames using an expired or revoked lease receive an `invalid_controller_lease` error and do not mutate the PTY.

The server may revoke a lease after a configured liveness timeout, explicit agent stop, or successful takeover. Lease expiration does not stop the agent process.

## Output ordering and replay

The runner assigns a monotonically increasing `sequence` to each output chunk within one `runnerGeneration`. Sequence values are contiguous but chunks do not correspond to lines, characters, or terminal escape sequences.

A runner restart creates a new generation and resets its sequence space. The initial `attached` frame identifies the current generation. Clients must discard cached terminal state or obtain a fresh snapshot if the generation differs from the one they last observed.

Replay is bounded by bytes and frame count. A slow consumer is disconnected with `slow_consumer` rather than causing unbounded memory growth. Permanent terminal transcripts are outside v1alpha1.

## Process completion

The server sends exactly one `exit` frame per runner generation when the agent process exits. It contains either `exitCode` or `signal`, never both. Clients may remain connected to inspect replay or reconnect while the Agent lifecycle reconciles the exit.

## Liveness and closure

Either endpoint may send application-level `ping`; the peer answers with `pong` carrying the same nonce. WebSocket protocol ping/pong frames may also be used by infrastructure.

The server sends an `error` frame before closing when possible. Stable error codes include:

| Code | Meaning | Retryable |
| --- | --- | --- |
| `unsupported_protocol` | The offered subprotocol or attach version is unsupported. | No |
| `agent_not_running` | The agent has no attachable process. | Sometimes |
| `forbidden_role` | The principal cannot request the role. | No |
| `invalid_controller_lease` | A mutating frame used a stale or foreign lease. | No |
| `replay_cursor_invalid` | The cursor is ahead of the current generation. | No |
| `slow_consumer` | The connection exceeded its output backlog. | Yes |
| `internal_error` | An internal stream failure occurred. | Yes |

HTTP errors before upgrade use the REST error envelope instead.

## Compatibility

Clients ignore unknown object fields within known frames. They must reject unknown frame `type` values because safely interpreting their direction and terminal effect is impossible. Breaking frame changes receive a new WebSocket subprotocol and `protocolVersion`.

## Examples

Valid examples live in [`examples/terminal`](examples/terminal). Intentionally invalid fixtures live in [`fixtures/terminal-invalid`](fixtures/terminal-invalid) and must fail schema validation.
