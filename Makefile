SHELL := /bin/sh

GO ?= go
DOCKER ?= docker
VERSION ?= dev
COMMIT ?= $(shell git rev-parse --short=12 HEAD 2>/dev/null || printf unknown)
BUILD_DATE ?= unknown
BINARIES := control-plane runner herdr-bridge workspace-bootstrap codex-auth codex-launcher smoke
CONTAINER_TARGETS := control-plane runner herdr-bridge workspace-bootstrap agent-runtime codex-runtime herdr-gateway
KUSTOMIZE_VERSION ?= v5.8.1
GOVULNCHECK_VERSION ?= v1.9.0
AGENT_SANDBOX_ROUTER_IMAGE ?= sandherd/agent-sandbox-router:v0.5.3
LDFLAGS := -s -w -X github.com/zjpiazza/sandherd/internal/buildinfo.Version=$(VERSION) -X github.com/zjpiazza/sandherd/internal/buildinfo.Commit=$(COMMIT) -X github.com/zjpiazza/sandherd/internal/buildinfo.Date=$(BUILD_DATE)

.DEFAULT_GOAL := verify

.PHONY: FORCE agent-sandbox-router-container build build-linux chart-check clean container-build contracts fmt fmt-check generate generated-check help lint platform rbac-check release-package smoke test test-race verify vulncheck

help:
	@printf '%s\n' \
		'make verify          Run every local and CI verification check' \
		'make fmt             Format Go source files' \
		'make test            Run unit tests' \
		'make test-race       Run unit tests with race detection' \
		'make vulncheck       Check reachable Go vulnerabilities' \
		'make contracts       Validate OpenAPI and terminal contracts' \
		'make build           Build host binaries into dist/' \
		'make build-linux     Build Linux amd64 and arm64 binaries' \
		'make container-build Build all Sandherd runtime container targets' \
		'make agent-sandbox-router-container Build the pinned upstream router' \
		'make platform        Validate Agent Sandbox deployment manifests' \
		'make rbac-check      Verify live homelab service-account permissions'

fmt:
	gofmt -w cmd internal

fmt-check:
	@files="$$(gofmt -l cmd internal)"; \
	if test -n "$$files"; then printf 'Go files need formatting:\n%s\n' "$$files"; exit 1; fi

lint:
	$(GO) vet ./...

test:
	$(GO) test ./...

test-race:
	$(GO) test -race ./...

vulncheck:
	$(GO) run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

contracts:
	./scripts/validate-contracts.sh

platform:
	KUSTOMIZE_VERSION="$(KUSTOMIZE_VERSION)" ./scripts/validate-platform.sh

chart-check:
	@command -v helm >/dev/null || { printf '%s\n' 'Install Helm 3 with scripts/install-helm.sh and add its tool directory to PATH.' >&2; exit 1; }
	python3 scripts/sync-chart-assets.py --check
	helm lint charts/sandherd --set network.cilium=true
	$(GO) test ./internal/chart

release-package:
	python3 scripts/package-release.py

rbac-check:
	./scripts/check-sandherd-rbac.sh

generate:
	$(GO) generate ./...

generated-check:
	GO="$(GO)" ./scripts/check-generated.sh

build: $(BINARIES:%=dist/%)

dist/%: FORCE
	@mkdir -p dist
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o "$@" "./cmd/$*"

build-linux:
	@mkdir -p dist/linux-amd64 dist/linux-arm64
	@set -e; for arch in amd64 arm64; do \
		for binary in $(BINARIES); do \
			CGO_ENABLED=0 GOOS=linux GOARCH="$$arch" $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o "dist/linux-$$arch/$$binary" "./cmd/$$binary"; \
		done; \
	done

smoke: build
	@for binary in $(BINARIES); do \
		"./dist/$$binary" --version || exit 1; \
	done

container-build:
	@for binary in $(CONTAINER_TARGETS); do \
		$(DOCKER) build --target "$$binary" --tag "sandherd/$$binary:dev" . || exit 1; \
	done

agent-sandbox-router-container:
	$(DOCKER) build \
		--file build/agent-sandbox-router/Dockerfile \
		--tag "$(AGENT_SANDBOX_ROUTER_IMAGE)" \
		.

verify: fmt-check lint test-race vulncheck generated-check contracts platform chart-check smoke build-linux

clean:
	rm -rf dist

FORCE:
