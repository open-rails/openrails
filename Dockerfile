# Stage 1: admin console SPA (#754 — dist is never committed; the image build
# owns the embed). Node is a BUILD-time dependency only.
FROM node:22-alpine AS console

WORKDIR /web/admin
COPY web/admin/package.json web/admin/package-lock.json ./
RUN npm ci
COPY web/admin/ ./
RUN npm run build -- --outDir /out --emptyOutDir


# Stage 2: build
FROM golang:1.26.5-alpine AS builder

# Install build dependencies
RUN apk add --no-cache git ca-certificates

WORKDIR /app

ARG GOPROXY=https://proxy.golang.org,direct
ARG GOSUMDB=sum.golang.org
ENV GOPROXY=${GOPROXY}
ENV GOSUMDB=${GOSUMDB}
ENV GIT_TERMINAL_PROMPT=0

# Copy go mod files first for better caching
COPY go.mod go.sum ./

# Download dependencies with cache mount for Go modules (with retry).
# All dependencies are public, so no authentication is required.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    set -eu; \
    for i in 1 2 3; do \
      go mod download && break || (echo "go mod download failed, retrying" && sleep 5); \
    done

# Copy source code
COPY . .
# The console build lands at the binary-boundary embed package; the tagged
# build below links it (a console-less image would just drop the tag).
COPY --from=console /out ./cmd/openrails/consoleassets/dist

# Build the application with cache mount
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    mkdir -p bin && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -tags console_assets -o bin/openrails ./cmd/openrails


# Stage 3: production
FROM alpine:3.19

# Install runtime dependencies (include wget for healthcheck)
RUN apk --no-cache add ca-certificates tzdata wget

WORKDIR /app

# Create non-root user
RUN addgroup -g 1001 -S billing && \
    adduser -S -D -H -u 1001 -s /sbin/nologin -G billing billing && \
    mkdir -p /var/lib/openrails/spool && \
    chown -R billing:billing /var/lib/openrails

# Copy binary and migrations from builder stage.
COPY --from=builder /app/bin/openrails /usr/local/bin/openrails
COPY --from=builder /app/migrations ./migrations/

# Configuration files must be mounted at runtime; none are baked into the image.

# Change ownership to non-root user
RUN chown -R billing:billing /app

# Switch to non-root user
USER billing

ENV GIN_MODE=release \
    TZ=UTC

# Expose the single public port. Server-to-server calls use OpenRails-issued
# API keys on this same port; there is no separate private/mTLS service port (#222).
EXPOSE 3053

# Health check
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD wget --no-verbose --tries=1 -O /dev/null http://localhost:3053/health/ready || exit 1

# Default entrypoint runs the CLI; override CMD to choose server vs worker.
ENTRYPOINT ["openrails"]
CMD ["run-server"]
