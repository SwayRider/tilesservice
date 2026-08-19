# syntax=docker/dockerfile:1.4
FROM --platform=$BUILDPLATFORM golang:1.26-bookworm AS builder

ARG TARGETOS
ARG TARGETARCH

# tilesservice depends on github.com/mattn/go-sqlite3 (MBTiles + disk cache),
# which requires cgo, so the cross-compilation toolchains are kept.
RUN apt-get update && apt-get install -y gcc
RUN if [ "${TARGETARCH}" = "arm64" ]; then apt-get install -y gcc-aarch64-linux-gnu; fi
RUN if [ "${TARGETARCH}" = "amd64" ]; then apt-get install -y gcc-x86-64-linux-gnu; fi

ENV CGO_ENABLED=1
ENV GOOS=${TARGETOS}
ENV GOARCH=${TARGETARCH}

RUN mkdir /app
WORKDIR /app

COPY . .

RUN if [ "${TARGETARCH}" = "amd64" ]; then \
        export CC=x86_64-linux-gnu-gcc && \
        export CXX=x86_64-linux-gnu-g++ && \
        export CGO_ENABLED=1; \
    elif [ "${TARGETARCH}" = "arm64" ]; then \
        export CC=aarch64-linux-gnu-gcc && \
        export CXX=aarch64-linux-gnu-g++ && \
        export CGO_ENABLED=1; \
    else \
        echo "Unknown architecture" && exit 1; \
    fi && \
    go clean -modcache && \
    go mod download && \
    go build -o tilesservice ./cmd/tilesservice/main.go

# Runtime stage. Pinned to a dated bookworm tag (rather than the mutable
# bookworm-slim alias) so builds are reproducible.
FROM --platform=$TARGETPLATFORM debian:bookworm-20260805-slim
RUN apt-get update && apt-get install -y --no-install-recommends curl && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY --from=builder /app/tilesservice .
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /app/assets/map/styles ./assets/map/styles
ENV STYLES_PATH=/app/assets/map/styles
HEALTHCHECK --interval=30s --timeout=5s --start-period=30s --retries=3 \
    CMD curl -fsS http://localhost:8080/v1/tiles/ping || exit 1
CMD ["./tilesservice"]
