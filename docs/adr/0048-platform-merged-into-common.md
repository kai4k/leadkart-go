# ADR 0048 — `internal/platform/` merged into `internal/common/` (TDL strict-canon alignment)

**Status:** Accepted
**Date:** 2026-05-22
**Supersedes:** the two-tier `common/` (pure) + `platform/` (infra) split from ADR 0047 prose
**Extended by:** ADR 0051 (single-module types move out of `common/`)

> **Wave 9.1a+b update (2026-05-23):** two of the 13 sub-packages listed below as moved to `internal/common/` have SINCE moved out again per ADR 0051:
> - `internal/common/breach/` → `internal/identity/{domain/passwordpolicy,adapters}/`
> - `internal/common/impersonation/` → `internal/identity/{domain,adapters}/`
>
> Both were Identity-only consumers; per the TDL single-module rule + Vernon IDDD "Shared Kernel minimalism," domain policy + per-module ports belong in their owning bounded context, not the cross-cutting kernel. ADR 0051 documents the rationale. The 11 remaining packages (`audit, cache, config, email, httpmw, idempotency, jobs, messaging, obs, openapi, pg`) are genuinely cross-cutting and stay in `internal/common/`.

## Context

Phase 1 + 1.5 introduced two cross-cutting tiers in the LeadKart-Go repo:

| Tier | Folder | Imports allowed | Example contents |
|---|---|---|---|
| **Pure** | `internal/common/` | stdlib + `google/uuid` only | clock, slug, email VO, ids, errs, pagination |
| **Infra-bearing** | `internal/platform/` | pgx, redis, watermill, OTel, etc. | pg, cache, messaging, audit, obs, httpmw |

The split was a LeadKart-Go-specific stylistic choice — the rationale was "domain-can't-import-infra rule visible-by-folder-name." A reviewer (or AI agent) scanning a file's imports could see `internal/common/...` and immediately know "safe for domain," vs `internal/platform/...` → "infra; domain stays away."

In practice, three issues surfaced:

1. **The split doesn't enforce anything mechanically.** Folder names are labels; the actual boundary enforcement is the import graph + `TestArch_AppDoesNotImportForbidden` arch test (ADR 0047). Renaming `platform/` → `common/infra/` would not change a single test outcome or Docker spin-up — domain tests stay fast because of the imports inside domain files, not because of where infra lives.

2. **Naming collision with Phase 2.** The LeadKart **Platform** bounded context (marketplace + lead credits + verification calls per BRD.md) lands in Phase 2. Having `internal/platform/` mean "cross-cutting infra" today AND `internal/platform/` mean "the Platform BC" tomorrow is irreconcilable.

3. **TDL Wild Workouts (the canonical Go DDD reference per CLAUDE.md doctrine) uses ONE folder: `internal/common/`.** Their `common/` mixes pure + infra freely — `internal/common/server/` (HTTP infra) lives next to `internal/common/decorator/` (pure CQRS). The split LeadKart-Go invented is an extra layer of formalism TDL doesn't share. Per CLAUDE.md "When in doubt, trust the actual code [of TDL] over both [rules + ADRs]" — TDL is the authoritative reference for our Go canon.

Industry references for the "one shared folder" pattern:

| Source | Folder name | Comment |
|---|---|---|
| **TDL Wild Workouts** | `internal/common/` | Single tier; pure + infra in same root |
| **Microsoft eShopOnContainers** | `BuildingBlocks/` | Single tier; the canonical .NET DDD reference |
| **Vaughn Vernon (IDDD)** | `BuildingBlocks/` | Single tier; the canonical book reference |
| **Brandur Leach / Crunchy** | flat top-level packages | No single root; per-concern (`db/`, `email/`, `kvstore/`) |
| **Go stdlib** | per-concern packages | `net/`, `crypto/`, `encoding/` — flat |
| **Microsoft Azure SDK for Go** | per-concern packages | `azcore/`, `azidentity/`, `azlog/` |

**Two-tier (LeadKart's previous shape) is an outlier.** The canonical Go DDD pattern is single-folder OR flat-per-concern; nobody else splits pure from infra by folder.

## Decision

**Merge `internal/platform/` into `internal/common/`.** All 13 sub-packages move:

```
Before                                  After
─────────────────────────────────       ─────────────────────────────────
internal/platform/audit/                internal/common/audit/
internal/platform/breach/               internal/common/breach/
internal/platform/cache/                internal/common/cache/
internal/platform/config/               internal/common/config/
internal/platform/email/                internal/common/email/  (merged with existing email VO)
internal/platform/httpmw/               internal/common/httpmw/
internal/platform/idempotency/          internal/common/idempotency/
internal/platform/impersonation/        internal/common/impersonation/
internal/platform/jobs/                 internal/common/jobs/
internal/platform/messaging/            internal/common/messaging/
internal/platform/obs/                  internal/common/obs/
internal/platform/openapi/              internal/common/openapi/
internal/platform/pg/                   internal/common/pg/
```

`internal/platform/` is now AVAILABLE for the Phase 2 Platform bounded context (marketplace + lead credits + verification calls) when it lands.

### Email merge

The pre-Wave-7 split had `internal/common/email/` (Address VO) and `internal/platform/email/` (Gateway interface + Recorder). Both used `package email`. Merge consolidates into a single `internal/common/email/` package with:

- `email.go` — `Address` VO (validating email-string), `ErrInvalid` (VO validation error)
- `gateway.go` — `Gateway` interface, `Message` VO, `Recorder` test double, `ErrInvalidMessage` (Message validation error — renamed from `ErrInvalid` to disambiguate within one package)
- `email_test.go` — VO tests
- `gateway_test.go` — Gateway + Recorder tests

`ErrInvalid` (Address VO) and `ErrInvalidMessage` (gateway Message) coexist in the same package — different concerns, different names. No external callers of `email.ErrInvalid` were broken; the rename only touched gateway-side validation paths.

### Boundary enforcement is unchanged

The `TestArch_AppDoesNotImportForbidden` test (ADR 0047) bans the SUBSTRATE imports:
- `internal/identity/adapters/db` (sqlc)
- `internal/identity/adapters` (concrete repos)
- `github.com/jackc/pgx/v5` + `pgxpool` + `pgtype` (DB driver)

**None of those mention `platform/` or `common/`.** The arch test was always boundary-by-import-path, never folder-by-name. So the merge is mechanically a no-op for the gate.

### Why this is right per CLAUDE.md doctrine

CLAUDE.md (the project's first-read instruction file) names ThreeDotsLabs' Wild Workouts as Tier 1 canonical authority for Go DDD patterns. The "Strict TDL style" rule the project commits to means TDL's actual repo shape IS the reference. TDL's `internal/common/` answers the "where does shared infra live" question; we follow.

CLAUDE.md "When in doubt" guidance: "Trust `.claude/rules/` over this map. Trust ADRs over rules when they conflict. **Trust the actual code over both** — and if the code drifts from doctrine, that's a finding: fix one or the other." TDL's code says `common/`. We were drifting. This ADR closes the drift.

### What stays the same

- Pure substrates (clock, slug, ids, etc.) keep their existing `internal/common/` location — no change.
- Domain interfaces (`tenant.Repository`, `audit.Reader`, `pg.UnitOfWork`, etc.) keep their consumer-defined location — interface near consumer, impl near substrate.
- The `TestArch_AppDoesNotImportForbidden` arch test gates the boundary; the merge doesn't relax it.
- Composition root (`cmd/api/main.go`) wires everything from one place; the merge doesn't change the wiring topology.

## Consequences

**Positive:**

- **TDL canon alignment** — folder layout matches the project's named Tier 1 reference.
- **Phase 2 unblocked** — `internal/platform/` is free for the Platform bounded context.
- **Simpler mental model** — one cross-cutting root instead of two; fewer "which tier does this belong in?" debates.
- **Matches every other DDD reference** — Microsoft eShop, Vernon IDDD, even most Go shops cluster shared things under one root.
- **Composition root reads cleaner** — `cmd/api/main.go` no longer has two parallel import groups for `common/*` and `platform/*`.

**Negative:**

- **One-time churn** — 13 packages moved, ~80 files touched (import-path rewrite). Mechanical sed; no semantic risk.
- **Lost visual hint** — reviewers can no longer scan "common/ = safe-for-domain" by folder name alone. Mitigated by:
  - Arch tests do the actual enforcement (always did; the folder was redundant signal).
  - File-level imports are 1 cmd+F away.
- **`email` package now has two error sentinels** (`ErrInvalid` for Address, `ErrInvalidMessage` for Message). Disambiguating names; no downstream churn since `ErrInvalidMessage` was the only renamed identifier and only the gateway test referenced it.

## Alternatives considered

1. **Keep the two-tier split + rename Phase 2 BC to `marketplace/`.** Considered. Rejected because:
   - The split is non-canon (TDL doesn't do it; eShop doesn't do it).
   - "Marketplace" arguably IS a better name for the Platform BC, but renaming the BC is a forward decision; renaming `platform/` (existing infra) is the backward-compatible move that doesn't pre-commit Phase 2's naming.
   - The split adds a mental model cost that arch tests already eliminate.

2. **Three-tier: `common/pure/`, `common/infra/`, `common/wiring/`.** Over-formalist. Adds nesting + ceremony without value. Nobody does this.

3. **Flat per-concern at `internal/` root: `internal/pg/`, `internal/cache/`, etc.** Brandur / stdlib style. Loses the "this is cross-cutting, not domain-specific" signal that the `common/` root carries. Also flattens `internal/identity/`, `internal/crm/`, etc. into the same namespace as `pg`, which is visually confusing.

4. **Defer the merge to Phase 2 (do it when Platform BC lands).** Rejected — the rename is mechanical; doing it now is one PR; doing it later means doing it AFTER Phase 2 work starts (more conflict surface).

## Sources

- **CLAUDE.md** § "Doctrine sources Tier 1" — ThreeDotsLabs Wild Workouts named as canonical Go DDD reference.
- **ThreeDotsLabs Wild Workouts repo** — `internal/common/{server,decorator,errors,logs,metrics,auth,client,genproto}/` shows the canonical single-tier shape.
- **eShopOnContainers** — `BuildingBlocks/` as the .NET parallel; same concept, different vocabulary.
- **Vaughn Vernon — IDDD** — book § "Building Blocks" treating shared cross-context infra as a single namespace.
- **ADR 0001** — Modular monolith topology (the BC structure this merge respects).
- **ADR 0047** — Layer-boundary discipline (the arch test that does the real enforcement; merge doesn't relax it).
- **CLAUDE.md** § "When in doubt" — "Trust the actual code [TDL's repo] over both [rules + ADRs]" — directly justifies aligning to TDL's actual layout.
