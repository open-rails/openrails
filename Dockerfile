# Digest-pinned Docker official images. Override BASE_REGISTRY with
# public.ecr.aws/docker/library if the default mirror is unavailable.
ARG BASE_REGISTRY=mirror.gcr.io/library

# Stage 1: admin console SPA into server/web/console/dist, which server/web/console/embed.go
# go:embeds (#754). Node is a BUILD-time dependency only. Its output is static
# files, so it runs once on the build platform for every target platform.
FROM --platform=$BUILDPLATFORM ${BASE_REGISTRY}/node:22-alpine@sha256:c610fcdfb1d5b4740dd70c284ed3cb16bb857e0f7166196e36a5501df7a3aa32 AS billing-ui

RUN npm install -g --ignore-scripts pnpm@11.0.0
WORKDIR /src/sdk/billing-ui
COPY sdk/billing-ui/ ./
RUN pnpm install --frozen-lockfile --ignore-scripts && pnpm --config.verify-deps-before-run=false run build
FROM billing-ui AS console
WORKDIR /src/server/web/console
COPY server/web/console/ ./
RUN pnpm install --frozen-lockfile --ignore-scripts && pnpm --config.verify-deps-before-run=false run build


# Stage 1b: the payment page into web/pay/dist, built from this
# checkout's billing-ui.
FROM billing-ui AS payment-page

WORKDIR /src/web/pay
COPY web/pay/ ./
RUN pnpm install --frozen-lockfile --ignore-scripts && pnpm --config.verify-deps-before-run=false run build


# Stage 2: build. Cross-compiles on the build platform (the binary needs no cgo),
# so only the runtime stage runs as the target platform.
FROM --platform=$BUILDPLATFORM ${BASE_REGISTRY}/golang:1.26.10-alpine@sha256:9ce231b28998b32e116927c61aa5c406183061d4f3f2d61b292c88e7f6bdf7ca AS builder

# Install build dependencies
RUN apk add --no-cache git ca-certificates

WORKDIR /app

ARG GOPROXY=https://proxy.golang.org,direct
ARG GOSUMDB=sum.golang.org
ENV GOPROXY=${GOPROXY}
ENV GOSUMDB=${GOSUMDB}
ENV GIT_TERMINAL_PROMPT=0

# Copy go mod files first for better caching. The binary is the server
# module's (server/), which builds against the root's source (its replace).
COPY go.mod go.sum ./
COPY server/go.mod server/go.sum ./server/
WORKDIR /app/server

# Download dependencies with cache mount for Go modules (with retry).
# All dependencies are public, so no authentication is required.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    set -eu; \
    for i in 1 2 3; do \
      go mod download && break || (echo "go mod download failed, retrying" && sleep 5); \
    done

# Copy source code, then the console build into the dir go:embed reads.
COPY . /app
COPY --from=console /src/server/web/console/dist /app/server/web/console/dist
COPY --from=payment-page /src/web/pay/dist /app/web/pay/dist

# Release identity (internal/buildinfo); unset falls back to "dev".
ARG VERSION=
ARG COMMIT=
ARG DATE=
ARG TARGETOS
ARG TARGETARCH

# Build the application with cache mount
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    test -f web/console/dist/index.html && test -f ../web/pay/dist/index.html && \
    mkdir -p ../bin && \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath \
      -ldflags "-s -w -X github.com/open-rails/openrails/internal/buildinfo.version=${VERSION} -X github.com/open-rails/openrails/internal/buildinfo.commit=${COMMIT} -X github.com/open-rails/openrails/internal/buildinfo.date=${DATE}" \
      -o ../bin/openrails ./cmd/openrails


# Stage 3: production
# 2026-10-21: bumped for libssl3/libcrypto3 3.5.7-r0 -> 3.5.8-r0 (CVE-2026-14456
# and 7 other openssl CVEs, see #1293).
FROM ${BASE_REGISTRY}/alpine:3.24@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6

LABEL org.opencontainers.image.source="https://github.com/open-rails/openrails"

# Install runtime dependencies (include wget for healthcheck)
RUN apk --no-cache add ca-certificates tzdata wget

# Alpine v3.24 publishes zlib 1.3.2-r1 (CVE-2026-85091) and musl 1.2.6-r3
# (CVE-2026-107891) before this pinned base includes them.
RUN apk upgrade --no-cache zlib musl musl-utils

WORKDIR /app

# Create non-root user
RUN addgroup -g 1001 -S billing && \
    adduser -S -D -H -u 1001 -s /sbin/nologin -G billing billing && \
    mkdir -p /vault/auth && \
    chown -R billing:billing /vault/auth && \
    chmod 0700 /vault/auth

# Copy the binary; database migrations are embedded in it.
COPY --from=builder /app/bin/openrails /usr/local/bin/openrails

# Configuration files must be mounted at runtime; none are baked into the image.

# Change ownership to non-root user
RUN chown -R billing:billing /app

# Switch to non-root user
USER billing

ENV TZ=UTC

# Expose the single public port. Server-to-server calls use OpenRails-issued
# API keys on this same port; there is no separate private/mTLS service port (#222).
EXPOSE 3053

# Health check
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD wget --no-verbose --tries=1 -O /dev/null http://localhost:3053/health/ready || exit 1

# Default entrypoint runs the CLI; override CMD to choose server vs worker.
ENTRYPOINT ["openrails"]
CMD ["run-server"]
