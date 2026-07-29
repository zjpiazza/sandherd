# Contributing to Sandherd

Sandherd is pre-alpha. Please use an issue to agree on the boundary of a
substantial change before investing in its implementation.

## Development setup

Install the tools listed in [the development guide](docs/development.md), then
run the same verification entry point used by CI:

```sh
make verify
```

Container changes should also pass:

```sh
make container-build
```

## Branches and commits

- Start from the current `main` branch.
- Name issue branches `issue-<number>-<short-description>`, for example
  `issue-2-go-monorepo`.
- Keep commits focused and use imperative subjects, such as
  `Scaffold Go commands and build tooling`.
- Do not commit generated build output, editor settings, credentials, or local
  environment files.

## Tests

- Add or update unit tests with behavior changes.
- Run `make verify` before pushing. It checks formatting, static analysis,
  tests, generated-file cleanliness, API contracts, and Linux builds.
- Run `make container-build` when changing commands, dependencies, or the
  `Dockerfile`.
- CI must pass on both Linux amd64 and arm64-compatible builds.

## Pull requests

- Link the issue in the PR body with `Closes #<number>` when the PR completes
  the issue.
- Explain what changed, why it belongs in this ticket, and how it was tested.
- Keep unrelated cleanup in a separate issue and PR.
- Mark breaking contract changes clearly and update the relevant ADR.

The repository uses the single `CI` workflow as its required pull-request
workflow. All of its jobs must pass before merge.
