# syntax=docker/dockerfile:1.7@sha256:a57df69d0ea827fb7266491f2813635de6f17269be881f696fbfdf2d83dda33e

ARG GO_VERSION=1.26.5
ARG CODEX_VERSION=0.146.0

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine@sha256:0178a641fbb4858c5f1b48e34bdaabe0350a330a1b1149aabd498d0699ff5fb2 AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY cmd ./cmd
COPY internal ./internal

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown

RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
    go build -trimpath \
      -ldflags="-s -w -X github.com/zjpiazza/sandherd/internal/buildinfo.Version=$VERSION -X github.com/zjpiazza/sandherd/internal/buildinfo.Commit=$COMMIT -X github.com/zjpiazza/sandherd/internal/buildinfo.Date=$BUILD_DATE" \
      -o /out/control-plane ./cmd/control-plane && \
    CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
    go build -trimpath \
      -ldflags="-s -w -X github.com/zjpiazza/sandherd/internal/buildinfo.Version=$VERSION -X github.com/zjpiazza/sandherd/internal/buildinfo.Commit=$COMMIT -X github.com/zjpiazza/sandherd/internal/buildinfo.Date=$BUILD_DATE" \
      -o /out/runner ./cmd/runner && \
    CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
    go build -trimpath \
      -ldflags="-s -w -X github.com/zjpiazza/sandherd/internal/buildinfo.Version=$VERSION -X github.com/zjpiazza/sandherd/internal/buildinfo.Commit=$COMMIT -X github.com/zjpiazza/sandherd/internal/buildinfo.Date=$BUILD_DATE" \
      -o /out/herdr-bridge ./cmd/herdr-bridge && \
    CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
    go build -trimpath \
      -ldflags="-s -w -X github.com/zjpiazza/sandherd/internal/buildinfo.Version=$VERSION -X github.com/zjpiazza/sandherd/internal/buildinfo.Commit=$COMMIT -X github.com/zjpiazza/sandherd/internal/buildinfo.Date=$BUILD_DATE" \
      -o /out/workspace-bootstrap ./cmd/workspace-bootstrap && \
    CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
    go build -trimpath \
      -ldflags="-s -w -X github.com/zjpiazza/sandherd/internal/buildinfo.Version=$VERSION -X github.com/zjpiazza/sandherd/internal/buildinfo.Commit=$COMMIT -X github.com/zjpiazza/sandherd/internal/buildinfo.Date=$BUILD_DATE" \
      -o /out/codex-auth ./cmd/codex-auth && \
    CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
    go build -trimpath \
      -ldflags="-s -w -X github.com/zjpiazza/sandherd/internal/buildinfo.Version=$VERSION -X github.com/zjpiazza/sandherd/internal/buildinfo.Commit=$COMMIT -X github.com/zjpiazza/sandherd/internal/buildinfo.Date=$BUILD_DATE" \
      -o /out/codex-launcher ./cmd/codex-launcher

FROM --platform=$BUILDPLATFORM alpine:3.23.5@sha256:fd791d74b68913cbb027c6546007b3f0d3bc45125f797758156952bc2d6daf40 AS codex-download
ARG CODEX_VERSION
ARG TARGETARCH
COPY build/codex/checksums.txt /tmp/codex-checksums.txt
RUN apk add --no-cache ca-certificates curl zstd && \
    case "$TARGETARCH" in \
      amd64) target=x86_64-unknown-linux-musl ;; \
      arm64) target=aarch64-unknown-linux-musl ;; \
      *) printf 'Unsupported Codex architecture: %s\n' "$TARGETARCH" >&2; exit 1 ;; \
    esac && \
    asset="codex-${target}.zst" && \
    curl --fail --location --silent --show-error \
      "https://github.com/openai/codex/releases/download/rust-v${CODEX_VERSION}/${asset}" \
      --output "/tmp/${asset}" && \
    grep "  ${asset}$" /tmp/codex-checksums.txt | sed 's#  #  /tmp/#' | sha256sum -c - && \
    zstd --decompress "/tmp/${asset}" -o /codex && \
    chmod 0755 /codex

FROM scratch AS runtime
USER 65532:65532

FROM runtime AS control-plane
COPY --from=build /out/control-plane /sandherd
ENTRYPOINT ["/sandherd"]

FROM runtime AS runner
COPY --from=build /out/runner /sandherd
ENTRYPOINT ["/sandherd"]

FROM runtime AS herdr-bridge
COPY --from=build /out/herdr-bridge /sandherd
ENTRYPOINT ["/sandherd"]

FROM runtime AS workspace-bootstrap
COPY --from=build /out/workspace-bootstrap /sandherd
ENTRYPOINT ["/sandherd"]

FROM alpine:3.23.5@sha256:fd791d74b68913cbb027c6546007b3f0d3bc45125f797758156952bc2d6daf40 AS agent-runtime
RUN apk add --no-cache bash ca-certificates git openssh-client
COPY --from=build /out/runner /usr/local/bin/sandherd-runner
COPY --from=build /out/workspace-bootstrap /usr/local/bin/workspace-bootstrap
USER 65532:65532
WORKDIR /workspace
ENTRYPOINT ["/usr/local/bin/sandherd-runner"]

FROM agent-runtime AS codex-runtime
ARG CODEX_VERSION
COPY --from=build /out/codex-auth /usr/local/bin/codex-auth
COPY --from=build /out/codex-launcher /usr/local/bin/codex-launcher
COPY --from=codex-download /codex /usr/local/bin/codex
ENV CODEX_VERSION=$CODEX_VERSION \
    CODEX_HOME=/home/sandherd/.codex
