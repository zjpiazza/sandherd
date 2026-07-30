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
the configured generic Codex specification. Attach focuses an existing bridge
pane when possible; otherwise it opens a new pane. Takeover explicitly opens a
new controller and lets the runner atomically revoke the previous lease.

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

## Uninstall local development link

```sh
herdr plugin unlink dev.sandherd
```

Unlinking leaves the working tree and plugin-owned configuration/state files in
place. A GitHub installation can instead be removed with `herdr plugin
uninstall dev.sandherd`.
