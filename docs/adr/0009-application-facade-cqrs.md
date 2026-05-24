# ADR 0009 — Command dispatch: `Application{Commands, Queries}` facade (no service interfaces)

**Status:** Accepted
**Date:** 2026-05-05

## Context

The team is migrating from .NET / EF Core where command dispatch typically uses MediatR or hand-rolled service interfaces. Go has no MediatR equivalent and the senior community discourages mediator patterns on top of Wolverine/Watermill ([Bogard's MediatR archive 2024](https://jimmybogard.com/), [Jeremy Miller "Wolverine vs Mediator" series](https://jeremydmiller.com/)).

Three Dots Labs Wild Workouts (verified Nov 2025) uses **CQRS with concrete handler structs** aggregated in an `Application{Commands, Queries}` facade — no service interface abstraction.

## Decision

**Per-module CQRS handlers as concrete structs, aggregated in `Application{Commands, Queries}` facade.**

```go
// internal/identity/app/command/register_tenant.go
package command

type RegisterTenantCommand struct {
    Slug       identity.Slug
    OwnerEmail identity.Email
}

type RegisterTenantHandler struct {
    repo   identity.TenantRepository
    outbox identity.OutboxWriter
    tx     identity.Transactor
    idGen  func() identity.TenantID
}

func NewRegisterTenantHandler(...) RegisterTenantHandler { ... }

func (h RegisterTenantHandler) Handle(ctx context.Context, cmd RegisterTenantCommand) (*identity.Tenant, error) {
    return h.repo.UpdateByID(ctx, ..., func(t *identity.Tenant) (bool, error) {
        // ... domain mutation
    })
}
```

```go
// internal/identity/app/app.go
package identity

type Application struct {
    Commands Commands
    Queries  Queries
}
type Commands struct {
    RegisterTenant command.RegisterTenantHandler
    SuspendTenant  command.SuspendTenantHandler
    // ...
}
type Queries struct {
    GetTenant      query.GetTenantHandler
    ListMemberships query.ListMembershipsHandler
    // ...
}
```

**HTTP handlers call directly via the facade:**

```go
func handleRegisterTenant(app identity.Application) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        tenant, err := app.Commands.RegisterTenant.Handle(r.Context(), cmd)
        // ...
    })
}
```

**No `TenantService` interface** — concrete handler structs are the contract. Decorator chains (logging/metrics/tracing) wrap individual handlers via Go generics (`go-cqrs` library or hand-rolled).

## Consequences

**Positive:**
- Compile-time wiring — every handler dependency surfaces in the constructor signature.
- Decorators applied at composition root, not interleaved into business logic.
- No reflection at runtime; no mediator overhead.
- Handler signatures self-document the contract — reading the file IS reading the API.
- Composition root in `cmd/api/main.go` shows the entire dependency graph at a glance.

**Negative:**
- Adding a new command requires touching 2-3 files (handler struct, `Commands` struct, composition root). MediatR-style auto-discovery doesn't exist.
- HTTP handlers' coupling to `identity.Application` means swapping handler implementations in tests requires reconstructing the facade. Mitigation: tests build the `Application` with fakes; not painful in practice.

## Alternatives considered

1. **MediatR-equivalent in-process bus** (e.g. `go-cqrs` runtime bus). Rejected: adds reflection + runtime resolution; senior Go community discourages mediator on top of explicit handler dispatch.
2. **Service interfaces (`type TenantService interface { RegisterTenant(...); SuspendTenant(...); ... }`).** Rejected per TDL canon — concrete handler structs are the contract; service interfaces are unnecessary indirection.
3. **One service struct per module with all use-cases as methods.** Rejected: god-struct anti-pattern; Mat Ryer 2024 explicitly retired this for the per-handler factory closure pattern.

## Sources

- [Three Dots Labs Wild Workouts (verified Nov 2025)](https://github.com/ThreeDotsLabs/wild-workouts-go-ddd-example) — `internal/trainings/app/app.go` shows `Application{Commands, Queries}` facade pattern.
- [TDL — Combining DDD, CQRS, and Clean Architecture](https://threedots.tech/post/ddd-cqrs-clean-architecture-combined/).
- [TDL go-cqrs library](https://github.com/ThreeDotsLabs/go-cqrs) — decorator chain + handler generics.
- Greg Young — *CQRS Documents* (2010) — original separation rationale.


**Fitness function:** convention-only — not mechanically expressible. The `Application{Commands, Queries}` facade shape is a naming convention; the per-module integration tests exercise the facade end-to-end.
