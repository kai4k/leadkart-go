# ADR 0062 — TDL Test Pyramid + Per-Aggregate FakeRepository Canon

**Status:** Accepted.
**Date:** 2026-05-25

## Fitness function

Three mechanical fitness functions in
`internal/architecture/tdl_canon_arch_test.go` enforce the canon
described below:

- `TestArch_EveryRepositoryHasFake` — every `domain/<X>/repository.go`
  declaring a `Repository` interface has a co-located
  `<aggregate>test/fake_repository.go`.
- `TestArch_FakeRepositoryHasCompileGate` — every
  `fake_repository.go` declares
  `var _ <aggregate>.Repository = (*FakeRepository)(nil)`.
- `TestArch_FakeRepositoryHasNoSync` — `<aggregate>test/` packages do
  not import the `sync` standard-library package.

The "fakes are faithful to SQL-adapter contract" claim is NOT
mechanically expressible — it's enforced at code-review time and by
the fakes carrying the same return-type signatures as the adapter
(typed `ErrXxx` errors translated from SQL state codes). PR review is
the gate.

## Context

The codebase had ~80+ integration tests against testcontainer Postgres,
many of which duplicated business-rule coverage already provided by
domain-level unit tests. Inline fake repositories were duplicated
across app/command test files, and several were unfaithful stubs
that returned `nil, nil` or `ErrNotFound` unconditionally instead of
mirroring the SQL adapter contract.

This produced three problems:

1. **Slow CI** — ~15min for the identity adapter shard alone, even
   after the shared-pgtest container refactor.
2. **Test discipline drift** — engineers added new business-rule
   assertions at the SQL layer (integration) rather than the domain
   layer (unit-against-fake) because the inline fakes were too
   broken to use for new tests.
3. **Hidden contract gaps** — the inline fakes diverged from the
   SQL adapters in 13+ documented places (see commit 145598a +
   11a65bc). Tests that passed against the inline fake might fail
   against real SQL, or vice versa.

The ThreeDotsLabs canon — referenced in CLAUDE.md as a Tier 1 source
("ThreeDotsLabs Wild Workouts (Nov 2025 canonical), 'Go with the
Domain', 'Go Event-Driven' training") — prescribes a different shape
that this ADR formalizes.

## Decision

### 1. Test pyramid by layer

| Layer | Tests | How | Target count per aggregate |
|---|---|---|---|
| **Domain** | Business rules — aggregate invariants, state machines, value-object validation | Pure unit tests against the aggregate type directly | Many (~10-30) |
| **App / command + query handlers** | Orchestration — handler calls repo, emits events, handles errors | Unit tests against `<aggregate>test.FakeRepository` | Many (~5-15) |
| **Adapter / SQL** | SQL contract — RLS policy fires, JSONB round-trips, unique-index 23505 translation, soft-delete partial-index behavior, outbox-row insertion | Integration tests against real Postgres via `pgtest.RunMain` | Few (~3-6) |

This is the TDL Wild Workouts shape. The load-bearing claim: SQL
adapters and domain logic are SEPARATE CONCERNS that test SEPARATELY.

### 2. Per-aggregate FakeRepository packages

Every domain aggregate that defines a `Repository` interface in its
`repository.go` file MUST ship a co-located `<aggregate>test/`
package containing a FaithfulFakeRepository.

Location: `internal/<module>/domain/<aggregate>/<aggregate>test/fake_repository.go`

Required contents:

```go
package <aggregate>test

type FakeRepository struct { /* in-memory maps + per-aggregate indexes */ }
func NewFakeRepository() *FakeRepository { ... }

// Compile-time interface conformance gate — drift in <aggregate>.Repository
// breaks at build time before any test runs.
var _ <aggregate>.Repository = (*FakeRepository)(nil)

// All Repository methods with FAITHFUL contract semantics:
//   - ErrNotFound on missing/soft-deleted IDs
//   - Typed errors that the SQL adapter returns (ErrAlreadyExists, etc.)
//   - Soft-delete filtering on reads if the aggregate supports it
//   - Stable sort order matching the SQL adapter ORDER BY
//   - Partial-unique-index semantics (e.g. duplicate-name only on LIVE rows)
```

Constraints:

- **NO sync primitives** (`sync.Mutex`, `sync.RWMutex`, etc.). The
  domain subtree is concurrency-free by canon (Bryan Mills GopherCon
  2018). Tests use the single-test-owner pattern: each test creates
  its own `FakeRepository` instance via `NewFakeRepository()`, so
  shared mutable state across goroutines doesn't exist.
- **Faithful to the SQL adapter contract**, not the path-of-least-
  resistance stub. If the SQL adapter translates SQLSTATE 23505 to
  `ErrAlreadyExists`, the fake MUST also return `ErrAlreadyExists`
  on the equivalent duplicate-insert scenario.
- **Compile-time interface conformance gate** is mandatory.
- **No imports of `internal/<module>/adapters/`** — fakes are pure
  domain-layer code; they implement the interface defined in the
  aggregate's `repository.go`, nothing more.

### 3. Integration tests are SQL-contract-only

Tests in `internal/<module>/adapters/*_repository_pg_test.go` MUST
test things that ARE SQL-specific:

- **SQLSTATE translations** — 23505 → typed `ErrXxxTaken`
- **RLS policy enforcement** — confirms the row-level-security gate
  actually fires; cross-tenant reads return empty
- **Partial-unique-index behavior** — soft-deleted homonyms free the
  name slot; live duplicates block
- **JSONB / typed-column round-trips** — Marshal/Unmarshal of
  permission lists, snapshots, value-object encodings survives the
  PG driver
- **Outbox-row writes** — ~~`repo.Add` writes both the aggregate row
  and its outbox row in the same transaction (per ADR 0008)~~
  **AMENDED 2026-05-29 (see Amendment 1 below):** outbox-row writes
  are NOT verified by querying the outbox table directly. They are
  verified end-to-end via the production forwarder + a Watermill
  subscriber that records published events (strict TDL canon).
- **EXPLAIN-under-RLS** — confirms the right index is chosen for
  the bounded-keyset pagination paths (per ADR 0038)
- **DB-trigger / function behavior** — e.g. role-hierarchy cycle
  detection, composite-FK cross-tenant rejection

Tests in adapter files MUST NOT test:

- Pure round-trip Add/GetByID (covered by `<aggregate>test.FakeRepository`)
- ErrNotFound contract on missing IDs (covered by fake)
- Domain-logic state machine transitions (covered by domain unit tests)
- Business-rule rejections that the aggregate's own `New()` /
  mutator methods enforce (covered by domain unit tests)

### 4. Architectural fitness functions

Three new arch tests (`internal/architecture/tdl_canon_arch_test.go`)
enforce the shape mechanically:

- `TestArch_EveryRepositoryHasFake` — every `domain/<X>/repository.go`
  declaring a `Repository` interface has a co-located
  `<aggregate>test/fake_repository.go`. New aggregates can't ship
  without their fake.
- `TestArch_FakeRepositoryHasCompileGate` — every
  `fake_repository.go` contains
  `var _ <aggregate>.Repository = (*FakeRepository)(nil)`.
- `TestArch_FakeRepositoryHasNoSync` — `<aggregate>test/` packages
  don't import `sync`. The single-test-owner pattern is mechanical.

### 5. Why fakes, not mocks (TDL "Go with the Domain" ch. 8)

Mocks couple the test to the call-pattern of the SUT (Subject Under
Test):

```go
// Mock-style — brittle
mock.On("Add", mock.Anything, mock.MatchedBy(predicate)).Return(nil)
mock.AssertCalled(t, "Add")
```

Fakes couple to the contract:

```go
// Fake-style — durable
repo := <aggregate>test.NewFakeRepository()
handler := command.NewHandler(repo, ...)
require.NoError(t, handler.Handle(ctx, cmd))
got, err := repo.GetByID(ctx, expectedID)  // assert on state
require.NoError(t, err)
require.Equal(t, expectedName, got.Name())
```

Refactoring the SUT (e.g. handler now calls `GetByID` before `Add`
instead of just `Add`) breaks mock-tests but leaves fake-tests
green. The fake remains valid as long as the Repository INTERFACE
is unchanged.

## Consequences

### Positive

- **CI wall-time reduction** — pruning integration tests to SQL-
  contract-only cuts the identity adapter shard by an estimated
  60-70% on top of the shared-pgtest savings.
- **Test pyramid restored** — business logic tested in <10ms unit
  tests instead of ~500ms-1s integration tests, even after the
  shared-container optimization.
- **Mechanical drift prevention** — three fitness functions block
  PRs that introduce aggregates without fakes, fakes without
  compile gates, or fakes with hidden concurrency primitives.
- **Faithful fakes uncover real bugs** — the migration from inline
  fakes to canonical ones surfaced 13+ stub-shaped divergences from
  the SQL adapters (documented in commit 145598a + 11a65bc).
  Fixing these closed real test gaps.

### Negative

- **Initial migration cost** — extracting inline fakes + auditing
  for faithfulness was ~6 hours of focused work across 17 aggregates.
- **Two implementations of every Repository** — the fake and the
  SQL adapter both implement the interface. When the interface
  evolves, both must be updated. The compile-time conformance gate
  makes the fake's update mechanical; the integration tests catch
  the SQL adapter's update.

### Mitigations

- The compile-time gate (`var _ Repository = (*FakeRepository)(nil)`)
  makes drift impossible to land without breaking the build.
- The `TestArch_EveryRepositoryHasFake` gate prevents new aggregates
  from skipping the fake.
- New integration tests in `*_repository_pg_test.go` are reviewed
  against the SQL-contract-only criteria above; tests that overlap
  with unit-against-fake coverage are rejected (review-time
  discipline; can be hardened to a fitness function if drift
  recurs).

## References

- ThreeDotsLabs "Go with the Domain" chapters 6-8
- ThreeDotsLabs Wild Workouts repository — per-aggregate
  `<aggregate>test/` co-location pattern
- Dave Cheney "Practical Go: Real-World Advice For Writing
  Maintainable Programs" §9 — accept interfaces, return structs
- Khorikov "Unit Testing Principles, Practices, and Patterns" §5 —
  state-based vs interaction-based testing
- Bryan Mills "Rethinking Concurrency Patterns" GopherCon 2018 —
  domain layer is concurrency-free
- ADR 0019 — Testing canon (stdlib + go-cmp + testify/require +
  testcontainers-go); this ADR refines the test-pyramid shape
- ADR 0047 — Layer-boundary discipline; reinforced by the
  fake-imports-no-adapters rule
- Commits 1f5abfc + 145598a + 11a65bc — implementation of the
  17-aggregate migration

## Amendment 1 — 2026-05-29: Outbox tests are subscriber-based, not state-based

### Why

The original §3 listed "outbox-row writes" as a SQL-contract concern
verified by integration tests reading the outbox table directly. In
practice this drove a `common/messaging/messagingtest/outboxtest.go`
helper that used `fmt.Sprintf` to inject schema names into raw SQL —
violating both the sqlc canon (ADR 0004) and the `internal/common/`
scope (TDL puts adapters in their owning module, not in a generic
shared package). The flake of
`TestTenantRepository_UpdateByID_PersistsActivatedOutboxEventInSameTx`
on PR #47 surfaced this — the helper's missing `id` tiebreaker
masked a production-readable consumer-side ordering bug.

The strict TDL canon (ThreeDotsLabs Wild Workouts, "Repository
pattern in Go" + "Database integration testing in Go") prescribes
testing event-emission behavior through the SUBSCRIBER side: real
forwarder, real Watermill pubsub, real subscriber records what
arrives. State-based reads of the outbox table are a leaky abstraction
that ties tests to internal table shape.

### What

For every aggregate that emits integration events via outbox:

1. The integration test wires a Watermill GoChannel +
   `adapters.NewOutboxForwarder` + a recording subscriber on the
   module's events topic.
2. The test runs the production code (`repo.Add` / `repo.UpdateByID`).
3. The test calls `forwarder.ForwardOnce(ctx)` to drain pending
   outbox rows.
4. The test asserts on what the subscriber recorded — events
   present, ordered, with the expected tenant_id metadata.

Tests MUST NOT directly query `<schema>.outbox` to assert event
emission. The forwarder + Watermill envelope are the canonical
observation surface — same shape production consumers see.

### Removed

- `internal/common/messaging/messagingtest/outboxtest.go` deleted.
- The arch-test allowlist for `messagingtest` raw SQL removed.
- New arch test `TestArch_NoDirectOutboxStateAssertions` forbids any
  `*_test.go` from selecting from `*.outbox` (heuristic: SQL
  containing both `FROM` + `outbox` outside `adapters/sql/` +
  `outbox_forwarder_test.go` — the forwarder itself legitimately
  inspects rows it's about to publish).

### Migration shape (per-module fixture)

Each module's `adapters/` test directory owns a small `outboxSubscriberFixture`
that combines GoChannel + drainSubscriber + OutboxForwarder.
References:
- `internal/identity/adapters/outbox_forwarder_test.go` —
  pre-existing `drainSubscriber` + `waitForCount` pattern that this
  amendment generalizes per-module.

### Scope of this override

This amendment supersedes §3's "outbox-row writes" bullet ONLY. The
other SQL-contract concerns (SQLSTATE translation, RLS enforcement,
partial-unique indexes, JSONB round-trips, EXPLAIN-under-RLS,
DB-trigger behavior) remain in the integration-test layer
unchanged — they're genuinely SQL-specific properties that no
subscriber-based test can verify.
