# Stage 1: build
FROM golang:1.25.5-alpine AS builder

# Install build dependencies
RUN apk add --no-cache git ca-certificates

WORKDIR /app

ARG GOPROXY=https://proxy.golang.org,direct
ARG GOPRIVATE=github.com/doujins-org/*
ARG GONOSUMDB=github.com/doujins-org/*
ARG GOSUMDB=sum.golang.org
ENV GOPROXY=${GOPROXY}
ENV GOPRIVATE=${GOPRIVATE}
ENV GONOSUMDB=${GONOSUMDB}
ENV GOSUMDB=${GOSUMDB}
ENV GIT_TERMINAL_PROMPT=0

# Copy go mod files first for better caching
COPY go.mod go.sum ./

# Download dependencies with cache mount for Go modules (with retry)
# GitHub token is optional since all dependencies are public
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=secret,id=gh_token \
    set -eu; \
    GH_TOKEN=""; \
    if [ -f /run/secrets/gh_token ]; then \
      GH_TOKEN=$(tr -d '\r\n' < /run/secrets/gh_token); \
    fi; \
    if [ -n "${GH_TOKEN}" ]; then \
      echo "Using GitHub token for authentication"; \
      git config --global url."https://${GH_TOKEN}@github.com/doujins-org/".insteadOf "https://github.com/doujins-org/"; \
      git config --global url."https://${GH_TOKEN}@github.com/".insteadOf "https://github.com/"; \
    else \
      echo "No GitHub token provided, using public access"; \
    fi; \
    for i in 1 2 3; do \
      go mod download && break || (echo "go mod download failed, retrying" && sleep 5); \
    done; \
    if [ -n "${GH_TOKEN}" ]; then \
      git config --global --unset-all url."https://${GH_TOKEN}@github.com/doujins-org/".insteadOf || true; \
      git config --global --unset-all url."https://${GH_TOKEN}@github.com/".insteadOf || true; \
    fi; \
    unset GH_TOKEN

# Copy source code
COPY . .

# Build the application with cache mount
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    mkdir -p bin && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -o bin/billing ./cmd/billing


# Stage 2: production
FROM alpine:3.19

# Install runtime dependencies (include wget for healthcheck)
RUN apk --no-cache add ca-certificates tzdata wget

WORKDIR /app

# Create non-root user
RUN addgroup -g 1001 -S billing && \
    adduser -S -D -H -u 1001 -s /sbin/nologin -G billing billing

# Copy binary and migrations from builder stage
COPY --from=builder /app/bin/billing ./billing-server
COPY --from=builder /app/migrations ./migrations/

# Configuration files must be mounted at runtime; none are baked into the image.

# Change ownership to non-root user
RUN chown -R billing:billing /app

# Switch to non-root user
USER billing

ENV GIN_MODE=release \
    TZ=UTC

# Expose ports (2053 public; 8060 private/internal)
EXPOSE 2053 8060

# Health check
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD wget --no-verbose --tries=1 -O /dev/null http://localhost:2053/health/ready || exit 1

# Default entrypoint runs the CLI; override CMD to choose server vs worker.
ENTRYPOINT ["/app/billing-server"]
CMD ["run-server"]
