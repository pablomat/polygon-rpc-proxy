# syntax=docker/dockerfile:1

# ---- build stage ------------------------------------------------------------
# Pinned by digest to prevent unexpected base-image tag drift.
# Run the builder on its native platform and cross-compile to the requested
# target architecture.
FROM --platform=$BUILDPLATFORM golang:1.26.8-bookworm@sha256:a688600ca24f8a4d3ca77f95b0dd40704a9fc787c826660eb7ba0b641b8b175d AS build

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
FROM gcr.io/distroless/static:nonroot@sha256:f7f8f729987ad0fdf6b05eeeae94b26e6a0f613bdf46feea7fc40f7bd72953e6

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
