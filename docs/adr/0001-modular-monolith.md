# ADR 0001 — Topology: Modular monolith

**Status:** Accepted
**Date:** 2026-05-05

## Context

LeadKart is a multi-tenant SaaS rebuild from .NET 10 (which already runs as a modular monolith). The Go ecosystem in 2024–2026 has consolidated around modular monolith as the canonical default for new SaaS, after the post-2018 microservices hype cycle reversed.

## Decision

**Modular monolith, single binary** (`cmd/api/main.go`). 8 bounded contexts (Identity, Platform, CRM, Orders, Inventory, Dispatch, Tasks, Notifications) live as sibling packages under `internal/`. Cross-context communication only via Watermill integration events on the outbox bus — never direct imports of another module's `domain/`, `app/`, `ports/`, or `adapters/`.

Module isolation enforced compile-time by Go's `internal/` rule: `internal/orders/...` cannot import `internal/identity/domain/...`.

## Consequences

**Positive:**
- Single deploy unit. Trivial operations (one binary, one deploy, one log stream).
- Refactoring across modules is type-safe (compile-time errors catch drift).
- No distributed transactions; outbox-in-Postgres handles cross-context consistency.
- Splittable into microservices later if/when team or load demands — boundaries already enforced.
- Matches LeadKart .NET decision; reference port is straightforward.

**Negative:**
- Single deployment cadence — no per-module independent rollout.
- Single failure domain — one bug can take down the whole service. Mitigation: Watermill events decouple async paths; HTTP handlers have separate timeouts; pgxpool isolates DB connection saturation.
- Larger codebase under one `go.mod` — partially mitigated by `internal/{module}` discipline.

## Alternatives considered

1. **Microservices from day one** (Three Dots Labs Wild Workouts canonical shape — 4 services). Rejected for single-team early-stage SaaS: operationally heavy, premature optimisation. TDL's own 2024+ guidance ([Modular Monolith vs Microservices in Go](https://threedots.tech/post/microservices-or-monolith-its-detail/), [The Distributed Monolith Trap](https://threedots.tech/episode/the-distributed-monolith-trap/)) recommends modular monolith first.
2. **Majestic monolith with extracted hot paths** (e.g. Notifications + Dispatch as separate services from day 1). Rejected: complexity not justified by current load.
3. **Service-shaped monolith** (single binary but per-module DBs, no shared Postgres). Rejected: complicates RLS, outbox, and cross-module reads without operational benefit at LeadKart's scale.

## Sources

- Three Dots Labs — [Microservices or Modular Monolith — it's a detail](https://threedots.tech/post/microservices-or-monolith-its-detail/), [The Distributed Monolith Trap](https://threedots.tech/episode/the-distributed-monolith-trap/) (Jan 2026 episode).
- Brandur Leach — [microservices essay](https://github.com/brandur/microservices); Crunchy Bridge runs as Go + Postgres monolith.
- Kamil Grzybek — [Modular Monolith: Domain-Centric Design](https://www.kamilgrzybek.com/blog/posts/modular-monolith-domain-centric-design).
- Industry consensus — [Foojay 2025](https://foojay.io/today/monolith-vs-microservices-2025/), [DZone post-monolith 2025](https://dzone.com/articles/post-monolith-architecture-2025), [Leapcell 2025](https://leapcell.io/blog/why-monolithic-architecture-reigns-supreme-for-new-projects-in-2025).


## Fitness function

`TestArch_NoCrossModuleImports` (in `internal/architecture/`).

Module boundaries enforced — no module imports another module's private `domain/app/ports/adapters/` packages (allow-listed shared-kernel exceptions documented in the test).
