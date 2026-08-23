# syntax=docker/dockerfile:1

# ---- build stage ------------------------------------------------------------
# Pinned by digest to prevent unexpected base-image tag drift.
# Run the builder on its native platform and cross-compile to the requested
# target architecture.
FROM --platform=$BUILDPLATFORM golang:1.26.5-bookworm@sha256:1ecb7edf62a0408027bd5729dfd6b1b8766e578e8df93995b225dfd0944eb651 AS build

# Use the toolchain shipped in the pinned builder instead of downloading a
# different Go release during the build.
ENV GOTOOLCHAIN=local \
    CGO_ENABLED=0 \
    GOFLAGS=-mod=readonly

WORKDIR /src

# Download modules and verify them against go.sum before copying source, so
# this layer caches on go.mod/go.sum alone.
COPY go.mod go.sum ./
RUN go mod download && go mod verify

COPY . .

# TARGETOS/TARGETARCH are provided by BuildKit from --platform (default: host).
ARG TARGETOS
ARG TARGETARCH
RUN GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath -ldflags="-s -w" -o /out/proxy ./cmd/proxy

# ---- runtime stage ----------------------------------------------------------
# Distroless static has no shell, package manager, or libc. It is pinned by
# digest, and :nonroot runs as an unprivileged user.
FROM gcr.io/distroless/static:nonroot@sha256:1c2c046bc09ed40fad370b599a0b1ae7987f55b01e247cf27a7c27cd97e5bbc7

LABEL org.opencontainers.image.title="polygon-rpc-proxy" \
      org.opencontainers.image.description="Bounded HTTP JSON-RPC reverse proxy for polygon.drpc.org" \
      org.opencontainers.image.source="https://github.com/pablomat/polygon-rpc-proxy" \
      org.opencontainers.image.licenses="MIT"

COPY --from=build /out/proxy /proxy

EXPOSE 8080
USER nonroot:nonroot

# The binary probes itself (no shell needed in distroless).
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD ["/proxy", "-healthcheck"]

ENTRYPOINT ["/proxy"]
