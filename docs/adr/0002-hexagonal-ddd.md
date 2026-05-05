# ADR 0002 — Architectural style: Hexagonal + DDD (TDL canon)

**Status:** Accepted
**Date:** 2026-05-05

## Context

Inside the modular monolith (ADR 0001), each bounded context needs an internal architecture that protects business invariants from infrastructure churn while staying pragmatic for typical SaaS workloads. The team is migrating from .NET / EF Core where domain-rich modules used DDD-Lite (aggregates with private fields, factory functions, domain events).

## Decision

**Hexagonal (Ports & Adapters) + DDD-Lite per Three Dots Labs Wild Workouts canon (verified Nov 2025)**.

Per-module structure:

```
internal/{module}/
├── domain/                   # entities, VOs, repository interfaces, domain events
├── app/                      # use-cases (CQRS handlers + Application{Commands, Queries} facade)
│   ├── app.go                # Application{Commands, Queries} struct
│   ├── command/              # one handler per file
│   └── query/                # one handler per file
├── ports/                    # PORT — inbound concrete impls (HTTP server, event subscribers)
└── adapters/                 # ADAPTER — outbound concrete impls (sqlc/pgx repo, watermill publisher)
```

**TDL ports/adapters terminology (deliberately NOT Cockburn's primary/secondary):**
- **port** = inbound concrete impl. NOT an interface.
- **adapter** = outbound concrete impl.
- Interfaces live with their consumer — repository interfaces in `domain/`, use-case-specific dep interfaces near the handler that uses them.
- **No service interfaces** (`TenantService` doesn't exist). CQRS replaces them with concrete `RegisterTenantHandler` structs aggregated in the `Application{Commands, Queries}` facade.

**Composition root** = `cmd/api/main.go`. Manual constructor wiring (Mat Ryer 2024 NewServer pattern). No DI container.

## Consequences

**Positive:**
- Domain stays free of infrastructure imports — compile-time enforced.
- Adapters are swappable (Postgres → CockroachDB; HTTP → gRPC) without domain changes.
- Tests can substitute fake adapters (in-memory port impls) — no DB needed for handler tests.
- Mental model transfers cleanly from EF Core (the team's prior experience): domain aggregates with private fields and factory ctors map directly.

**Negative:**
- More ceremony than flat `package identity` with everything in one file. Per ADR 0023 / "modular not religious" doctrine, thin modules (Notifications, Tasks if simple) MAY skip the `domain/` layer with explicit justification in their README.
- 5+ packages per module — more boilerplate per new module.
- Wild Workouts uses per-module `service/service.go` for composition; LeadKart consolidates this into single `cmd/api/main.go` since modular monolith ≠ microservices.

## Alternatives considered

1. **Mat Ryer flat layout** (single `package identity` with all files at the bounded-context root). Rejected: works for thin services, fights LeadKart's domain richness across 8 modules with cross-aggregate invariants.
2. **Clean Architecture (Robert Martin)** with explicit layers (entities/use-cases/interface-adapters/frameworks). Rejected: maps poorly to Go's package system; TDL's hexagonal is the Go-canonical adaptation.
3. **Onion Architecture**. Rejected: no Go-community canonical reference; TDL hexagonal has stronger named-peer adoption.
4. **Vertical slice without layers**. Rejected: works for small features but doesn't scale to LeadKart's 8 modules with shared kernel concerns.

## Sources

- ThreeDotsLabs — [Wild Workouts repo](https://github.com/ThreeDotsLabs/wild-workouts-go-ddd-example) (verified Nov 3, 2025 master branch).
- ThreeDotsLabs — ["Introducing Clean Architecture"](https://threedots.tech/post/introducing-clean-architecture/) (load-bearing quote: *"Our ports are Hexagonal Architecture's Primary Adapters. Our adapters are Hexagonal Architecture's Secondary Adapters."*).
- ThreeDotsLabs — ["DDD Lite in Go"](https://threedots.tech/post/ddd-lite-in-go-introduction/) blog series (2020, refreshed 2024).
- ThreeDotsLabs — ["Repository Pattern in Go"](https://threedots.tech/post/repository-pattern-in-go/).
- Mat Ryer — ["How I write HTTP services in Go after 13 years"](https://grafana.com/blog/2024/02/09/how-i-write-http-services-in-go-after-13-years/) (Grafana, Feb 2024).
- Vaughn Vernon — *Implementing Domain-Driven Design* (2013) — strategic + tactical patterns.
- Alistair Cockburn — [Hexagonal Architecture](https://alistair.cockburn.us/hexagonal-architecture/) (original 2005, republished 2024).
