# Build stage. It runs on the machine that builds, whatever the image is for:
# Go cross-compiles to TARGETARCH, so a multi-platform build emulates nothing
# here. Only the runtime stage's few commands run under QEMU.
#
# The playground UI the binary embeds (//go:embed all:playground/dist) is the
# one committed in playground/dist, as it is for `go install` and the release
# binaries. It is not rebuilt here: CI's "Web UI dist freshness" job and the
# release workflow's gate (scripts/check-webui-dist.sh) hold it to a fresh
# build, byte for byte.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS builder

ARG TARGETOS=linux
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_DATE=unknown

WORKDIR /build

# Copy go mod files
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

# Copy source code
COPY . .

# Build the binary with version info
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
    -ldflags "-s -w \
      -X github.com/qumo-dev/qumo/internal/version.version=${VERSION} \
      -X github.com/qumo-dev/qumo/internal/version.commit=${COMMIT} \
      -X github.com/qumo-dev/qumo/internal/version.date=${BUILD_DATE}" \
    -o qumo .

# Runtime stage
FROM alpine:latest

ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_DATE=unknown

# OCI image labels
LABEL org.opencontainers.image.title="qumo" \
      org.opencontainers.image.description="MoQT Relay" \
      org.opencontainers.image.url="https://github.com/qumo-dev/qumo" \
      org.opencontainers.image.source="https://github.com/qumo-dev/qumo" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.created="${BUILD_DATE}" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.licenses="MIT"

# Install runtime dependencies. apk upgrade pulls patched releases of base
# packages (e.g. OpenSSL CVE fixes) even when the floating alpine:latest base
# digest predates the fix.
RUN apk --no-cache upgrade && apk --no-cache add ca-certificates tzdata wget

WORKDIR /app

# Copy binary from builder
COPY --from=builder /build/qumo .

# Create certs directory
RUN mkdir -p certs

# Expose ports
# 4433: MoQT (QUIC) and HTTP health/metrics (TCP)
EXPOSE 4433/udp 4433

# Health check
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD wget --no-verbose --tries=1 --spider http://localhost:4433/health || exit 1

ENTRYPOINT ["/app/qumo"]

# Default command
CMD ["relay"]