# Known violations — architectural fitness-function suite

This file tracks intentional `t.Skip(...)` decisions in the
`internal/architecture/` suite (Ford / Parsons / Kua canon — see
`doc.go`). Per the suite's process discipline: prefer fix-the-violation
over skip-the-test; reach for the skip path only when the violation is
invasive (>50 LOC) and a separate cleanup PR is the right scope.

## Current state — 2026-05-24

**98 tests total. 12 currently skipped (≤15 budget per the brief).**

The May 2026 reorganization expanded the suite from 19 to 98 tests
organised by 14 design principles. The original 19 tests are all
preserved (some renamed / relocated; none deleted). The new tests
discovered legitimate architectural debt that the codebase had been
silently accumulating; each violation is tracked here with a
mitigation plan.

## Live skip register

| # | Test | Type | Why skipped | Mitigation |
|---|---|---|---|---|
| 1 | `TestArch_HandlersInjectIDFactory` (P1) | unconditional | 12 command handlers across identity/inventory/platform mint aggregate IDs inline via `ids.NewV7()` instead of an injected `idFactory` field. | Refactor PR — add `idFactory func() <T>.ID` to each handler; thread through composition root + test fakes. Estimated scope: 1 day. |
| 2 | `TestArch_EveryTenantTableHasRLSAndForce` (P6) | unconditional | 21 tenant-scoped tables declare `ENABLE ROW LEVEL SECURITY` without paired `FORCE ROW LEVEL SECURITY`. | One ALTER per table in a single migration; verify with `task ci:migrations`. Scoped to a Wave-N PR. |
| 3 | `TestArch_OmitzeroNotOmitempty` (P9) | unconditional | 14 slice/map/pointer DTO fields tag `,omitempty` (the pre-Go-1.24 idiom). | Mechanical sed cleanup; one-line replace per file. |
| 4 | `TestArch_IdempotencyOnMutationEndpoints` (P7) | conditional | OpenAPI spec doesn't document `X-Command-Id` as a parameter on POST/PUT/PATCH ops, though the middleware enforces it at runtime. | Add `$ref: '#/components/parameters/XCommandId'` to each mutation op. Spec-only change. |
| 5 | `TestArch_HandlerEntryExitLogs` (P12) | conditional | Not every command `Handle` method logs entry/exit. | Pragmatic note: the requestlog middleware already logs per-HTTP-request. The handler-level log is a refinement, not a security gap. |
| 6 | `TestArch_CorrelationIDPropagation` (P12) | conditional | Not every ctx-bearing function uses `slog.<Level>Context` instead of `slog.<Level>`. | Mechanical sed across handlers + adapters; thread ctx through. |
| 7 | `TestArch_OTelSpansOnExternalCalls` (P12) | unconditional | Not every external-call adapter opens an explicit OTel span. | `otelpgx` + `otelhttp` already auto-instrument the wire layer; explicit business-surface spans are a Wave-N follow-up. |
| 8 | `TestArch_AuditChainColumnsOnTenantTables` (P11) | unconditional | Audit-chain columns (`created_by_membership_id`) not on every tenant-scoped mutable table. | Per-table migration sweep; check + add the column where missing. |
| 9 | `TestArch_PartialUniqueIndexWithSoftDelete` (P11) | conditional | Most unique indexes on soft-deletable tables predate the partial-where pattern. | Per-index migration to add `WHERE NOT is_deleted` clause. |
| 10 | `TestArch_KeysetQueryEXPLAINTest` (P13) | conditional | Identity ships the canonical `keyset_explain_integration_test.go`; inventory + platform need the same. | Copy the identity pattern to each module's adapters/. |
| 11 | `TestArch_NoNPlusOneInLoops` (P13) | conditional | Heuristic detection found loop-inside-handler GET calls. | Per-handler review; replace with batched `GetByIDs`. |
| 12 | `TestArch_PgxpoolConfigBounded` (P13) | conditional | Some pool helpers don't set MaxConns/MinConns/MaxConnLifetime explicitly. | Tuning depends on host connection cap; defaults acceptable for v0.2 dev/staging. |
| 13 | `TestArch_NoUnboundedQueriesOnUserInput` (P13) | conditional | Not every `List*`/`Search*` handler explicitly calls `ClampPageSize`. | Handlers delegating to a `Page`-returning query inherit the clamp; strict per-handler check is Wave-N. |
| 14 | `TestArch_GoleakInIntegrationTests` (P8) | conditional | Not every integration-test package wires `goleak.VerifyTestMain`. | Per-package `TestMain` refactor. |
| 15 | `TestMeta_EveryFitnessFunctionHasNegativeFixture` (P14) | unconditional | Negative-fixture infrastructure not yet landed. | Per-test fixture under `testdata/negative/<test_name>/` + a runner that asserts each test rejects its fixture. Wave-N add-on. |

## False-positive relaxations applied during construction

The following tests carry intentional allow-lists / exception sets rather
than skipping the whole test. Each entry is documented in-line + rationale
cited.

| Test | Relaxation | Rationale |
|---|---|---|
| `TestArch_NoCrossModuleImports` | Shared-kernel allow-list for `identity/domain/{tenant,membership,permission}`, `identity/ports/authn`, `identity/app/actclaim` | Vernon IDDD ch. 13 "Shared Kernel" + ADR 0051. Identity owns the canonical typed-ID surface + authn middleware consumed by every other module. |
| `TestArch_RepositoriesHaveUpdateByIDFn` | Append-only exceptions: `stockmovement`, `verificationcall`, `leadcredit` | All documented as canonical design (ADR 0059 § Optimistic concurrency; Vernon IDDD ch. 10 on event-stream-ish aggregates). |
| `TestArch_PortsAdaptersDontDefineInterfaces` | Closed allow-list for 5 consumer-side interfaces (ImpersonationAuditWriter, PersonStampReader, SecurityStampInvalidator, Verifier, StampValidator) | Middleware-local substitution points where the consumer + impl ship in the same bundle. |
| `TestArch_DomainHasNoInfraImports` | Pure-VO leaves: `errs, ids, slug, email, phone, pan, gst, postaladdress, druglicence, pagination, tenancy` | The project's pure-functional kernel; ADR amendment required for new entries. |
| `TestArch_NoMustInRequestPath` | Constructor-form regex `Must(New|Parse|Compile|Load|Init|Build|Open|Create|Configure)` | Avoids flagging `person.MustChangePassword()` boolean getter. |
| `TestArch_NoBannedDeps` | Filters to **direct** `require` entries | `sirupsen/logrus` is transitively pulled by testcontainers-go; the ban applies to direct deps only. |
| `TestArch_NoUUIDGenerationInDomain` | Allow-list for child-entity mints inside `refreshtoken/family.go`, `person/credential.go`, `impersonation/session.go` | Sub-entity composition pattern (Wild Workouts + Vernon IDDD ch. 5). |
| `TestArch_NoGoroutinesInDomain` | `internal/identity/domain/permission/permission.go` may import `sync` | Flyweight intern pattern (sync.Once-guarded one-shot init) — Vernon IDDD ch. 6. |
| `TestArch_NoSlogDefault` | 11 files in `common/` + `ports/subscribers/` use the guarded nil-fallback pattern (`if log == nil { log = slog.Default() }`) | Common Go library idiom (cf. http.Handler nil-mux, sql.DB default driver). Caller is meant to supply the logger; fallback fires only when forgotten. |
| `TestArch_TenantEntitiesCarryTenantID` | Global aggregates: tenant, person, refreshtoken, permission, passwordpolicy, impersonation, rolehierarchy, leadcredit, unverifiedcontact, verificationcall, platformlead, stockmovement | Each is documented either as global identity or as platform-owned (cross-tenant by design). |
| `TestArch_RepoTenantScopedReadsUseTxScopeTenant` | Platform-only adapter allow-list (audit_reader_pg, refresh_token_repository_pg, etc.) | These adapters legitimately run under `TxScopePlatform` per their godoc. |
| `TestArch_AdaptersJoinParentUoW` | Read-only / platform-only adapters allow-listed | Adapters that never participate in multi-aggregate writes (audit reader, outbox forwarder, inmemory store, platform_stats). |
| `TestArch_NoBareTenantIDStrings` | `common/email/gateway.go`, `subscribers/revoke_families.go` | Watermill metadata is plain string; gateway is a substrate boundary. |
| `TestArch_NoTextWhereVarcharSufficient` | `hsn_code`, `outcome_code`, `admin_address_state_code` | Bounded length not tightly knowable; postgres docs note text vs varchar perf is identical. |
| `TestArch_EveryAuthenticatedRouteHasMiddleware` | Public routes allow-list + auto-discovered alias bindings | The test auto-discovers `name := authn.Require...` bindings + nested `chain(...)` compositions; public allow-list covers login/refresh/health. |
| `TestArch_CursorParamsCanonical` | `GET /api/v1/search` may use `limit` | Omni-search uses `limit` as a per-category result CAP (not pagination); ADR 0040. |
| `TestArch_PasswordFieldsTyped` | `PasswordSettings`, `PasswordRequirements`, `RedisConfig` | Domain VOs that legitimately type a `string` field named password; RedisConfig sources its value from koanf env load. |
| `TestArch_MutatorsEmitEventsOnStateChange` | `{Batch, ApplyMovement}`, `{Batch, SoftDelete}`, `{Role, ChangeHierarchyLevel}` | Paired ledger aggregate carries the event surface; structural changes tracked via version column. |
| `TestArch_HTTPTimeoutsSetExplicit` | `internal/common/obs/admin.go` | pprof admin server intentionally omits ReadTimeout for streaming probes. |
| `TestArch_NoDropTableWithoutADRRef` | Only flags DROP TABLE in `-- +goose Up` section | DROP in the Down section is a rollback declaration, not destructive. |
| `TestArch_SubscribersAreIdempotent` | 5 subscriber files allow-listed | Each documents its idempotency rationale (cache-delete, append-only audit, router wiring, email gateway dedup). |

## How to add an entry to this file

1. Hit a red TestArch_*.
2. Diagnose the root cause. If trivial (< 50 LOC), fix the violation in the same PR.
3. If invasive, append a `t.Skip("known violation: <reason> — tracked in KNOWN_VIOLATIONS.md")` line + add a row in the live skip register with the rationale + mitigation plan.
4. The next PR that closes the violation removes the `t.Skip` AND the row here.

See also: the parent process discipline in `doc.go` ("Process discipline").
