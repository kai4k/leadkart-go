# ADR 0029 — Two-binary deploy: cmd/api + cmd/worker

**Status:** Accepted
**Date:** 2026-05-08

## Context

Phase 1 shipped one binary (`cmd/api`) hosting both the request path AND event-processing — gochannel pubsub, outbox forwarder, Watermill subscriber router, all running as goroutines next to the HTTP handlers. That worked for Phase 0/1 scale but has three problems:

1. **Subscriber retries fight request CPU.** A slow handler exponentially backs off; a busy API host shares its goroutine pool between answering customer requests and burning cycles on retry timers. The two workloads have different SLOs and shouldn't share scheduling.
2. **Horizontal scaling is wasteful.** N replicas of cmd/api means N replicas of the forwarder + N copies of the in-memory cache + every replica polling the outbox. Doubling capacity for a request-rate spike doubles event-processing capacity that you don't need.
3. **The outbox/subscriber wiring was silently broken in production.** `cmd/api/main.go` constructed the gochannel publisher + forwarder, but never registered subscribers. Every integration event was published to a topic with zero consumers and dropped. The cascade ran in tests only. (See `feature/security-stamp-cache` PR.)

The .NET reference deploys a single ASP.NET Core process with Hangfire as a sidecar; the Go architecture has no such constraint.

## Decision

**Two binaries, one Postgres, one Redis.**

- **`cmd/api`** — REQUEST PATH ONLY. HTTP handlers, JWT issuance, freshness gate, outbox writes (per-handler, inside the command's transaction). NO forwarder, NO subscriber router, NO in-process pubsub.
- **`cmd/worker`** — EVENT PROCESSING. Outbox forwarder polls every module's outbox table, publishes to Watermill GoChannel (in-process for v0.2; broker swap to Redis Streams or Kafka in v0.3+ is one Publisher swap). Subscriber router runs the canonical middleware stack (Recoverer + CorrelationID + TenantContext + Idempotency + Audit + Retry per `messaging.md`). River background-job pool runs alongside (ADR 0010).

Both binaries:
- Share the same Postgres pool (different connection strings; same database). The outbox is the seam — API writes, worker reads.
- Share the same Redis (HybridCache L2 — `SecurityStampCache` invalidation runs on the worker side; the API reads through the same L2).
- Have their own admin probe listener (`/alive` + `/ready` + `/health`) on distinct ports (`:9090` API, `:9091` worker).
- Use distinct OTel `service.name` resource attributes so backends can split telemetry per binary.

The split is a deploy-shape change only. v0.2 single-replica dev environments run both binaries as separate processes against shared Postgres + Redis (docker-compose orchestrates). v0.3+ scales each independently — typically more API replicas than workers, with worker periodics (ADR 0010 PeriodicJob) using river's advisory-lock leader election so a multi-worker deploy still runs each periodic at most once.

## Consequences

**Positive:**

- Subscriber retries can't starve request-path threads.
- Independent horizontal scaling.
- The "forwarder published but no subscribers consumed" failure mode is structurally impossible — the worker binary's purpose IS to consume; if it's not deployed, the absence is loud.
- Per-binary OTel splits make alert routing easier (an API 500 page is different from a worker job-stalled page).
- Each binary's `cmd/.../main.go` stays focused — neither carries dead-weight imports for the other's concern.

**Negative:**

- Operators run two binaries instead of one. Mitigated: same config struct (subset used per binary), same image build, same logging shape — just two `kubectl apply`s instead of one.
- Dev environments must run both. Without the worker, integration events accumulate in the outbox and downstream cache invalidation never fires. Documented in `cmd/api/main.go` package doc; docker-compose handles the dev case.
- The in-process gochannel publisher means cmd/worker is currently a single replica per process group. Production multi-replica means swapping to Redis Streams / NATS / Kafka — the `messaging.Router` interface is broker-neutral so this is one Publisher swap.

## Alternatives considered

1. **Single binary with a build flag (`--mode api` / `--mode worker`).** Considered. Rejected: same image for both is OK, but a single `main()` accreting both modes is a worse separation than two `main()`s. The shared composition (`internal/...` packages) is what's reusable; `main()` is the binary's identity.
2. **Sidecar pattern (worker as a kubernetes sidecar to the API pod).** Rejected: pod-coupling forces them to scale together — exactly the problem we're solving.
3. **Keep one binary; gate the worker pieces on a config flag.** Rejected: same coupling problem; configuration sprawl.

## Sources

- LeadKart `.NET .claude/rules/messaging.md` — "subscribers without a producer (or producers without a consumer) = dead code that lies about behaviour" — the canon that flagged the silent-prod-gap failure mode.
- [Mat Ryer 2024 — "How I write HTTP services in Go after 13 years"](https://grafana.com/blog/2024/02/09/how-i-write-http-services-in-go-after-13-years/) — `NewServer` composition pattern preserved on both binaries.
- ADR 0008 (Watermill messaging) + ADR 0010 (river jobs) + ADR 0014 (OTel) — all extend cleanly to two binaries.
