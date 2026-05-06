# LeadKart Go — multi-stage, multi-target Dockerfile
#
# Build:
#   docker buildx build --platform=linux/amd64,linux/arm64 \
#     --target=api --tag=ghcr.io/leadkart/api:dev -f docker/Dockerfile .
#
# Targets: api / worker / migrate (one builder, three slim final images).
# Final base: cgr.dev/chainguard/static:latest (signed, SBOM-attested).
# BuildKit cache mounts give dramatic rebuild speedup.
# syntax=docker/dockerfile:1.7

ARG GO_VERSION=1.26

# ===========================================================================
# Build stage (shared across all targets)
# ===========================================================================
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS build
WORKDIR /src

# Cache go mod downloads across builds via BuildKit
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=bind,source=go.sum,target=go.sum \
    --mount=type=bind,source=go.mod,target=go.mod \
    go mod download -x

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev

# Build with cache mounts for go build + go mod
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=bind,target=. \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
        go build -trimpath -ldflags="-s -w -X main.version=$VERSION" -o /out/api     ./cmd/api && \
    if [ -d ./cmd/worker ]; then \
        CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
            go build -trimpath -ldflags="-s -w -X main.version=$VERSION" -o /out/worker  ./cmd/worker; \
    fi && \
    if [ -d ./cmd/migrate ]; then \
        CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
            go build -trimpath -ldflags="-s -w -X main.version=$VERSION" -o /out/migrate ./cmd/migrate; \
    fi

# ===========================================================================
# Final stage: API (HTTP server)
# ===========================================================================
FROM cgr.dev/chainguard/static:latest AS api
COPY --from=build /out/api /api
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/api"]

# ===========================================================================
# Final stage: Worker (river background jobs)
# ===========================================================================
FROM cgr.dev/chainguard/static:latest AS worker
COPY --from=build /out/worker /worker
USER nonroot:nonroot
ENTRYPOINT ["/worker"]

# ===========================================================================
# Final stage: Migrate (goose runner)
# ===========================================================================
FROM cgr.dev/chainguard/static:latest AS migrate
COPY --from=build /out/migrate /migrate
COPY --from=build /src/migrations /migrations
USER nonroot:nonroot
ENTRYPOINT ["/migrate"]
