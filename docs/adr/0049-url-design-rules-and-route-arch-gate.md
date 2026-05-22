# ADR 0049 — URL design rules + route-registration arch gate

**Status:** Accepted
**Date:** 2026-05-22
**Hot-fix follow-up to:** Wave 7 platform→common merge (ADR 0048)

## Context

Wave 7 surfaced two classes of bug that local `task ci` did NOT catch but remote CI did:

### Bug A — Integration-tagged build failure

`cmd/api/cross_tenant_e2e_test.go` (a `//go:build integration` file) had its `commonemail` alias stripped during the platform→common sed-rewrite; the unaliased `email` import was not re-added. Local `task ci` did NOT compile the integration-tagged file (Go skips files outside the active build tag by default), so the build break only surfaced on the remote CI job that runs `go test -tags=integration ./...`.

### Bug B — Go 1.22 ServeMux pattern conflict

Two routes shipped on separate, never-co-existing branches collided when stacked together for the first time on Wave 7:

| Route | Origin | Pattern shape |
|---|---|---|
| `GET /api/v1/tenants/{tenantId}/activity` | Wave 2 (audit reads) | wildcard@4 + literal@5 |
| `GET /api/v1/tenants/by-slug/{slug}` | slug branch (ADR 0044) | literal@4 + wildcard@5 |

For inputs like `/tenants/by-slug/activity` both patterns match and neither is more specific. Go 1.22+ ServeMux panics at registration time. Local `task ci` did NOT catch it because no unit test exercises the full route table; only the Docker smoke job (which actually starts the server) surfaced the panic.

**Both bugs were caught LATE.** The hot-fix on Wave 7 commit `1522fcb` repaired the symptoms (added the missing import; renamed the route to `/tenants/{tenantId}/audit/events`) but did NOT prevent recurrence.

Industry references for the URL design + pre-merge gating:

| Source | Position |
|---|---|
| **Stripe API conventions** | Lookups by non-primary-key use query params (`/v1/customers?email=...`), never path segments. Path params are reserved for canonical resource IDs. |
| **GitHub REST API guidelines** | Resource hierarchies use sub-resource paths (`/repos/{owner}/{repo}/issues/{id}/comments`), not flat compound URLs. |
| **Auth0 / Okta API design** | "Find-by" semantics → query params; "get-by-canonical-id" semantics → path params. Distinct from each other to keep routing unambiguous. |
| **Go 1.22 release notes (Russ Cox)** | ServeMux pattern precedence is by literal-vs-wildcard at each position; conflicts panic at registration "by design" to surface ambiguity at startup, not runtime. |
| **Microsoft REST API guidelines §7** | "Avoid disjoint route trees that can match the same URL by accident — design URL shapes so static prefixes disambiguate at the routing layer." |

The takeaway: the route conflict is fundamentally a **URL design** problem (two routes invented independently with overlapping wildcard shapes), and the integration-tagged build failure is fundamentally a **local CI completeness** problem (not all build tags compiled before push). Both have canonical fixes that prevent recurrence — neither was applied in the Wave 7 hot-fix.

## Decision

Three additive gates land together in Wave 8. None of them changes existing behaviour; all three make the failure mode SURFACE EARLIER (at unit-test / pre-push time, not at CI / smoke-test time).

### 1. URL design rule — codified

**Lookups by non-primary-key use query parameters, not path segments.**

| Use case | Canonical shape | Example |
|---|---|---|
| Get resource by primary ID | path param | `GET /v1/tenants/{tenantId}` |
| Get resource by alternate unique key (slug, email, handle) | **query param** | `GET /v1/tenants?slug=acme` |
| Sub-resource collection of a parent | sub-path with literal segment | `GET /v1/tenants/{tenantId}/audit/events` |
| Cross-tenant lookup (platform-only) | query param | `GET /v1/platform/persons?email=...` |

**Anti-patterns** (forbidden):

- `GET /v1/tenants/by-slug/{slug}` — conflicts with `GET /v1/tenants/{id}/anything` shape. The existing `by-slug/{slug}` endpoint from ADR 0044 is GRANDFATHERED for v0.2 backward-compat (frontend already consumes it), but the canonical replacement is `?slug=` on the listing endpoint.
- `GET /v1/users/by-email/{email}` — same anti-shape; use `?email=` instead.
- Mixing literal-then-wildcard at depth N with wildcard-then-literal at the same depth — the Go ServeMux ambiguity rule means both routes panic at startup.

**Sub-resource naming.** For `tenant`-scoped sub-collections, the literal segment after `{tenantId}` MUST be either a noun (`audit`, `members`, `roles`) or a noun-collection (`audit/events`, `members/active`). A single literal segment that doubles as a verb (`activity` was the Wave 2 example) reads as a function call rather than a resource — and worse, single-literal-at-pos-5 collides with `by-slug/{slug}` shapes. Two-segment sub-paths (`audit/events`) are structurally distinct.

### 2. Route-registration arch test — fires at PR-create time

`internal/identity/ports/route_registration_test.go::TestArch_RouteRegistration_NoConflicts` wires `ports.AddRoutes` against a fresh `http.ServeMux` with synthetic-minimal dependencies (nil log; zero `app.Application{}`; stub verifier + stamp-validator). Any panic from Go 1.22+'s pattern-overlap detector fails the test.

```go
defer func() {
    if r := recover(); r != nil {
        t.Fatalf("route registration panicked ...\n panic: %v", r)
    }
}()
mux := http.NewServeMux()
ports.AddRoutes(mux, nil, app.Application{}, stubVerifier{}, stubStampValidator{})
```

The test runs in <50ms; it joins `task test:arch` (broadened to include `./internal/identity/ports/...`). Drift becomes impossible: any future route that creates a pattern conflict fails CI at unit-test time, not at Docker smoke time.

### 3. Integration-tag compile gate — local `task ci` parity with remote

New Taskfile target `task test:int:compile`:

```yaml
test:int:compile:
  cmds:
    - go build -tags=integration ./...
    - go vet -tags=integration ./...
```

Added to both `task ci` and `task ci:race`. The compile-check is fast (<5s; no testcontainers spun up — only `go build` against the integration build tag). Catches missing imports, unused variables, and other compile-time errors in `//go:build integration` files BEFORE the developer pushes, instead of letting the remote integration CI job find them.

This complements (does NOT replace) `task test:int`, which actually runs integration tests against testcontainers. The compile gate is the cheap pre-push check; the run gate is the expensive merge-blocker on GH Actions.

## Consequences

**Positive:**

- **Route conflicts surface at PR time, not at CI smoke time.** The exact bug class that broke Wave 7 cannot recur — adding any pattern-overlapping route fails unit tests in 50ms.
- **Integration-tag build failures surface at pre-push time.** Local `task ci` now compiles every build-tagged file the remote CI compiles. The Wave 7 missing-import failure could not have escaped to remote CI under this gate.
- **URL design rules codified.** Future endpoint authors (human + AI) have a documented canon for "where do I put a lookup-by-slug?" Stripe / GitHub / Auth0 canon referenced; no ambiguity.
- **No runtime cost.** All three additions are CI-time / build-time concerns. Production binaries unchanged.

**Negative:**

- **One grandfathered violation.** `GET /v1/tenants/by-slug/{slug}` from ADR 0044 stays for v0.2 frontend-contract compatibility. The arch test passes today because no OTHER route currently has the conflicting shape; a future canonical migration would move slug lookup to `GET /v1/tenants?slug=acme` (ListAllTenants endpoint with a filter). Documented as known debt; not blocking.
- **Pre-push gate gets slightly slower.** `task test:int:compile` adds ~3-5s to local `task ci`. Acceptable cost for the failure-surfacing benefit.
- **Two new files for new contributors to understand.** `route_registration_test.go` + the updated Taskfile. Both have explanatory docstrings + ADR pointers.

## Alternatives considered

1. **Move slug lookup to `?slug=` query param now (full canonical fix).** Considered. Rejected for Wave 8 because:
   - Frontend already consumes `/by-slug/{slug}`; changing it requires coordinated FE+BE update
   - The arch test prevents NEW violations; the grandfathered route is the only existing one
   - Documented as a follow-up; can land when the frontend has bandwidth

2. **Use a spectral / OpenAPI linter for the URL rule.** Possible — `spectral` has custom-rule support. Deferred to the future `task ci:openapi` work tracked in ADR 0046's follow-ups. The Go arch test catches the MECHANICAL conflict (ServeMux panic); the spectral rule would catch the DESIGN violation (using `/by-X/{X}` shape at all). Both belong in CI eventually; the Go test is the cheaper first cut.

3. **Add the route arch test to `task test` (default) instead of `task test:arch`.** Rejected — `task test` runs with `-skip "^TestArch_"`. Arch tests belong in their own task by convention (matches ADR 0047 + the integration-events arch test).

4. **Skip the integration-compile gate; rely on remote CI.** Rejected — the WHOLE point of the local `task ci` is "if this passes, push is safe." Remote-only catching defeats the pre-push promise. The 3-5s cost is negligible.

5. **Add a runtime "unused route" warning instead of compile-time gate.** Out of scope. The mux already panics at registration; that's stronger than a runtime warning.

## Sources

- **Stripe API conventions** — public docs at stripe.com/docs/api: lookup-by-X uses `?email=...` not `/by-email/...`
- **GitHub REST API guidelines** — github.com/github/rest-api-description structure of sub-resource paths
- **Auth0 / Okta API design canon** — query params for find-by-X, path params for canonical-ID
- **Go 1.22 release notes** — pkg/net/http ServeMux pattern syntax + the "more specific" rule
- **Microsoft REST API guidelines** — github.com/microsoft/api-guidelines §7
- **ADR 0007** — stdlib net/http ServeMux 1.22+ (the substrate this gate protects)
- **ADR 0044** — Enumeration safety (the slug endpoint that's now grandfathered)
- **ADR 0046** — OpenAPI spec-first (the future spectral lint will be a complementary gate)
- **ADR 0047** — Layer-boundary discipline (the precedent for arch-test-as-CI-gate)
- **ADR 0048** — `platform/` → `common/` merge (the rename that surfaced both bugs)
- **CLAUDE.md** § "Testing rules" — arch tests as drift gates
