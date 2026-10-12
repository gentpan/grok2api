# Go toolchain: floating 1.26 tag so the image always carries a 1.26.x patch
# release satisfying the `go 1.26.4` directive in go.mod (official golang
# images use GOTOOLCHAIN=local, so an older patch would refuse to build).
# Pin an exact patch with: --build-arg GO_VERSION=1.26.4
ARG GO_VERSION=1.26
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS builder
ARG TARGETOS TARGETARCH
ARG VERSION=dev
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w -X main.Version=${VERSION}" -o grok2api .

FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata su-exec \
    && addgroup -g 10001 -S app \
    && adduser -u 10001 -S -G app -h /app -s /sbin/nologin app
WORKDIR /app
COPY --from=builder /app/grok2api .

# Runtime state lives in two directories; both are created owned by `app`.
ENV SERVER_PORT=8000 \
    DATA_DIR=/app/data \
    LOG_DIR=/app/logs
RUN mkdir -p /app/data /app/logs && chown -R app:app /app

# The entrypoint (see docker-entrypoint.sh) starts as root only to repair
# ownership of bind-mounted volumes, then drops to the unprivileged `app` user.
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod 0755 /usr/local/bin/docker-entrypoint.sh

VOLUME ["/app/data"]
EXPOSE 8000
# Probe the address the server is actually bound to: SERVER_HOST may be a
# specific IP, and 0.0.0.0 / empty means "all interfaces" -> use loopback.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD h="${SERVER_HOST:-127.0.0.1}"; [ "$h" = "0.0.0.0" ] && h=127.0.0.1; \
      wget -qO- "http://${h}:${SERVER_PORT:-8000}/health" >/dev/null || exit 1
ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh", "./grok2api"]
