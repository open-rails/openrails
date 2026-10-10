# The example app: its React app and Go server. compose.yaml builds it from
# the repository root, because the example builds against this checkout's
# billing-ui and OpenRails module.

FROM --platform=$BUILDPLATFORM public.ecr.aws/docker/library/node:22-alpine@sha256:c610fcdfb1d5b4740dd70c284ed3cb16bb857e0f7166196e36a5501df7a3aa32 AS web
RUN npm install -g --ignore-scripts pnpm@11.0.0
WORKDIR /src/sdk/billing-ui
COPY sdk/billing-ui/ ./
RUN pnpm install --frozen-lockfile --ignore-scripts && pnpm --config.verify-deps-before-run=false run build
WORKDIR /src/examples/standalone/web
COPY examples/standalone/web/ ./
RUN pnpm install --frozen-lockfile --ignore-scripts && pnpm --config.verify-deps-before-run=false run build

FROM --platform=$BUILDPLATFORM public.ecr.aws/docker/library/golang:1.26.9-alpine@sha256:cdfd4fe2da6b225d8b40c6b7a105736e548e83ff56d5d8f9394446eeb5eb84e0 AS server
WORKDIR /src/examples
COPY go.mod go.sum /src/
COPY examples/go.mod examples/go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . /src
ARG TARGETOS
ARG TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -o /out/courses ./standalone

FROM public.ecr.aws/docker/library/alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b
RUN apk --no-cache add ca-certificates wget && adduser -S -D -H -u 1001 courses
WORKDIR /app
COPY --from=server /out/courses /app/courses
COPY --from=web /src/examples/standalone/web/dist /app/web/dist
COPY examples/standalone/media /app/media
USER courses
EXPOSE 8080
HEALTHCHECK --interval=5s --timeout=3s --start-period=60s CMD wget -q -O /dev/null http://localhost:8080/api/billing || exit 1
ENTRYPOINT ["/app/courses"]
