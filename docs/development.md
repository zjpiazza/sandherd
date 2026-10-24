# Local development

## Toolchain

Sandherd requires Go 1.26.0 or newer and selects Go 1.26.5 through the module's
`toolchain` directive. Contract checks require Node.js 24 and download their
pinned validators with `npx`. Container builds require Docker with BuildKit.

## Repository layout

```text
api/                    REST and terminal protocol source contracts
cmd/control-plane/      Client-neutral lifecycle API entry point
cmd/runner/             In-sandbox agent process entry point
cmd/herdr-bridge/       Herdr integration entry point
cmd/workspace-bootstrap/ Durable workspace initializer entry point
build/agent-sandbox-router/ Pinned upstream router image build
deploy/agent-sandbox/   Kustomize, Flux, policy, and smoke resources
deploy/agent-runtime/   Standard runtime template and credential profiles
deploy/control-plane/   Agent CRD, API deployment, RBAC, and network policy
deploy/control-plane-homelab/ Cilium overlay for the Talos homelab
internal/api/           Transport-neutral API types
internal/auth/          Authentication and authorization boundary
internal/bootstrap/     Safe, idempotent durable workspace bootstrap
internal/herdrbridge/   Herdr plugin, API client, and terminal bridge
internal/kubernetes/    Kubernetes and Agent Sandbox adapter boundary
internal/lifecycle/     Agent state transition boundary
internal/runner/        PTY owner, replay hub, leases, and internal runner API
internal/runtimeadapter/ Trusted installed-adapter registry and launch resolution
internal/terminal/      Reconnectable terminal protocol boundary
docs/adr/               Architecture decision records
docs/platform/          Cluster installation and validation records
scripts/                Repository validation scripts
```

Go's `internal` visibility rule prevents packages outside this module from
depending on implementation details. Clients integrate through the published
REST and terminal contracts, not by importing these packages.

## Common commands

```sh
make verify          # every check required by CI
make fmt             # format Go source
make test            # unit tests
make test-race       # unit tests with the race detector
make contracts       # OpenAPI and terminal schema validation
make build           # host binaries in dist/
make build-linux     # amd64 and arm64 Linux binaries in dist/
make container-build # all local runtime images
make platform        # render and validate Agent Sandbox manifests
make agent-sandbox-router-container # build the pinned upstream router
```

Each command can report its build metadata without contacting any external
dependency:

```sh
go run ./cmd/control-plane --version
go run ./cmd/runner --version
go run ./cmd/herdr-bridge --version
go run ./cmd/workspace-bootstrap --version
```

The minimal runtime container targets are `control-plane`, `runner`,
`herdr-bridge`, and `workspace-bootstrap`. They contain only a statically linked binary, run as numeric
user and group `65532:65532`, and intentionally contain neither a shell nor the
Go compiler.

`agent-runtime` supplies a shell and Git for agent workspaces. `codex-runtime`
adds the pinned Codex CLI and credential helpers. `herdr-gateway` supplies the
private SSH/Herdr endpoint. The host and Linux binary builds also include
`codex-auth`, `codex-launcher`, and the installed-system `smoke` command.

## Installed-system verification

The `smoke` binary creates a disposable shell Agent through the public API,
writes a workspace canary through the terminal, disconnects while output is
produced, checks replay after reconnect, stops/resumes, verifies persistence,
and deletes the Agent. It exits unsuccessfully if test cleanup fails. Use an
existing control principal token file; never pass tokens as command arguments.

```sh
./dist/smoke --base-url https://sandherd.example \
  --token-file /secure/path/sandherd-api-token
```

For an operator regression test, add `--recovery-context admin@homelab`.
This option uses Kubernetes access to modify only the disposable test Agent's
stopped Sandbox: its next Pod exits successfully, and the control plane must
replace that completed Pod automatically. The test checks the workspace PVC
identity, verifies runner CPU/memory requests and limits before and after
recovery, and waits for the test claim, Sandbox, Pod, PVC, and derived runtime
templates/pools to be deleted. Use `--cpu 750m --memory 768Mi` to test budgets
different from the source-template defaults.
Use `--namespace` when the installation is outside `sandherd-system`.

This test verifies the shared lifecycle and terminal infrastructure without a
model provider. Each coding-agent adapter also needs its own inference,
credential, and native-session acceptance tests.

The runner's complete command-line and internal API contract are documented in
the [runner guide](runner.md). The lifecycle API and Kubernetes controller are
documented in the [control-plane guide](control-plane.md).
Installed runtime configuration and the no-core-branch contributor flow are in
the [adapter guide](adapters.md).
The plugin manifest, local linking flow, actions, and reconnect behavior are in
the [Herdr integration guide](herdr-integration.md).

## CI and generated files

`.github/workflows/ci.yml` is the repository's single required workflow. It
runs verification, builds both supported Linux architectures, and builds every
container target for amd64 and arm64. Branch protection should require all jobs
from the `CI` workflow.

`make verify` includes a pinned `govulncheck` scan of reachable Go symbols across
all commands. The separate Codex image scan also checks its OS packages. Both
are needed: scanning only an agent image does not cover control-plane modules.

Trusted `release/**` branches also publish all runtime images to GHCR, tagged
`sha-<full Git commit>`, after repository verification and the Codex
provenance/vulnerability check pass. Every image includes amd64 and arm64
manifests, build provenance, and an SBOM. Pull-request and `main` builds do not
publish images. Deploy using the resulting immutable digest, not a mutable
branch tag. These image builds are not yet a supported packaged release; the
Helm chart and install/upgrade acceptance are tracked in issue #10.

`make generated-check` snapshots tracked and untracked content, runs all Go
generators, and fails only if generation changes that snapshot. There are no
checked-in generated Go files yet; this guard is in place for future API and
Kubernetes code generation.

The [Agent Sandbox platform guide](platform/agent-sandbox.md) documents the
pinned controller, router build, Kubernetes installation, smoke test, Flux
example, and guarded removal path.
