# syntax=docker/dockerfile:1.7@sha256:a57df69d0ea827fb7266491f2813635de6f17269be881f696fbfdf2d83dda33e

ARG GO_VERSION=1.26.10
ARG CODEX_VERSION=0.146.0

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine@sha256:27f2d3b83cfc5803c8078f8e730af0f7c431bf067afaec8f60a36fb763187cba AS build
WORKDIR /src
ENV GOTOOLCHAIN=local GOFLAGS=-p=2

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY cmd ./cmd
COPY internal ./internal

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=tmpfs,target=/tmp \
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

FROM --platform=$BUILDPLATFORM alpine:3.24.2@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6 AS codex-download
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

FROM --platform=$BUILDPLATFORM alpine:3.24.2@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6 AS herdr-download
ARG TARGETARCH
RUN apk add --no-cache ca-certificates curl && \
    case "$TARGETARCH" in \
      amd64) asset=herdr-linux-x86_64; checksum=2a02fed16beb651ef006e1d43f048f652ca4dc58ad053cd2d44450563d5c54b7 ;; \
      arm64) asset=herdr-linux-aarch64; checksum=f4ccf4de745f2cb9a39a983e9ba3703dad50ec2a58dea83026ceab721bbd8d9e ;; \
      *) printf 'Unsupported Herdr architecture: %s\n' "$TARGETARCH" >&2; exit 1 ;; \
    esac && \
    curl --fail --location --silent --show-error \
      "https://github.com/herdrdev/herdr/releases/download/v0.9.1/${asset}" \
      --output /herdr && \
    printf '%s  /herdr\n' "$checksum" | sha256sum -c - && \
    chmod 0755 /herdr

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

FROM alpine:3.24.2@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6 AS herdr-gateway
RUN apk add --no-cache bash ca-certificates openssh-server openssh-client tini && \
    addgroup -g 65532 sandherd && \
    adduser -D -u 65532 -G sandherd -h /home/sandherd -s /bin/bash sandherd && \
    passwd -d sandherd && \
    mkdir -p /var/empty && chmod 0755 /var/empty
COPY --from=build /out/herdr-bridge /usr/local/bin/sandherd-herdr-bridge
COPY --from=herdr-download /herdr /usr/local/bin/herdr
COPY build/herdr-gateway/sshd_config /etc/ssh/sshd_config
COPY --chmod=0755 build/herdr-gateway/entrypoint.sh /usr/local/bin/herdr-gateway-entrypoint
ENV HOME=/home/sandherd \
    SHELL=/bin/bash \
    XDG_RUNTIME_DIR=/tmp/sandherd-herdr \
    SANDHERD_CONFIG_FILE=/etc/sandherd/config.json \
    HERDR_PLUGIN_STATE_DIR=/home/sandherd/.local/state/sandherd
USER 65532:65532
WORKDIR /home
EXPOSE 2222
ENTRYPOINT ["/sbin/tini", "-g", "--", "/usr/local/bin/herdr-gateway-entrypoint"]

FROM runtime AS workspace-bootstrap
COPY --from=build /out/workspace-bootstrap /sandherd
ENTRYPOINT ["/sandherd"]

FROM alpine:3.24.2@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6 AS agent-runtime
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
