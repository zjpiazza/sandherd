# syntax=docker/dockerfile:1.7@sha256:a57df69d0ea827fb7266491f2813635de6f17269be881f696fbfdf2d83dda33e

ARG GO_VERSION=1.26.5

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
      -o /out/herdr-bridge ./cmd/herdr-bridge

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
