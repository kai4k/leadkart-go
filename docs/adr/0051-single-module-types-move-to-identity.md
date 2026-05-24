# ADR 0051 — Single-module types move out of `internal/common/` to `internal/identity/`

**Status:** Accepted
**Date:** 2026-05-23

## Context

Wave 7 (ADR 0048) merged `internal/platform/` into `internal/common/` to align with TDL Wild Workouts canon. That merge mechanically moved 13 sub-packages without re-evaluating WHERE each one truly belongs by ownership. Two were flagged in my Wave 7 follow-up audit as **single-module concerns living in the shared substrate** — anti-canon per TDL "shared kernel" rule:

| Package | Sole consumer | Anti-canon symptom |
|---|---|---|
| `internal/common/breach/` | Identity (`ChangePassword`, `ConfirmPasswordReset`, `RequestPasswordReset` exclusively) | Domain policy living in the cross-cutting kernel. Any non-Identity module that imported it would couple to a non-shared concern. |
| `internal/common/impersonation/` | Identity (`CreateImpersonationSession`, `End`, `List` exclusively) | Same — domain primitive lives outside its module. |

TDL Wild Workouts canon: domain types + their ports live inside the bounded context that owns them. Cross-cutting `internal/common/` (the shared kernel per Vernon IDDD ch. 14) is for primitives EVERY module needs — clock, ids, errs, slug, email VO, pagination, tenancy. NOT for module-specific policies even if they "feel like" infrastructure.

Industry references:

| Source | Position |
|---|---|
| **TDL Wild Workouts** | `internal/{module}/domain/` owns the module's ports + value objects; `internal/common/` is strictly cross-cutting |
| **Vaughn Vernon (IDDD ch. 14 "Shared Kernel")** | "Keep the Shared Kernel minimal... resist the urge to share anything that doesn't strictly need to be shared." |
| **Eric Evans (DDD Blue Book p.354)** | "Beware the Shared Kernel growing into a hidden Big Ball of Mud — every additional shared type increases coupling between contexts." |
| **Vladimir Khorikov "Pragmatic Clean Architecture" §11** | Aggregate-related types (factories, repositories, sentinels) belong with their aggregate. |

Without enforcement, the shared kernel WILL grow as Phase 2 modules (Platform BC, CRM, Orders) ship — each module's first-pass author will be tempted to drop a "useful general thing" into `common/` even when only their module consumes it.

## Decision

**Two move-only refactors, no semantic changes.**

### 1. `internal/common/breach/` → split

| Concern | New location |
|---|---|
| `Checker` interface + `Noop` test fake + `ErrBreached` sentinel | `internal/identity/domain/passwordpolicy/` |
| `OfflineList` concrete impl | `internal/identity/adapters/passwordpolicy_offline_list.go` |

Renames:

```
breach.Checker        → passwordpolicy.Checker
breach.Noop           → passwordpolicy.Noop
breach.ErrBreached    → passwordpolicy.ErrBreached
breach.OfflineList    → adapters.OfflinePasswordList
breach.NewOfflineList → adapters.NewOfflinePasswordList
```

Domain layer (`passwordpolicy.Checker`) is consumer-defined per Cheney; the concrete adapter (`adapters.OfflinePasswordList`) lives where the rest of identity's pg-backed substrate lives. Boundary discipline (ADR 0047) preserved — app/command handlers depend on `passwordpolicy.Checker` interface only; composition root wires the concrete impl.

### 2. `internal/common/impersonation/` → split

| Concern | New location |
|---|---|
| `Session` value type + `Store` interface + `ErrSessionNotFound`/`ErrInvalidSession` + `MinReasonLength`/`MaxDuration`/`DefaultDuration` + `NewSession()`/`UnmarshalSession()` | `internal/identity/domain/impersonation/` |
| `InMemoryStore` concrete impl | `internal/identity/adapters/impersonation_inmemory_store.go` |
| `AuditEntry` + `AuditWriter` + `PgAuditWriter` + `NoopAuditWriter` (Phase 2 ready; not yet wired) | `internal/identity/adapters/impersonation_audit_pg.go` |

Renames (collision avoidance in flat `adapters/`):

```
impersonation.InMemoryStore       → adapters.ImpersonationInMemoryStore
impersonation.NewInMemoryStore    → adapters.NewImpersonationInMemoryStore
impersonation.AuditEntry          → adapters.ImpersonationAuditEntry
impersonation.AuditWriter         → adapters.ImpersonationAuditWriter
impersonation.PgAuditWriter       → adapters.ImpersonationAuditWriterPG
impersonation.NoopAuditWriter     → adapters.ImpersonationAuditWriterNoop
```

Domain surface (`impersonation.Session`, `impersonation.Store`, `impersonation.ErrSessionNotFound`, validation constants) keeps its names — only the import path moves. App-layer command/query handlers see no API change beyond the import path rewrite.

A NEW `UnmarshalSession` factory is added to the domain package so adapters can rehydrate from persisted state without going through the validation-aware `NewSession()` (the Redis-backed Phase 2 store + the InMemoryStore both load from "trusted storage" semantics — direct field assignment).

## Consequences

**Positive:**

- **Shared kernel stays minimal.** `internal/common/` is now strictly cross-cutting primitives. No module-specific policies live there. Reviewers + AI agents reading `internal/common/*` see "yes, every module needs this" — no exceptions.
- **Future-module-author discipline.** Phase 2 Platform / CRM / Orders authors have a clean reference: domain policy → `internal/{module}/domain/<policy>/`; concrete adapter → `internal/{module}/adapters/`. The pattern is visible in the repo's own history.
- **Test boundary cleaner.** `passwordpolicy.Noop` is now in the consuming module — Identity tests don't reach into `common/` for a test double.
- **Phase 2 impersonation audit table is now wired where it'll be consumed.** `ImpersonationAuditWriterPG` lives in `adapters/` ready for Wave 4.1's follow-up activation (ADR 0056 picks up the trail).
- **Naming collisions avoided.** Flat `adapters/` has multiple Pg-backed writers; the explicit `Impersonation*` prefix prevents a future `audit.AuditWriter` interface from clashing with `impersonation.AuditWriter`.

**Negative:**

- **Two-tier rename touches ~12 caller sites + tests.** Mechanical; one PR per move; no semantic risk.
- **The flat `adapters/` package picks up two more file types** (`passwordpolicy_offline_list.go`, `impersonation_inmemory_store.go`, `impersonation_audit_pg.go`). At ~15 files now; still well below the package-split threshold per TDL canon.

## Alternatives considered

1. **Leave them in `internal/common/` per "consistency with Wave 7."** Rejected. Wave 7 was a mechanical merge; this ADR is the deliberate re-evaluation. ADR 0048's stance is "merge the tiers"; this ADR's stance is "now place each thing correctly within the merged tier." Compatible, not contradictory.

2. **Move `breach/` to `internal/identity/adapters/passwordpolicy/` (sub-package) instead of `internal/identity/domain/passwordpolicy/`.** Rejected. The `Checker` is a port (domain interface); `OfflineList` is the adapter. Standard hexagonal split. Putting the port in the adapter folder would invert the dependency.

3. **Keep `AuditWriter` types in `common/audit/` alongside the existing `audit.Writer`.** Rejected. Different concerns — `audit.Writer` writes to `buildingblocks.audit_log_entry` (cross-cutting); `ImpersonationAuditWriter` writes to `buildingblocks.admin_impersonation_audit` (Identity-specific Wave 4.1 surface). Same DB schema doesn't mean same module boundary.

4. **Co-locate the interface + concrete in `internal/identity/domain/impersonation/`** (skip the adapter split). Rejected. The concrete `InMemoryStore` is fine to ship in domain BUT future `RedisStore` (Phase 2) will import `redis-go` — that's substrate; domain stays substrate-free per ADR 0047.

## Sources

- ADR 0001 — Modular monolith (the bounded-context boundary this refactor respects)
- ADR 0002 — Hexagonal + DDD (the port/adapter split this enforces)
- ADR 0047 — Layer-boundary discipline (the arch test that keeps this rule honest)
- ADR 0048 — `platform/` → `common/` merge (the prior layout this refactor refines)
- ThreeDotsLabs Wild Workouts — `internal/{module}/domain/` + `internal/common/` pattern reference
- Vaughn Vernon, *Implementing Domain-Driven Design* — Shared Kernel discipline (ch. 14)
- Eric Evans, *Domain-Driven Design* — "Beware the Shared Kernel" (p.354)
- Vladimir Khorikov, *Pragmatic Clean Architecture* — Aggregate-related types belong with their aggregate (§11)


## Fitness function

`TestArch_NoCrossModuleImports` (in `internal/architecture/`).

Cross-module imports allow-list documents the shared-kernel exceptions (identity/domain/{tenant,membership,permission}, identity/ports/authn, identity/app/actclaim) — the closed set defined here.
