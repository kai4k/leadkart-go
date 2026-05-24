# ADR 0006 — Multi-tenancy: Postgres RLS + `SET LOCAL app.tenant_id`

**Status:** Accepted
**Date:** 2026-05-05

## Context

LeadKart is a multi-tenant SaaS. Tenant isolation MUST be enforced at the database layer (defence in depth) — not relying solely on application-level WHERE clauses, which can drift when a developer forgets the filter.

The .NET LeadKart uses Postgres RLS + `app.current_tenant()` GUC wrapper functions + EF Core connection interceptor. The Go rebuild uses the same Postgres-level mechanism with a Go-native connection-acquire callback.

## Decision

**Postgres RLS + `FORCE ROW LEVEL SECURITY` + tenant context via `SET LOCAL app.tenant_id` per transaction.**

Mechanism:

1. **Tenant context lives in `context.Context`** via the `tenancy` package (`internal/common/tenancy/`) — `tenancy.WithID(ctx, id)` writes; `tenancy.FromContext(ctx)` reads. Keys are unexported types per Go canon.
2. **HTTP middleware** decodes the JWT → calls `tenancy.WithID(ctx, claim.TenantID)` → propagates ctx down the handler chain.
3. **pgxpool `AfterAcquire(ctx, conn) bool` callback** runs on every connection checkout. It reads `tenancy.FromContext(ctx)` and executes `SET LOCAL app.tenant_id = $1` on the connection. If the tenant ID is empty (cross-tenant operator paths only), the callback opts out explicitly.
4. **Postgres RLS policies** on every tenant-scoped table:
   ```sql
   ALTER TABLE identity.tenants ENABLE ROW LEVEL SECURITY;
   ALTER TABLE identity.tenants FORCE  ROW LEVEL SECURITY;
   CREATE POLICY tenant_isolation ON identity.tenants
       USING (id = current_setting('app.tenant_id')::uuid);
   ```
5. **Cross-tenant operator paths** (Platform impersonation, `SuperAdmin` god-mode) use a separate connection pool acquired with `pool.Acquire(ctx)` and a different `BeforeAcquire` that sets `app.tenant_id` to empty + `app.is_platform = 'true'`. RLS policy includes `OR app.is_platform()`.

**Three Postgres roles:**
- `leadkart_owner` — owns schemas; runs migrations only.
- `leadkart_app` — runtime API; FORCE RLS enforced (cannot bypass).
- `leadkart_test` — integration tests; same FORCE RLS as runtime.

**No app-layer query filter as primary defence.** Application code does NOT include `WHERE tenant_id = ?` in business queries — RLS does that work at the DB. (Defence-in-depth WHERE clauses are acceptable but not the primary mechanism.)

## Consequences

**Positive:**
- Tenant leak impossible at the SQL layer — even a developer forgetting WHERE can't cross-tenant read.
- Zero ORM bridge layers — pgxpool + sqlc + RLS works without compatibility shims.
- Compromise of `leadkart_app` role still cannot bypass RLS (FORCE).

**Negative:**
- Cross-tenant operator queries require explicit second pool + role + policy — operational complexity.
- Connection-acquire callback adds ~100 µs per pool checkout. Negligible at LeadKart's scale.
- If `app.tenant_id` GUC ever leaks across requests (broken pool reuse), data corruption is silent. Mitigation: `SET LOCAL` is per-transaction; `pgxpool` resets session state on connection return.

## Alternatives considered

1. **App-layer WHERE clauses only.** Rejected — no defence in depth; one missed filter = full tenant leak.
2. **Schema-per-tenant** (Citus / Atlas Cloud pattern). Rejected — Atlas Cloud's own GopherCon 2025 case study describes regretting this choice and migrating back to row-level RLS.
3. **Database-per-tenant.** Rejected — operationally infeasible at Year-3 target (5,000 tenants).
4. **Custom tenant-aware pgx wrapper.** Rejected — pgxpool's `AfterAcquire` is the canonical integration point ([pgxpool docs](https://pkg.go.dev/github.com/jackc/pgx/v5/pgxpool#Config)).

## Sources

- [Picus Security — Enforcing DB-level Multi-tenancy with PostgreSQL RLS](https://medium.com/picus-security-engineering/enforcing-db-level-multi-tenancy-using-postgresql-row-level-security-c11d037d3f49).
- [DEV — PostgreSQL RLS in Go: Architecting Secure Multi-tenancy](https://dev.to/__8fa66572/postgresql-rls-in-go-architecting-secure-multi-tenancy-4ifm).
- [Logto blog — Implement multi-tenancy](https://blog.logto.io/implement-multi-tenancy).
- [Atlas Cloud GopherCon 2025 — Building Scalable Multi-tenant Apps in Go](https://atlasgo.io/blog/2025/05/26/gophercon-scalable-multi-tenant-apps-in-go) — schema-per-tenant regret post-mortem.
- LeadKart .NET — `multi-tenancy.md` doctrine (mirror reference for Go port).


**Fitness function:** convention-only — not mechanically expressible. RLS policies live at the Postgres layer + are exercised by the integration-test suite (`task test:int`); mechanically asserting the policy from Go would require an actual DB connection.
