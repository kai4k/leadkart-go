# ADR 0046 — OpenAPI spec-first contract + Scalar `/docs`

**Status:** Accepted
**Date:** 2026-05-25

## Context

[README.md:80](../../README.md) declared since v0.1 that `api/openapi.yaml` is "single source of truth for HTTP contract" — but the file didn't exist. The frontend has hand-maintained TypeScript interfaces (`TenantDto`, `UserDto`, etc.) that drift from Go's `dto.go` whenever one side changes without the other.

Phase 1.5+ shipped ~60 endpoints across Auth / Tenants / Users / Roles / Platform / Search / Audit categories. Without an OpenAPI spec:

- Frontend hand-maintains every DTO interface; drift is silent until runtime
- No interactive API docs (engineers grep through `internal/identity/ports/*.go` to discover endpoints)
- No contract-test gate — backend can change wire shape without a CI signal
- Future API consumers (mobile apps, partner integrations) get no machine-readable contract

Industry canon: every modern API at scale publishes an OpenAPI spec. Stripe / GitHub / Twilio / Cloudflare / Anthropic / Resend / Linear (REST surface) / PayPal — universal. Two valid approaches:

| Approach | Mechanism | Vendors |
|---|---|---|
| **Spec-first** | Hand-write `openapi.yaml`; handlers conform to spec | Stripe, GitHub, Anthropic, modern best-practice |
| **Code-first** | Annotate handlers (Go comments DSL); codegen → spec | swaggo/swag, springdoc-openapi, FastAPI |

Constraints inherited from preceding ADRs:

- **ADR 0007** — stdlib `net/http` ServeMux. No framework-coupled annotation systems.
- **ADR 0018** — Manual NewServer composition, no DI container. The Scalar UI integration is a plain `http.Handler` registration, no middleware ceremony.
- **ADR 0024** — Chainguard distroless static binary. The OpenAPI spec MUST be embedded into the binary (no external file dependency).
- **CLAUDE.md `cmd/api/main.go` thin host rule** — no business logic in main; the spec embed + Scalar HTML lives in a dedicated `internal/common/openapi/` package.

Non-goals:

- Code generation FROM the spec (generated stubs). The Go handlers already exist; this ADR captures the contract, not regenerates it. If/when sqlc-style codegen-from-spec becomes valuable for partner integrations, that's a future tool, not this ADR.
- OpenAPI-derived validation middleware (request/response shape-check against spec on every call). Useful at very large API surfaces; over-engineering at v0.2. Routes already validate via the DDD VO ctors at the domain boundary.

## Decision

**Spec-first. Hand-written `api/openapi.yaml` is the canonical contract; Go handlers conform to it. Embedded in the binary via `//go:embed`; served at `/openapi.yaml`. Scalar UI at `/docs` renders the spec for human + AI engineers.**

### Why spec-first over code-first

| Property | Spec-first wins | Code-first wins |
|---|---|---|
| **Review surface** | Spec changes reviewed independently of code; designers / PMs can comment on contract before implementation | — |
| **Drift detection** | Arch test can assert every `mux.Handle` has a matching operation in spec; the spec is the gate | Drift is hidden inside Go comments; CI rarely catches drift |
| **Multiple language clients** | One spec generates TS + Python + Go + Java clients; all stay aligned | Same — both approaches can do this |
| **Mocking before implementation** | Frontend can build against Prism/Mockoon mocks from the spec while backend is in flight | — |
| **Versioning** | Spec changes get a separate `v2.yaml` cleanly; codebase doesn't fork | Versioning Go comments is awkward |
| **Author-time effort** | Higher — write spec separately from code | Lower — comments inline with handlers |
| **AI-assistant comprehension** | Higher — single document Claude can read end-to-end; covers every endpoint + DTO in canonical shape | Comments fragmented across many files |

Stripe / GitHub / Anthropic all chose spec-first after starting code-first; the migration is harder if you start the wrong way. Choosing spec-first now at ~60 endpoints is cheaper than at 600.

### File layout

```
api/
└── openapi.yaml              # canonical spec — hand-edited

internal/common/openapi/
├── openapi.go                # //go:embed openapi.yaml + Handler() (serves spec)
└── scalar.go                 # Scalar UI handler (single HTML page)

cmd/api/main.go               # mounts /openapi.yaml + /docs at the mux
```

The spec lives in `api/` (per README convention) rather than inside `internal/common/openapi/` so it can be reviewed without exposing internal-package paths. The embed is one level up.

### Spec coverage

v0.2 ships coverage of the realistic frontend surface:

- Auth flows (login / refresh / logout / password reset / change password)
- `/v1/auth/me/capabilities` + `/v1/auth/me/activity`
- Sessions (list / revoke)
- Tenant register + read (by UUID + slug)
- User management (list / get / create / update profile / deactivate / roles / permissions / manager)
- Roles (CRUD)
- Platform admin (list tenants / get person by ID + email / impersonation sessions / stats)
- Search (omni-search)
- Audit log (self + tenant)

Roughly 50 operations. The remaining ~10 (granular tenant patches, role permission grant/revoke) are added in follow-up PRs as the frontend asks.

### Scalar over Swagger UI / Redoc

| Tool | Verdict |
|---|---|
| **Scalar** ✅ | Modern UI. Single static HTML + one `<script>` tag. Used by Anthropic API docs, Resend, Hono. Open-source, MIT. Try-It-Out works without CORS hacks. Sub-second load even for 1000-operation specs. |
| Swagger UI | 2014-era UX. Still works but feels dated. Bundled JS is heavier. |
| Redoc | 3-column layout; opinionated. Less interactive. Good for read-only documentation sites. |
| RapiDoc | Webcomponent-based; lighter than Swagger UI. Niche. |
| GitHub-style custom UI | Way out of scope. |

**Scalar** is the canonical 2024-2026 choice. The Scalar HTML is ~30 lines of static markup loaded from a single CDN URL OR embedded entirely (no network dependency at runtime — fits the Chainguard distroless model).

### Endpoint shapes registered

Per cmd/api/main.go composition root:

```go
mux.Handle("GET /openapi.yaml",      openapi.SpecHandler())   // serves the embedded YAML
mux.Handle("GET /docs",              openapi.ScalarHandler()) // serves the Scalar HTML page
mux.Handle("GET /docs/",             openapi.ScalarHandler()) // trailing-slash also
```

The existing root handler (`GET /{$}`) redirects to `/docs` so a browser hitting bare `localhost:8080` lands on something useful.

### Versioning + sealing

The spec lives in `api/openapi.yaml` (single file, no folder split — splitting starts mattering past ~150 operations per Stripe convention). When breaking changes accumulate, a `v2.yaml` lands as a parallel file + the URL space gets `/api/v2/...`. v1 stays stable; that's the API-versioning ADR's territory (separate decision).

The `info.version` field on the spec follows semver — `0.2.0` at v0.2.x, `0.3.0` when Phase 2 lands. Tags reflect URL groups (`auth`, `tenants`, `users`, etc.).

### Maintenance discipline

Three rules to keep the spec from rotting:

1. **Every PR that touches `internal/identity/ports/*.go` MUST update `api/openapi.yaml`** if the change affects wire shape. Reviewer-enforced; arch test eventually.
2. **`task ci:openapi`** (future) — lints the spec via `spectral` or `redocly cli`. Not in Wave 5.
3. **`TestArch_RouteHasSpecOperation`** (future) — Go arch test that walks `mux.Handle` registrations + asserts each has a matching operation in the spec. Not in Wave 5; explicit follow-up.

## Consequences

**Positive:**

- **Single source of truth.** Frontend, backend, partner integrations, mobile all point at one document.
- **Auto-generated typed clients.** Frontend runs `openapi-typescript` against `/openapi.yaml` → typed TS package. No hand-maintained DTOs.
- **Interactive docs out of the box.** Engineers + PMs explore the API at `/docs` without grep-spelunking through Go files.
- **AI-assistant comprehension dramatically improved.** Claude / Copilot read one document instead of fragmenting across 60 handlers.
- **Mockable before implementation.** Frontend can `prism mock api/openapi.yaml` and build against Phase 2 endpoints before backend ships them.
- **Sealed for reviewers.** Spec changes are reviewable independently of implementation.

**Negative:**

- **Spec authorship cost.** ~1 day for the initial v0.2 spec + ongoing maintenance discipline. Mitigated by Stripe / GitHub doing it for hundreds of operations — the cost is linear, not exponential.
- **Drift risk without enforcement.** If the spec stays hand-edited without a CI gate, it WILL drift. Arch test follow-up is non-optional; tracked as future work.
- **Scalar JS dependency.** Loaded from CDN by default. Mitigated by embedding the Scalar bundle OR using a self-hosted copy — Wave 5 uses CDN for simplicity; switch to embed at Phase 5 hardening.
- **Embedded YAML grows binary size.** v0.2 spec is ~30KB; trivial. At 1000+ operations the spec might be ~500KB — still fine vs the 50MB Go binary.

## Alternatives considered

1. **Code-first via swaggo/swag.** Considered. Rejected because:
   - Annotation DSL pollutes handler comments
   - Spec is hidden inside Go files; reviewers can't comment on it independently
   - Doesn't compose well with our hand-written DTOs (which have rich validation already encoded in VO ctors)
   - Industry trend is spec-first at scale

2. **Code-first via `kin-openapi` programmatic builder.** Considered. Rejected for the same review-friction reasons + adds runtime dependency on a builder library.

3. **GraphQL schema instead of OpenAPI.** Considered + rejected per implicit choice in ADR 0007 (stdlib `net/http`). GraphQL is a different ecosystem; switching architectures over docs ergonomics is over-correction.

4. **Swagger UI instead of Scalar.** Functional. Scalar's UX is meaningfully better at the same cost.

5. **Hosted docs via Mintlify / ReadMe.io / Stoplight.** $$ + external dependency + same content authored once anyway. Scalar gives 90% of the UX at $0. Re-evaluate when partner docs / external developer experience becomes a product investment.

6. **Defer the spec entirely; use Postman collection + hand-maintained TS types.** Rejected. Postman collections aren't a contract; they're a tool. The frontend pain (drift, hand-maintained DTOs) is the deal-breaker.

## Sources

- [OpenAPI Specification 3.1](https://spec.openapis.org/oas/v3.1.0) — current canonical reference.
- [Stripe OpenAPI](https://github.com/stripe/openapi) — public spec repo; the canonical large-scale spec-first reference.
- [GitHub REST API description](https://github.com/github/rest-api-description) — public spec repo.
- [Anthropic API documentation](https://docs.anthropic.com) — Scalar-rendered.
- [Scalar GitHub](https://github.com/scalar/scalar) — the UI tool.
- [openapi-typescript](https://github.com/openapi-ts/openapi-typescript) — auto-generates frontend types from the spec.
- [Spectral](https://stoplight.io/open-source/spectral) — spec linter (future `task ci:openapi` target).
- README.md:80 — the v0.1 declaration of `api/openapi.yaml` as "single source of truth".
- ADR 0007 — stdlib net/http ServeMux (the substrate this spec describes).
- ADR 0024 — Chainguard distroless static (the embed-into-binary constraint).
