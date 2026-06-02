# ADR 0066 — Go 1.24–1.26 canon sweep, shared `pgconv`, and arch-gate upgrades

**Status:** Accepted
**Date:** 2026-05-30

## Context

Two gaps surfaced in a Go-canon + arch-test review.

1. **Pre-1.26 idioms + duplicated adapter plumbing.** The codebase targets Go 1.26.3 but still carried the throwaway-local `x := val; return &x` shape (superseded by `new(expr)`), a stray `omitempty` on a `*time.Time` field (Go 1.24 wants `omitzero` for time types), and legacy `sort.Slice`/`sort.Strings` in tests (Go 1.21 `slices`). More importantly, every bounded context kept its own copy of the Go↔pgtype conversion helpers (`pgUUID`, `pgTimestamp`, `pgRequiredTimestamp`, `pgDate`, `pgUUIDOpt`/`pgUUIDOrNull`, `uuidFromPg`, `timeFromPg`) plus three differently-named copies of the same zero→nil string converter (`stringPtr`, `stringPtrFromValue`, `nilIfEmpty`). Identical logic, four-way duplication, drift only by name — a single-approach violation.

2. **Arch tests were direct-import only.** Measured against the canonical "5 architecture tests" set (layer-dependency / naming / colocation / visibility / dependency-guard), our gates covered four buckets well but had three real holes: dependency guards matched **direct** imports only (a transitive `app → helper → pgx` leak escaped — the ".NET transitive NuGet leak" class), there was no **depguard** linter (the Go-canon, declarative layer guard), and no **visibility** gate (Go's "internal by default" = unexported-unless-cross-package).

TDL canon settled the nullable-representation question: the `*T`-vs-`pgtype.Text` choice never crosses the adapter boundary, so canon is indifferent to the type and demands only one rule + one source + explicit mapping (`docs/doctrine/tdl_canon.md`). `emit_pointers_for_null_types: true` is already set on all four sqlc targets, so `*T` for primitive nullables is the tooling-native choice — flipping it and regenerating would buy a purity canon never asked for.

## Decision

1. **Adopt the modern idioms** repo-wide: `new(true)`/`new(false)` at the unconditional pointer sites; `omitzero` on time-typed JSON fields; `for i := range n`; `strings.SplitSeq`; `slices.SortFunc`/`slices.Sort` (no `sort.Slice`/`sort.Strings` anywhere).

2. **Single shared `internal/common/pgconv`** owns every Go↔pgtype converter (`PgUUID`, `PgUUIDOrNull`, `PgTimestamp`, `PgRequiredTimestamp`, `PgDate`, `UUIDFromPg`, `TimeFromPg`, `TimeFromPgDate`, and the generic `ZeroToNil[T comparable]`). All four modules' adapters import it; the per-module `conversion.go` copies are deleted. Domain-specific transformers (`uuidParamOpt` parsing a wire string, `nameQueryPattern` adding `%…%`, `nullableTextArray`/`nilIfEmptySlice` for `@>`) legitimately stay in their adapter — they are not pure conversion. `ZeroToNil` stays a conditional helper, *not* `new(expr)`: `new` always yields a non-nil pointer, and the whole point is nil on the zero value.

3. **Three new gate capabilities** close the .NET-parity holes: a transitive dependency guard (`go list` closure), a `depguard` linter config (driver-ban for `domain/`+`app/`), and a no-gratuitous-exports visibility gate. Plus gates that lock in decisions 1–2 so they cannot regress.

## Consequences

- ~470 adapter call sites now qualify `pgconv.*`; the conversion logic has one home and one test.
- `app/` keeps reaching `pgx` transitively via `internal/common/pg` (the `pg.UnitOfWork` interface bridges the driver) — that is legitimate, so the transitive gate bans only `adapters/db` + concrete `adapters` for `app/`, and bans `pgx` for `domain/`.
- depguard adds fast per-lint feedback for the direct driver-ban; it complements (does not replace) the AST import gates.
- The visibility gate's textual usage scan errs toward not-flagging (false negatives, never false positives); a genuinely-external-by-value export can opt out with `// arch-test:exported-for-wiring`.

## Fitness function

`TestArch_NoLegacySortPackage`, `TestArch_NoAddressOfThrowawayLocal`, `TestArch_TimeFieldsUseOmitzero`, `TestArch_NoModuleLocalPgConversionHelpers`, `TestArch_NoInlinePgtypeConstruction` (bans inline `pgtype.{UUID,Timestamptz,Date}{...}` in adapters — the bypass the helper gate can't see), `TestArch_LayersHaveNoForbiddenTransitiveDeps`, `TestArch_NoGratuitousExports`, and the depguard assertion in `TestArch_GolangciLintConfigCanonical` (all in `internal/architecture/`).
