# ADR 0050 — OpenAPI as code-of-record + spec/code drift CI gates

**Status:** Accepted
**Date:** 2026-05-23

## Context

ADR 0046 declared the OpenAPI spec at `api/openapi.yaml` the canonical HTTP contract — "spec-first, Go handlers conform to it." That ADR shipped ~30 operations in Wave 5. Between Waves 5 and 9, the code surface grew to **60 routes** while the spec lagged: by Wave 9.3 audit, **27 mux.Handle registrations had no matching operation in the spec** (45% drift). No CI gate caught it because no test compared the two surfaces.

Separately, Wave 9.3 audit discovered the cloud CI's `architecture` job ran arch tests against ONLY `internal/identity/integrationevents/...` — silently skipping the Wave 6 boundary-discipline test (ADR 0047) + Wave 8 route-conflict test (ADR 0049). Local `task ci` exercised all three packages; cloud CI exercised one. Three months of arch-test commits were ungated remote-side.

Industry references for spec-of-record + drift gating:

| Source | Position |
|---|---|
| **Stripe API platform** | OpenAPI spec is the source of truth; an internal "spec sync" check runs on every PR to flag operations added in Go without corresponding spec entries. |
| **GitHub REST API description repo** (`github/rest-api-description`) | Public spec repo; the canonical reference for "treat the spec as a versioned product, not a doc afterthought." |
| **Postman OpenAPI Best Practices** | "Spec drift is the #1 cause of broken integrations. Lint + sync-check on every PR." |
| **Microsoft API Guidelines §1** | "Specifications are first-class artifacts; treat their hygiene with the same rigor as code reviews." |
| **Spectral** (Stoplight) | Canonical OpenAPI linter — extends `spectral:oas` for design hygiene + lets you codify org-specific rules. Used by Cloudflare, Box, DigitalOcean. |

Without paired gates (drift detection + design lint), the spec rots silently and the published TypeScript clients + Scalar docs misrepresent reality.

## Decision

Three additive gates land together as Wave 9.3, complementing the existing arch-test discipline from ADRs 0047 (boundary) and 0049 (route conflicts):

### 1. `TestArch_RouteHasSpecOperation` — bijective drift gate

Located at `internal/identity/ports/route_spec_test.go`. Parses both surfaces + asserts they are equal:

| Side | Source | How extracted |
|---|---|---|
| Code routes | `internal/identity/ports/http.go` | `go/parser` AST walk; finds every `mux.Handle("METHOD /api/v1/...", _)` literal |
| Spec operations | `api/openapi.yaml` | `gopkg.in/yaml.v3` parse; reads top-level `paths.*[get|post|put|patch|delete|head|options]` keys |

Scope: `/api/v1/*` prefix only. Infrastructure routes (`GET /`, `GET /favicon.ico`, `GET /docs`, `GET /openapi.yaml`) live OUTSIDE the product API and are deliberately excluded from the gate.

Two failure modes caught:

- **Code orphan**: `mux.Handle` exists, no matching spec operation → add the operation OR remove the handler.
- **Spec orphan**: spec operation exists, no matching `mux.Handle` → ship the handler OR remove the spec entry.

Test runs in <50ms. Joins `task test:arch`.

**Wave 9.3 alignment:** at gate land-time, the spec had 33 operations and the code had 60. The Wave 9.3a-align sub-PR added the missing 27 operations + 18 new request/response schemas to bring both surfaces into perfect alignment. `info.version` stayed at `0.2.0` (documenting existing routes ≠ semver bump).

### 2. `task ci:openapi` — Spectral design lint

Spectral (Stoplight) is the canonical OpenAPI linter (used by Cloudflare, Box, DigitalOcean, Anthropic's docs). Runs via `npx @stoplight/spectral-cli` — no `package.json` needed; zero-config first run pulls the binary; ~5s on warm cache.

Ruleset at `.spectral.yaml` extends `spectral:oas` (the shipped canonical OpenAPI 3.x ruleset) with LeadKart-specific rules:

| Rule | Severity | Enforces |
|---|---|---|
| `operation-summary` | error | Every operation must carry a `summary` (for Scalar nav + openapi-typescript identifier names) |
| `operation-tag-defined` | error | Every operation must declare at least one tag (drives Scalar sidebar grouping) |
| `response-2xx-content-schema` | warn | 2xx responses with bodies should declare a schema |
| `no-by-x-path-lookups` | warn | Per ADR 0049: lookups by non-PK use query params, not path segments. The grandfathered `/v1/tenants/by-slug/{slug}` triggers the warning; new violations would too. |
| `paths-api-v1-prefix` | error | All paths must start with `/api/v1/` (no typo `/v1/` or `/api/`) |

Local: `task ci:openapi`.
Cloud: `openapi-lint` job in `.github/workflows/ci.yml`.

Both gates run only when `api/openapi.yaml` (or its embed copy, or `.spectral.yaml`) changes — the `dorny/paths-filter@v4` `openapi` output gates the cloud job. PRs that don't touch the spec skip the spectral cost.

### 3. CI matrix alignment — paths-filter ↔ Taskfile parity audit

Found + fixed the silent gap: cloud CI's `architecture` job listed only `internal/identity/integrationevents/...` while local `task test:arch` exercises three packages. After the fix:

```yaml
# .github/workflows/ci.yml — architecture job
- name: Run architecture tests
  run: |
    gotestsum ... -- -run "^TestArch_" -shuffle=on \
      ./internal/identity/integrationevents/... \
      ./internal/identity/app/... \
      ./internal/identity/ports/...
```

This matches the local `task test:arch` command exactly. Future arch-test additions on new packages MUST update both sides — discipline.

#### Paths-filter rules — documented

`changes` job in `.github/workflows/ci.yml` emits four area-booleans via `dorny/paths-filter@v4`:

| Output | Watched paths | Gates which jobs |
|---|---|---|
| `go` | `**.go`, `go.mod`, `go.sum`, `migrations/**`, `internal/identity/adapters/sql/**`, `sqlc.yaml`, `.golangci.yml`, the workflow itself | `unit`, `architecture`, `integration` |
| `docker` | `docker/**`, `scripts/smoke.sh`, `cmd/**`, `migrations/**`, `go.mod`, `go.sum`, the workflow | `docker-build`, `container-smoke` |
| `migrations` | `migrations/**`, `cmd/migrate/**`, the workflow | `migrations-check` |
| `openapi` (new in Wave 9.3) | `api/openapi.yaml`, `internal/common/openapi/all_routes.yaml`, `.spectral.yaml`, the workflow | `openapi-lint` |

Every downstream job carries `if: needs.changes.outputs.<area> == 'true' || github.event_name == 'push'`. **Push-to-main runs the full pipeline regardless** — this is the regression gate + the required-status-check freshness signal for branch protection. **Pull-request runs only jobs whose paths actually changed.** Pure-docs PRs skip the workflow entirely via the top-level `paths-ignore`.

This is intentional cost-saving CI design (matches Kubernetes, Terraform, golang/go canonical CI shape). Not a bug. Future contributors who add new arch-test sites OR new product surfaces MUST update the paths-filter rules so their changes still trigger the right gates.

## Consequences

**Positive:**

- **Drift is impossible at PR time.** `TestArch_RouteHasSpecOperation` fails on the FIRST commit that adds a mux.Handle without a corresponding spec entry (or vice versa).
- **Spec hygiene enforced at PR time.** Spectral catches missing summaries / untagged operations / bad path prefixes before the spec ships.
- **Frontend codegen stays accurate.** `openapi-typescript` runs against `/openapi.yaml` to generate the TS client; with the gate, the TS types match the Go API exactly.
- **Scalar UI matches reality.** Engineers/PMs reading `/docs` see what actually exists.
- **CI matrix has no silent skips.** The Wave 6 + Wave 8 + Wave 9.3 arch tests now run on cloud CI alongside the integrationevents tests.
- **Onboarding clearer.** ADR 0050 + the in-workflow comments document why CI jobs run / don't run + what each gate protects.

**Negative:**

- **Spec authorship becomes load-bearing.** Adding a route now requires a paired spec update OR the PR is blocked. Author cost: ~5-10 minutes per new operation (request schema, response schema, security, tags). Mitigated by: existing 60-operation spec has the patterns; copy-from-neighbour is fast.
- **Spectral adds an npm-based step to CI.** ~30s overhead on PRs that touch the spec. Acceptable; mitigated by paths-filter gating.
- **`task ci:openapi` requires `node` locally.** Most engineers already have it (frontend in same repo); for backend-only contributors, the spectral lint is a cloud-CI-only check.

## Alternatives considered

1. **Spec linting via Go-native rules instead of Spectral.** Considered. Rejected — Spectral has 60+ rules out of the box (`spectral:oas` ruleset covers OpenAPI 3.x hygiene comprehensively); writing equivalent Go rules is months of work for less coverage. Spectral is the industry canon (Anthropic API docs use it).

2. **Build the drift gate as a `task ci:openapi-sync` shell script (grep both files, diff).** Rejected. Brittle against YAML formatting changes + can't handle path-param normalization edge cases. `go/parser` AST + `gopkg.in/yaml.v3` is more robust + matches the existing arch-test pattern (ADRs 0047, 0049).

3. **Wait until openapi-typescript / frontend complains about drift.** Rejected. Frontend currently uses hand-maintained DTOs (Wave 5 motivation was switching to codegen). Without a backend-side gate, frontend gets a broken codegen and we discover it at integration time, not PR time.

4. **Single mega-gate via `redocly cli`.** Considered. Redocly is an alternative to Spectral with bundling + ref-resolution features. Rejected because Spectral has better Spotlight integration with Scalar (ADR 0046) and lighter dependency footprint. Re-evaluate if the spec grows past 200 operations and we want bundled splits.

5. **Run the new arch test in the existing `architecture` job only — skip Spectral.** Rejected. Drift gate + design hygiene are complementary, not redundant. Drift gate catches "wrong number of routes"; Spectral catches "untagged operation," "missing summary," "schema-less 200 response." Both gates earn their place.

## Sources

- ADR 0007 — stdlib `net/http` ServeMux (the substrate `mux.Handle` walks)
- ADR 0046 — OpenAPI spec-first contract + Scalar `/docs` (the spec this ADR protects)
- ADR 0047 — Layer-boundary discipline (the precedent for arch-test-as-CI-gate)
- ADR 0049 — URL design rules + route-registration arch gate (sibling Wave 8 gate; this ADR completes the trio)
- Stripe API engineering blog — "Why we treat our OpenAPI as code-of-record"
- GitHub REST API description — `github/rest-api-description` (the public spec repo)
- Spectral docs — stoplight.io/open-source/spectral
- Cloudflare API engineering — public ref of Spectral usage in their CI
- ThreeDotsLabs Wild Workouts — arch-test pattern (the boundary-discipline ancestor of this trio)
