# ADR 0033 — Tenant context: `tenancy.FromContext(ctx)` package function

**Status:** Accepted
**Date:** 2026-05-05

## Context

Tenant context propagates through every request — HTTP middleware → handler → service → repository → pgxpool `AfterAcquire` → `SET LOCAL app.tenant_id` (ADR 0006). The .NET version uses `ICurrentTenant` interface injected via DI. Go's idiom is `context.Context` carrying request-scoped values + a package providing typed accessors.

## Decision

**`internal/common/tenancy/` package** with `tenancy.WithID(ctx, id)` to set + `tenancy.FromContext(ctx)` to read. NO `ICurrentTenant` interface.

```go
package tenancy

import "context"

// ID is a tenant identifier (UUIDv7 wrapped).
type ID string

func (i ID) String() string { return string(i) }
func (i ID) IsZero() bool   { return i == "" }

// ctxKey is unexported per Go canon — prevents collision with other packages.
type ctxKey struct{ name string }

var tenantKey = ctxKey{"tenant"}

// WithID returns a new context with the tenant ID attached.
func WithID(ctx context.Context, id ID) context.Context {
    return context.WithValue(ctx, tenantKey, id)
}

// FromContext extracts the tenant ID, returning (zero, false) if absent.
func FromContext(ctx context.Context) (ID, bool) {
    id, ok := ctx.Value(tenantKey).(ID)
    return id, ok
}

// MustFromContext panics if the tenant is absent — for use in code paths
// where tenant must be set (request-scoped repositories). NOT for handlers,
// which should return an error on missing tenant.
func MustFromContext(ctx context.Context) ID {
    id, ok := FromContext(ctx)
    if !ok || id.IsZero() {
        panic("tenancy: tenant required in context")
    }
    return id
}
```

**Wiring:**
1. **HTTP middleware** decodes JWT → `ctx = tenancy.WithID(ctx, claim.TenantID)` → propagates.
2. **pgxpool `AfterAcquire`** reads `tenancy.FromContext(ctx)` → executes `SET LOCAL app.tenant_id = $1`.
3. **Watermill subscribers** read tenant from event metadata header → `tenancy.WithID(ctx, ...)` before calling handlers.

## Consequences

**Positive:**
- Idiomatic Go — `context.Context` is the canonical request-scoped value carrier.
- No DI container needed — accessors are package-level functions.
- Compile-time prevents collision with other packages (unexported `ctxKey`).
- `MustFromContext` is the audit-grep target — every call must justify its presence.

**Negative:**
- `ctx.Value` is an antipattern when overused — easy to abuse. Mitigation: rule that ONLY tenancy + correlation/trace IDs go in context; everything else flows through explicit args.
- Tests must remember to set tenant ID before exercising repository code. Mitigation: test fixtures (`identitytest.WithTenant(ctx, id)`) make this explicit.

## Alternatives considered

1. **`ICurrentTenant` interface injected via constructor.** Rejected as `.NET ideology` — Go uses ctx for request-scoped values per Go core team canon (Sameer Ajmani 2014, still authoritative).
2. **Global var or thread-local.** Rejected — fights Go's explicit context propagation; breaks under goroutines.
3. **Pgxpool `BeforeAcquire` reading from a request struct.** Rejected — pgxpool only sees `ctx`, not request structs.

## Sources

- [Sameer Ajmani — Go Concurrency Patterns: Context (Go blog)](https://go.dev/blog/context).
- [Bryan C. Mills — Rethinking Classical Concurrency Patterns (GopherCon 2018)](https://www.youtube.com/watch?v=5zXAHh5tJqQ).
- [Peter Bourgon — context isn't for everything (2017)](https://peter.bourgon.org/blog/2018/04/04/context-package-and-go-language.html).
- LeadKart .NET — `multi-tenancy.md` "Identity model" + ADR 0006 (this repo) for the RLS mechanism this powers.


**Fitness function:** convention-only — not mechanically expressible. `tenant.FromContext(ctx)` is a single-package API surface; usage discipline is observed in code review.
