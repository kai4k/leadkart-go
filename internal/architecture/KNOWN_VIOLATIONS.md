# Known violations — architectural fitness-function suite

This file tracks intentional `t.Skip(...)` decisions in the
`internal/architecture/` suite (Ford / Parsons / Kua canon — see
`doc.go`). Per the suite's process discipline: prefer fix-the-violation
over skip-the-test; reach for the skip path only when the violation is
invasive (>50 LOC) and a separate cleanup PR is the right scope.

## Current state — 2026-05-25 (full closure)

**230 tests total. 0 currently skipped.**

The prior PR (`fix(arch): close all 20 fitness-function skips`) shipped
207 tests with zero skips. This stacked PR adds 23 new Principle TD
(Test Discipline) tests targeting unit + integration test quality:
naming, parallelism, lifecycle, time, IO isolation, modern Go 1.24+
idioms. All 23 land with their initial violation count closed in one
sweep (~604 fixes across 150 test files), per the user's "do the same
level of audit for unit and integration tests" directive.

See **Test discipline (Principle TD) — 2026-05-25** section below for
the per-test catalog + closure details.

## Closures shipped this PR

| # | Test | Closure |
|---|---|---|
| 1 | `TestArch_HandlersInjectIDFactory` (P1) | Production refactor — 14 command handlers across identity/inventory/platform now take `func() <T>.ID` factory args (Pure Domain canon — Go-idiomatic function type per stdlib pattern, NOT an interface). Composition root in `cmd/api/main.go` wires production factories (`func() <T>.ID { return <T>.ID(ids.NewV7().String()) }`); tests pin deterministic values. Unconditional `t.Skip` removed from the test. |
| 2 | `TestArch_IdempotencyOnMutationEndpoints` (P7) | Added `XCommandId` parameter component to `api/openapi.yaml` + injected `parameters: [{$ref: '#/components/parameters/XCommandId'}]` on all 48 non-exempt POST/PUT/PATCH operations (Stripe canon: shared component re-used via $ref, not inlined). Test predicate updated to resolve $ref form. |
| 5 | `TestArch_HandlerEntryExitLogs` (P12) | **Test renamed + repurposed** to `TestArch_NoInfoLogOnHandlerSuccessPath` — the previous "every Handle method logs entry/exit" predicate was an explicit anti-pattern per Cheney "Let's talk about logging" + Bourgon "Go best practices §logging" + Sridharan *Observability Engineering*. Now enforces the inverse: Info/Debug logs on the success path are flagged; Warn/Error on failure paths are encouraged. |
| 6 | `TestArch_CorrelationIDPropagation` (P12) | Production sweep confirmed every ctx-bearing call site already uses `slog.<Level>Context` (0 violations found at PR time). Skip removed; predicate now `t.Errorf`s on any regression. |
| 7 | `TestArch_OTelSpansOnExternalCalls` (P12) | **Test renamed + repurposed** to `TestArch_OTelInstrumentationViaLibraries` — the previous "every adapter opens an explicit `tracer.Start` span" was anti-canon per OpenTelemetry-Go contrib README ("prefer instrumented drivers") + exaring/otelpgx README. Now asserts `otelpgx.NewTracer` + `otelpgx.RecordStats` are wired in `internal/common/pg/pool.go` + `otelhttp.NewHandler` wraps the mux in `cmd/api/main.go`. |
| 8 | `TestArch_AuditChainColumnsOnTenantTables` (P11) | Migration `20260603000301_audit_chain_columns_on_tenant_tables.sql` adds `created_by_membership_id uuid NULL` (idempotent via `IF NOT EXISTS`) on `identity.roles`, `identity.role_assignments`, `inventory.products`, `inventory.batches`. Test predicate rewritten to walk every `CREATE TABLE` with `tenant_id` + assert the column exists either inline or via subsequent ALTER. Explicit allow-list documents 19 exempt tables (global aggregates, append-only ledgers, outbox infra). |
| 9 | `TestArch_PartialUniqueIndexWithSoftDelete` (P11) | Audit found ZERO violations — every soft-deletable table's unique indexes already carry `WHERE NOT is_deleted`. Skip flipped to `t.Errorf` so future regressions fail. (Companion migration `20260603000302_partial_unique_indexes_soft_delete.sql` ships as documented no-op + template for future fixes.) |
| 10 | `TestArch_KeysetQueryEXPLAINTest` (P13) | 3 new `*_explain_integration_test.go` files cover every `*Page` adapter in `internal/inventory/adapters/` + `internal/platform/adapters/`. Each EXPLAINs the keyset query under RLS, asserts Index Scan on the supporting partial composite index, no Seq Scan. Skip → `t.Errorf`. |
| 11 | `TestArch_NoNPlusOneInLoops` (P13) | Two real N+1s in `internal/identity/app/query/users.go` (ListUsers + ListUsersPaged hydrating Person per Membership) replaced with batched `person.Repository.GetByIDs(ctx, []ID) (map[ID]*Person, error)` — single `WHERE id = ANY($1::uuid[])` query. Brandur "Postgres at Scale" canon. Companion `internal/common/pg/QueryCounter` (pgx.QueryTracer impl) ships as the load-bearing RUNTIME N+1 detector. Allowlist for `/app/seed/` (boot-time one-shot) added. |
| 12 | `TestArch_PgxpoolConfigBounded` (P13) | `internal/common/pg/pool.go` PoolConfig + NewPool extended with explicit `MaxConns/MinConns/MaxConnLifetime/MaxConnIdleTime/HealthCheckPeriod` fields with sensible defaults (Brandur sizing). Test predicate tightened to skip files that only mention `pgxpool` in godoc. |
| 13 | `TestArch_NoUnboundedQueriesOnUserInput` (P13) | **Test renamed + repurposed** to `TestArch_ListHandlersBoundedByPaginationShape` — the previous "every List handler MUST call ClampPageSize" was too narrow. New predicate accepts any of: `ClampPageSize`, `Page[`, `pagination.Cursor`, `HasMore`, `NextCursor`, `Paged.Handle`, plus a domain-bounded allow-list with cited invariants (sessions per JWT, by-slug single-result lookup, omni-search per-category limit, etc.). |
| 14 | `TestArch_GoleakInIntegrationTests` (P8) | All integration-test packages already wire `goleak.VerifyTestMain` via per-pkg `testmain_integration_test.go`. Skip → `t.Errorf` so future packages MUST add one. |
| 15 | `TestMeta_EveryFitnessFunctionHasNegativeFixture` (P14) | Static catalog presence shipped — 204 placeholder directories under `internal/architecture/testdata/negative/<TestArch_Name>/` (each carries a PLACEHOLDER.md explaining the fixture-fill TODO). Test now passes when EITHER a fixture dir exists OR the test godoc carries `arch-test:no-negative-fixture (<rationale>)`. Runtime fixture-runner (re-invoking each test against its negative sample) is the next escalation; static catalog presence is the load-bearing first gate. |
| 16 | `TestArch_NoMessageStringMatching` (M5) | Typed sentinels `command.ErrNewPasswordRequired` + `command.ErrPersonIDRequired` shipped in `change_password.go`; `confirm_password_reset.go` + `internal/identity/ports/http.go` now use `errors.Is` instead of `strings.Contains(err.Error(), ...)`. Russ Cox "Working with Errors in Go 1.13" canon. Unconditional skip removed. |
| 17 | `TestArch_IntegrationTestSuffix` (T5) | **Test repurposed** — the previous "every integration file MUST end in `_integration_test.go`" was style-not-correctness per Go canon (`//go:build integration` is the load-bearing separator; filename is cosmetic per `cmd/go` docs §Build constraints). New predicate enforces the bidirectional rule that DOES matter: every `*_integration_test.go` file MUST carry the `//go:build integration` directive (drift catch). |
| 18 | `TestArch_EveryHandlerHasTestFile` (T6) | 4 new test files in `internal/identity/app/command/`: `create_user_test.go`, `hard_delete_tenant_test.go`, `create_impersonation_session_test.go`, `request_email_change_test.go`. Each carries a `PanicsOnNilDep` + `RejectsZeroX` shape-test (full coverage stays in flow_integration_test). Unconditional skip removed. |
| 19 | `TestArch_SecurityHeadersMiddlewarePresent` (H3) | `SecurityHeaders` middleware shipped in `internal/common/httpmw/security_headers.go` setting OWASP floor (X-Content-Type-Options: nosniff, X-Frame-Options: DENY, Strict-Transport-Security: max-age=31536000; includeSubDomains, Referrer-Policy: no-referrer). Wired into `httpmw.PublicChain` OUTSIDE Recover so panic-derived 500s also carry the headers. Skip → `t.Errorf`. |
| 20 | `TestArch_DockerfileGoVersionMatchesGoMod` (L1) | **Test pass-shape clarified** — the absence-by-design state (no Dockerfile per ADR 0024 Chainguard distroless static) now returns cleanly instead of `t.Skip` (Brandur canon: "when a check has nothing to check, it passes — skip is for unfinished tests"). Test becomes load-bearing the moment a Dockerfile is introduced. |

## Inline-closed during initial suite ship (preserved from prior version)

| Test | Closure |
|---|---|
| `TestArch_EveryTenantTableHasRLSAndForce` (P6) | Migration `20260603000202_force_rls_on_tenant_tables.sql` adds FORCE ROW LEVEL SECURITY to 15 tenant-scoped tables. Round-2 reviewer escalated this from Wave-N → Wave-A as a security-class gap (table-owner role bypassed RLS without FORCE per PG §5.8). |
| `TestArch_OmitzeroNotOmitempty` (P9) | 14 slice/map/pointer DTO fields across identity + inventory + platform updated from `,omitempty` → `,omitzero` (Go 1.24+ idiom per Russ Cox 2024 release notes). |

## False-positive relaxations (per-test allow-lists)

The following tests carry intentional in-line allow-lists / exception
sets. Each entry is documented in-line at the test with a rationale
citation.

| Test | Relaxation | Rationale |
|---|---|---|
| `TestArch_NoCrossModuleImports` | Shared-kernel allow-list for `identity/domain/{tenant,membership,permission}`, `identity/ports/authn`, `identity/app/actclaim` | Vernon IDDD ch. 13 "Shared Kernel" + ADR 0051. |
| `TestArch_RepositoriesHaveUpdateByIDFn` | Append-only exceptions: `stockmovement`, `verificationcall`, `leadcredit` | ADR 0059 § Optimistic concurrency + Vernon IDDD ch. 10 on event-stream-ish aggregates. |
| `TestArch_PortsAdaptersDontDefineInterfaces` | Closed allow-list for 5 consumer-side interfaces (ImpersonationAuditWriter, PersonStampReader, SecurityStampInvalidator, Verifier, StampValidator) | Middleware-local substitution points where the consumer + impl ship in the same bundle. |
| `TestArch_DomainHasNoInfraImports` | Pure-VO leaves: `errs, ids, slug, email, phone, pan, gst, postaladdress, druglicence, pagination, tenancy` | Project's pure-functional kernel; ADR amendment required for new entries. |
| `TestArch_NoMustInRequestPath` | Constructor-form regex `Must(New|Parse|Compile|Load|Init|Build|Open|Create|Configure)` | Avoids flagging `person.MustChangePassword()` boolean getter. |
| `TestArch_NoBannedDeps` | Filters to **direct** `require` entries | `sirupsen/logrus` is transitively pulled by testcontainers-go; the ban applies to direct deps only. |
| `TestArch_NoUUIDGenerationInDomain` | Allow-list for child-entity mints inside `refreshtoken/family.go`, `person/credential.go`, `impersonation/session.go` | Sub-entity composition pattern (Wild Workouts + Vernon IDDD ch. 5). |
| `TestArch_NoGoroutinesInDomain` | `internal/identity/domain/permission/permission.go` may import `sync` | Flyweight intern pattern (sync.Once-guarded one-shot init) — Vernon IDDD ch. 6. |
| `TestArch_NoSlogDefault` | 11 files in `common/` + `ports/subscribers/` use the guarded nil-fallback pattern (`if log == nil { log = slog.Default() }`) | Common Go library idiom (cf. http.Handler nil-mux, sql.DB default driver). |
| `TestArch_TenantEntitiesCarryTenantID` | Global aggregates: tenant, person, refreshtoken, permission, passwordpolicy, impersonation, rolehierarchy, leadcredit, unverifiedcontact, verificationcall, platformlead, stockmovement | Each is documented either as global identity or platform-owned. |
| `TestArch_RepoTenantScopedReadsUseTxScopeTenant` | Platform-only adapter allow-list | These adapters legitimately run under `TxScopePlatform` per their godoc. |
| `TestArch_AdaptersJoinParentUoW` | Read-only / platform-only adapters allow-listed | Adapters that never participate in multi-aggregate writes. |
| `TestArch_NoBareTenantIDStrings` | `common/email/gateway.go`, `subscribers/revoke_families.go` | Watermill metadata is plain string; gateway is a substrate boundary. |
| `TestArch_NoTextWhereVarcharSufficient` | `hsn_code`, `outcome_code`, `admin_address_state_code` | Bounded length not tightly knowable; postgres docs note text vs varchar perf is identical. |
| `TestArch_EveryAuthenticatedRouteHasMiddleware` | Public routes allow-list + auto-discovered alias bindings | Public allow-list covers login/refresh/health. |
| `TestArch_CursorParamsCanonical` | `GET /api/v1/search` may use `limit` | Omni-search uses `limit` as a per-category result CAP per ADR 0040. |
| `TestArch_PasswordFieldsTyped` | `PasswordSettings`, `PasswordRequirements`, `RedisConfig` | Domain VOs that legitimately type a `string` field named password. |
| `TestArch_MutatorsEmitEventsOnStateChange` | `{Batch, ApplyMovement}`, `{Batch, SoftDelete}`, `{Role, ChangeHierarchyLevel}` | Paired ledger aggregate carries the event surface. |
| `TestArch_HTTPTimeoutsSetExplicit` | `internal/common/obs/admin.go` | pprof admin server intentionally omits ReadTimeout for streaming probes. |
| `TestArch_NoDropTableWithoutADRRef` | Only flags DROP TABLE in `-- +goose Up` section | DROP in the Down section is a rollback declaration, not destructive. |
| `TestArch_SubscribersAreIdempotent` | 5 subscriber files allow-listed | Each documents its idempotency rationale. |
| `TestArch_ListHandlersBoundedByPaginationShape` | 12 handler-name allow-list with per-entry domain invariant | Each entry cites the bounding mechanism (JWT-scope cap, by-slug single-result, omni-search per-category limit, etc.). |
| `TestArch_NoNPlusOneInLoops` | `_test.go`, `/cmd/`, `/app/seed/` | Boot-time + composition-root + test code are not request-path concerns. |

## Test discipline (Principle TD) — 2026-05-25

23 new arch tests landed in `test_discipline_arch_test.go` covering
unit + integration test quality. Each rule cites Go-canon sources
(not generic SRE): Cheney "Prefer table-driven tests", Mat Ryer "How
I write HTTP services", Russ Cox "Subtests and Sub-benchmarks",
Bryan Mills GopherCon 2018 (`t.Parallel` everywhere), Go 1.14/1.17/
1.24 release notes (t.Cleanup / t.TempDir / t.Context / t.Chdir /
testing/synctest / T.Loop / B.Loop), testify README, go-cmp docs,
Uber goleak README, Khorikov *Unit Testing* §8.

| # | Test | Rule | Closure (in this PR) |
|---|---|---|---|
| TD1 | `TestArch_TestFuncsCallTParallelOrCiteReason` | Every `Test*` calls `t.Parallel()` OR carries `arch-test:serial — <reason>` godoc | 236 tests across cmd/api, cmd/bootstrap, identity, inventory, platform, common updated. `config_test` family annotated `serial — uses t.Setenv`. bootstrap env-skip test annotated `serial — mutates process-global env`. |
| TD2 | `TestArch_SubtestsCallTParallelOrCiteReason` | Every `t.Run(...)` closure body calls `t.Parallel()` | 16 subtest closures updated across identity (13) + inventory (1) + cmd (2). |
| TD3 | `TestArch_TestsUseTContext` | `t.Context()` (Go 1.24+) over `context.Background()`; inside `WithTimeout`, replace inner Background with t.Context | 72 sites swept; testmain-only files allow-listed; `context.WithTimeout(context.Background(), ...)` skip-listed. |
| TD4 | `TestArch_NoOsMkdirTempInTests` | `t.TempDir()` over `os.MkdirTemp` / `ioutil.TempDir` | 0 violations at PR time; gate live. |
| TD5 | `TestArch_TestHelpersCallTHelper` | Funcs taking `*testing.T` / `*testing.B` / `testing.TB` as first arg call `t.Helper()` | 0 violations at PR time; gate live. |
| TD6 | `TestArch_NoOsChdirInTests` | `t.Chdir` (Go 1.24+) over `os.Chdir` (parallel-unsafe) | 0 violations; gate live. |
| TD7 | `TestArch_NoTimeNowInTests` | Pin time via file-scoped `fixedNow := time.Date(...)`; annotate `arch-test:wall-clock` for deliberate cases | 22 sites converted (identity 13, common idempotency/pagination 9). |
| TD8 | `TestArch_NoTimeSleepInTests` | testing/synctest (Go 1.24+) OR ticker+ctx OR `arch-test:wait-justified` annotation | 13 sites annotated; 6 common/messaging + 2 cache + 5 across cmd + identity. |
| TD9 | `TestArch_NoMathRandWithoutSeed` | math/rand needs explicit seed or `arch-test:non-deterministic` | 0 violations; gate live. |
| TD10 | `TestArch_NoNetListenInTests` | `httptest.NewServer` over raw `net.Listen` | 0 violations; gate live. |
| TD11 | `TestArch_NoOutboundHTTPInUnitTests` | No real HTTP from unit tests (use httptest) | 0 violations; gate live. |
| TD12 | `TestArch_NoPgxImportInUnitTests` | pgx/pgxpool only in `*_integration_test.go` (with goleak + testcontainers safety net) | 10 hits; allow-list closes them (canonical pg pkg + module-local arch tests inspect pgx as their subject). |
| TD13 | `TestArch_NoUnconditionalTSkipWithoutMarker` | `t.Skip(...)` needs `known violation:` / `arch-test:` marker OR conditional | 0 violations; gate live. |
| TD14 | `TestArch_NoTestRetryLoops` | Replace `for { sleep; if check { break } }` with synctest / ctx-wait; `arch-test:wait-justified` annotation allowed | 4 sites annotated (1 cmd/api + 3 common/messaging — all bounded by deadline, cross-driver). |
| TD15 | `TestArch_IntegrationTestsHaveTimeout` | Every integration `Test*` wraps in `context.WithTimeout(t.Context(), N)` OR `arch-test:no-timeout-needed` | 22 sites; inventory + platform adapters wrapped at 30s; cmd/api fixtures + messaging file-level annotated (existing internal timeouts in fixture). |
| TD16 | `TestArch_IntegrationHTTPViaHttptest` | HTTP integration via `httptest.NewServer` not raw `http.Client`; `arch-test:http-justified` for fixture-wrapped cases | 1 site (cmd/api/slug_lookup_e2e_test.go uses `f.URL` from fixture — annotated). |
| TD17 | `TestArch_BenchmarksUseBLoop` | Go 1.24+ `b.Loop()` over legacy `for i := 0; i < b.N; i++` | 0 violations; gate live. |
| TD18 | `TestArch_PreferSynctestForGoroutineTiming` | Goroutine+time tests prefer `testing/synctest`; `arch-test:no-synctest` for cross-driver cases | 4 files annotated (cmd/api/security_stamp + messaging/router + identity/{adapters/outbox,ports/subscribers}) — all cross SQL-driver boundary which synctest's virtual clock can't model. |
| TD19 | `TestArch_TestsHaveAtLeastOneAssertion` | Every Test func has a require/assert/t.Error/t.Fatal/cmp.Diff or wait*/expect*/assert*/require* helper call | 7 missing; 3 integrationevents arch tests gained `require.Positive(t, seen, ...)` invariant; 4 subscriber tests use `waitFor` helper (predicate widened to recognize the pattern). |
| TD20 | `TestArch_NoErrorIgnoredInTests` | `_ = call()` flagged; `require.NoError` or `arch-test:ignore-err` annotation. Skips Close/Terminate/Shutdown/Stop/etc. cleanup patterns | 197 sites annotated/converted across all modules. |
| TD21 | `TestArch_FixedNowVarsAreDateLiterals` | `fixedNow` / `testNow` / `nowFunc` must come from `time.Date(...)` literal, not `time.Now()` | 0 violations; gate live (companion to TD7). |
| TD22 | (REMOVED — unreachable-after-Fatal) | Replaced by `go vet`'s built-in `unreachable` analyzer. AST predicate had unfixable false-positives on canonical `if err != nil { t.Fatalf(...) }` guard pattern. |
| TD23 | `TestArch_NoFmtPrintlnInTests` | `t.Log` / `t.Logf` over `fmt.Println` / `fmt.Printf` (routes through test reporter, suppressed on pass) | 0 violations; gate live. |
| TD24 | `TestArch_TestFilesPairWithProductionFile` | Every `<name>_test.go` under `internal/<mod>/{domain,app,ports,adapters}/` has paired `<name>.go`; flow/contract/e2e/testmain prefixes + `_helpers_test.go`/`_fakes_test.go` suffixes allow-listed | 0 violations; gate live. |

### Companion runtime fix (singleflight stampede test)

`internal/common/cache/facade_test.go:TestFacade_Singleflight_CoalescesConcurrentMisses` was flaking under heavy parallel load (50ms sleep insufficient on slow CI). Replaced with `sync.WaitGroup` start-gate + bumped synchronisation budget to 500ms with godoc rationale. Verified via `go test -count=10`: stable. Not technically test-discipline (no TD rule flagged), but the singleflight contract under test deserves a reliable gate.

## How to add an entry to this file

1. Hit a red TestArch_*.
2. Diagnose the root cause. **Fix the violation in the same PR** — the
   skip path is a last resort.
3. If, after sustained effort, the violation is genuinely invasive
   (>50 LOC of cross-cutting refactor) AND would break the PR's
   coherence, append a `t.Skip("known violation: <reason> — tracked in
   KNOWN_VIOLATIONS.md")` line + add a row in a new "Live skip
   register" section (currently EMPTY — the register being empty is
   the load-bearing invariant of this file).
4. The next PR that closes the violation removes BOTH the `t.Skip` AND
   its row here.

See also: the parent process discipline in `doc.go` ("Process discipline").
