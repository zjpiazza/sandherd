# syntax=docker/dockerfile:1.7@sha256:a57df69d0ea827fb7266491f2813635de6f17269be881f696fbfdf2d83dda33e

ARG GO_VERSION=1.26.2

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}@sha256:b54cbf583d390341599d7bcbc062425c081105cc5ef6d170ced98ef9d047c716 AS build
WORKDIR /src

ADD --checksum=sha256:5c154ff2525bcad4564744e052af1a527465fff4047c00a007fda1beb8e0e7e8 \
    https://github.com/kubernetes-sigs/agent-sandbox/archive/f9cf22a0936390a120353380fed80392278de6b6.tar.gz \
    /tmp/agent-sandbox.tar.gz
RUN tar --extract --gzip --file=/tmp/agent-sandbox.tar.gz --strip-components=1

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=v0.5.3
ARG COMMIT=f9cf22a0936390a120353380fed80392278de6b6
ARG BUILD_DATE=unknown

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go mod download && \
    CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
    go build -trimpath \
      -ldflags="-s -w -X sigs.k8s.io/agent-sandbox/internal/version.gitVersion=$VERSION -X sigs.k8s.io/agent-sandbox/internal/version.gitSHA=$COMMIT -X sigs.k8s.io/agent-sandbox/internal/version.buildDate=$BUILD_DATE" \
      -o /out/sandbox-router ./sandbox-router/cmd

FROM scratch
COPY --from=build /out/sandbox-router /sandbox-router
USER 65532:65532
EXPOSE 8080 8081 9090
ENTRYPOINT ["/sandbox-router"]
