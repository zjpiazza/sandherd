# Herdr integration

The Sandherd plugin makes Herdr the first interactive control surface for the
client-neutral Sandherd API. The plugin never calls Kubernetes, Agent Sandbox,
or the internal router. It knows only a Sandherd base URL, a credential file,
logical Agent IDs, and the public REST and terminal protocols.

## Install

After this repository is public at a released revision, install it directly
from GitHub:

```sh
herdr plugin install zjpiazza/sandherd --ref v0.1.0
```

For local development, build the bridge and link the checkout:

```sh
mkdir -p bin
go build -trimpath -o bin/sandherd-herdr-bridge ./cmd/herdr-bridge
herdr plugin link "$PWD"
```

Herdr validates [`herdr-plugin.toml`](../herdr-plugin.toml), creates private
plugin config and state directories, and exposes the declared actions and pane
entrypoints. The plugin requires Herdr 0.7.4 or newer on Linux or macOS.

## Configure

Ask Herdr for the plugin-owned config directory and copy the example:

```sh
config_dir=$(herdr plugin config-dir dev.sandherd)
cp docs/examples/herdr-config.json "$config_dir/config.json"

mkdir -p "$HOME/.config/sandherd"
printf '%s\n' 'replace-with-the-Sandherd-API-token' > "$HOME/.config/sandherd/api-token"
chmod 600 "$HOME/.config/sandherd/api-token"
```

Edit `config.json` with the Tailscale-reachable Sandherd control-plane URL, the
credential file path, and the default resource/workspace specification used for
new agents. `SANDHERD_CONFIG_FILE`, `SANDHERD_BASE_URL`, and
`SANDHERD_TOKEN_FILE` can override the corresponding values for development.
There is deliberately no kubeconfig setting.

The plugin state directory contains one JSON record per Agent. Records contain
only the logical Agent ID and name, non-secret base URL, remembered Herdr pane
ID, runner generation, and acknowledged output cursor. The API token remains in
the configured credential file and is read for each request so it can rotate.

## Use

Open the action palette or invoke an action directly:

```sh
herdr plugin action invoke dev.sandherd.manage
herdr plugin action invoke dev.sandherd.create
herdr plugin action invoke dev.sandherd.attach
herdr plugin action invoke dev.sandherd.stop
herdr plugin action invoke dev.sandherd.resume
herdr plugin action invoke dev.sandherd.focus
herdr plugin action invoke dev.sandherd.takeover
herdr plugin action invoke dev.sandherd.delete
```

Actions open a small managed picker. Create asks for a DNS-label name and uses
the configured adapter and credential profile. Attach focuses an existing
bridge pane when possible; otherwise it opens a new pane. Takeover explicitly
opens a new controller and lets the runner atomically revoke the previous lease.

On terminals narrower than 100 columns the bridge opens in a new tab instead
of splitting an already narrow phone display. Set `SANDHERD_AGENT_PLACEMENT` to
`split` or `tab` to override this choice.

## Terminal and lifecycle behavior

The pane bridge puts the local TTY into raw mode and forwards arbitrary input,
ANSI sequences, binary bytes, and resize events through
`sandherd.terminal.v1alpha1`. It acknowledges output only after writing it to
the Herdr PTY and stores the last sequence. Network loss, Herdr reattachment,
gateway replacement, and transient sandbox unavailability reconnect with that
cursor. A replay gap is visible in the pane before streaming continues.

The bridge sends an application-level heartbeat every ten seconds so an idle
terminal retains the runner's default thirty-second controller lease. Custom
runner lease durations must leave sufficient margin above that interval.
WebSocket transport pings alone do not renew the controller lease.

Closing or detaching the pane only closes the terminal attachment. It never
calls stop or delete. Those lifecycle mutations happen only through their
explicit actions.

The bridge is the Herdr lifecycle authority for its pane:

- provisioning and reconnecting report `working`;
- successful attachment and quiet output report `idle` (which Herdr presents
  as `done` when background work finishes unseen);
- local input reports `working`;
- controller conflicts, authorization failures, and protocol mismatches report
  `blocked` and emit a Herdr attention notification;
- process exit reports `idle` and emits a completion notification.

Terminal content and credentials are never included in Herdr state, metadata,
notifications, or plugin logs. An unsupported terminal protocol produces a
clear instruction to upgrade the plugin and Sandherd together.

## One cluster machine

Herdr's saved-machine interface connects to a Herdr server over SSH. The
optional [`herdr-gateway`](../deploy/herdr-gateway) deployment supplies that
interface for one Sandherd installation: one machine entry, many agent panes.
It does not run coding agents itself. Each pane runs `sandherd-herdr-bridge`
against the public Sandherd API; agents and their workspace PVCs remain in their
individual Kubernetes sandboxes.

The endpoint runs as UID/GID 65532 with a read-only root filesystem, no
capabilities, no Kubernetes service-account token, and public-key-only SSH on
container port 2222. Its only application egress is to the control plane.
The homelab overlay exposes Service port 22 privately through Tailscale as
`sandherd-herdr` and uses `rook-ceph-block` for a separate 1 GiB HOME PVC.
That PVC stores Herdr layout/session state, not agent repositories or provider
credentials. Losing a gateway connection does not stop the sandbox agents.

Build and publish the endpoint image with the existing container build:

```sh
docker build --target herdr-gateway \
  --tag registry.example/sandherd-herdr-gateway:pilot .
docker push registry.example/sandherd-herdr-gateway:pilot
```

Configure that immutable image reference in a local Kustomize overlay before
applying `deploy/herdr-gateway-homelab`. The endpoint requires an existing
`sandherd-herdr-access` Secret in `sandherd-system` with:

- `authorized_keys`: the operator's SSH public key, never the client private key;
- `ssh_host_ed25519_key`: a dedicated persistent SSH host private key; and
- `api-token`: a Sandherd bearer token scoped to the operator's principal.

The ConfigMap sets `SANDHERD_CONFIG_FILE` and the token file path. Its default
create request selects Codex with `chatgpt-subscription` and a retained 1 GiB
workspace. ChatGPT and OpenRouter provider credentials are not mounted into the
endpoint. Store and rotate the SSH host key outside the repository; pin its
public key on the client before connecting.

Once SSH and the default Herdr session are reachable:

```sh
herdr machine add sandherd@sandherd-herdr \
  --label Cluster --remote-session default
herdr --machine Cluster workspace list
```

Create a workspace/pane on that machine, then run
`sandherd-herdr-bridge attach --agent-id <agent-id>` in its pane. Agent IDs come
from authenticated `GET /v1alpha1/agents`; they are not Kubernetes Pod names.
Use `--takeover` only when intentionally replacing another terminal controller.
Removing the saved machine disconnects the UI without deleting its agents.
Deleting an Agent still follows its own workspace retention policy.

## Uninstall local development link

```sh
herdr plugin unlink dev.sandherd
```

Unlinking leaves the working tree and plugin-owned configuration/state files in
place. A GitHub installation can instead be removed with `herdr plugin
uninstall dev.sandherd`.
