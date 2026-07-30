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
build/agent-sandbox-router/ Pinned upstream router image build
deploy/agent-sandbox/   Kustomize, Flux, policy, and smoke resources
deploy/control-plane/   Agent CRD, API deployment, RBAC, and network policy
deploy/control-plane-homelab/ Cilium overlay for the Talos homelab
internal/api/           Transport-neutral API types
internal/auth/          Authentication and authorization boundary
internal/herdrbridge/   Herdr plugin, API client, and terminal bridge
internal/kubernetes/    Kubernetes and Agent Sandbox adapter boundary
internal/lifecycle/     Agent state transition boundary
internal/runner/        PTY owner, replay hub, leases, and internal runner API
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
make container-build # all three local runtime images
make platform        # render and validate Agent Sandbox manifests
make agent-sandbox-router-container # build the pinned upstream router
```

Each command can report its build metadata without contacting any external
dependency:

```sh
go run ./cmd/control-plane --version
go run ./cmd/runner --version
go run ./cmd/herdr-bridge --version
```

The runtime container targets are `control-plane`, `runner`, and
`herdr-bridge`. They contain only a statically linked binary, run as numeric
user and group `65532:65532`, and intentionally contain neither a shell nor the
Go compiler.

The runner's complete command-line and internal API contract are documented in
the [runner guide](runner.md). The lifecycle API and Kubernetes controller are
documented in the [control-plane guide](control-plane.md).
The plugin manifest, local linking flow, actions, and reconnect behavior are in
the [Herdr integration guide](herdr-integration.md).

## CI and generated files

`.github/workflows/ci.yml` is the repository's single required workflow. It
runs verification, builds both supported Linux architectures, and builds every
container target for amd64 and arm64. Branch protection should require all jobs
from the `CI` workflow.

`make generated-check` snapshots tracked and untracked content, runs all Go
generators, and fails only if generation changes that snapshot. There are no
checked-in generated Go files yet; this guard is in place for future API and
Kubernetes code generation.

The [Agent Sandbox platform guide](platform/agent-sandbox.md) documents the
pinned controller, router build, Kubernetes installation, smoke test, Flux
example, and guarded removal path.
