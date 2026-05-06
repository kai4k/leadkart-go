# syntax=docker/dockerfile:1.7
#
# ============================================================================
# LeadKart Go — Multi-stage, multi-target Dockerfile
# ============================================================================
# Build:
#   docker buildx build --platform=linux/amd64,linux/arm64 \
#     --target=api --tag=ghcr.io/leadkart/leadkart-go-api:dev \
#     -f docker/Dockerfile .
#
#   docker buildx build --platform=linux/amd64,linux/arm64 \
#     --target=migrate --tag=ghcr.io/leadkart/leadkart-go-migrate:dev \
#     -f docker/Dockerfile .
#
# Run (api):
#   docker run --rm -p 8080:8080 -p 9090:9090 \
#     -e LEADKART_POSTGRES__DSN=... \
#     -e LEADKART_REDIS__ADDR=... \
#     -e LEADKART_JWT__KEY_ID=k1 -e LEADKART_JWT__SIGNING_KEY=... \
#     ghcr.io/leadkart/leadkart-go-api:dev
#
# Run (migrate):
#   docker run --rm -e DATABASE_URL=postgres://... \
#     ghcr.io/leadkart/leadkart-go-migrate:dev up
#
# Targets:
#   api      — HTTP API. EXPOSE 8080 (public) + 9090 (admin: /alive, /ready,
#              /health, /debug/pprof/*). HEALTHCHECK invokes `/api -healthcheck`
#              against the admin listener — distroless static has no shell,
#              so the binary itself is the probe (chainguard canonical pattern).
#   worker   — river background jobs. Built only when cmd/worker exists
#              (Phase 2+ Platform module). Today the COPY is a no-op guard.
#   migrate  — goose runner; image carries /migrate + /migrations sidecar.
#              One-shot container — exits after `up` / `status` / etc.
#
# Final base: cgr.dev/chainguard/static:latest — Wolfi-built, signed +
# SBOM-attested upstream by Chainguard, distroless (no shell, no package
# manager, no busybox), running as nonroot 65532:65532.
# BuildKit cache mounts give dramatic rebuild speedup; --mount=type=bind
# avoids physically copying the build context for every layer.
# ============================================================================

ARG GO_VERSION=1.26

# ===========================================================================
# Stage 1 — Build (shared across all final targets)
# ===========================================================================
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS build
WORKDIR /src

# Cache go mod downloads across builds via BuildKit. Bind-mount go.{mod,sum}
# instead of COPY-ing — avoids invalidating the layer when other files change.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=bind,source=go.sum,target=go.sum \
    --mount=type=bind,source=go.mod,target=go.mod \
    go mod download -x

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev

# Build with cache mounts for go build + go mod. Three binaries from one
# `go build` invocation each (Go's compiler caches inter-package work).
# The `if [ -d ... ]` guards future-proof the worker target — when
# cmd/worker ships in v0.3 the build kicks in automatically without
# Dockerfile churn.
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
# Stage 2 — Final stage: API (public HTTP server + admin probe listener)
# ===========================================================================
FROM cgr.dev/chainguard/static:latest AS api

# OCI labels — readable from `docker inspect` + populated automatically
# by docker/metadata-action in CI but baked here for local builds.
ARG VERSION=dev
LABEL org.opencontainers.image.title="leadkart-go-api"
LABEL org.opencontainers.image.description="LeadKart Go HTTP API + outbox forwarder"
LABEL org.opencontainers.image.source="https://github.com/kai4k/leadkart-go"
LABEL org.opencontainers.image.licenses="Proprietary"
LABEL org.opencontainers.image.version=$VERSION
LABEL org.opencontainers.image.base.name="cgr.dev/chainguard/static:latest"

# Sensible defaults — overridable at runtime via -e / docker compose
# environment block. Mirrors the .NET LeadKart Dockerfile's
# ASPNETCORE_URLS / ASPNETCORE_ENVIRONMENT pattern.
ENV LEADKART_LISTEN__API=":8080"
ENV LEADKART_LISTEN__ADMIN=":9090"
ENV TZ="UTC"

COPY --from=build /out/api /api
USER nonroot:nonroot
EXPOSE 8080 9090

# Distroless HEALTHCHECK — invokes the binary's own healthcheck branch
# (no wget/curl in chainguard/static). Probes the admin listener's /alive
# endpoint per audit-checklist.md §12 (probes never on public-facing path).
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD ["/api", "-healthcheck"]

ENTRYPOINT ["/api"]

# ===========================================================================
# Stage 3 — Final stage: Worker (river background jobs — Phase 2+)
# ===========================================================================
# Built only when cmd/worker exists in source. Today the COPY would fail
# (no /out/worker) — keep target declared so Dockerfile shape is stable
# across the v0.3 worker landing.
FROM cgr.dev/chainguard/static:latest AS worker

ARG VERSION=dev
LABEL org.opencontainers.image.title="leadkart-go-worker"
LABEL org.opencontainers.image.description="LeadKart Go river background-job worker"
LABEL org.opencontainers.image.source="https://github.com/kai4k/leadkart-go"
LABEL org.opencontainers.image.licenses="Proprietary"
LABEL org.opencontainers.image.version=$VERSION

ENV TZ="UTC"

COPY --from=build /out/worker /worker
USER nonroot:nonroot
ENTRYPOINT ["/worker"]

# ===========================================================================
# Stage 4 — Final stage: Migrate (goose runner)
# ===========================================================================
# One-shot container. docker compose `restart: "no"` +
# `depends_on: { condition: service_completed_successfully }` is the
# canonical orchestration pattern (mirrors .NET Dockerfile.migrations).
FROM cgr.dev/chainguard/static:latest AS migrate

ARG VERSION=dev
LABEL org.opencontainers.image.title="leadkart-go-migrate"
LABEL org.opencontainers.image.description="LeadKart Go goose migration runner — applies pending migrations as the owner role and exits"
LABEL org.opencontainers.image.source="https://github.com/kai4k/leadkart-go"
LABEL org.opencontainers.image.licenses="Proprietary"
LABEL org.opencontainers.image.version=$VERSION

# Sensible defaults — DATABASE_URL must be supplied at runtime.
ENV MIGRATIONS_DIR="/migrations"
ENV TZ="UTC"

COPY --from=build /out/migrate /migrate
COPY --from=build /src/migrations /migrations
USER nonroot:nonroot
ENTRYPOINT ["/migrate"]
CMD ["up"]
