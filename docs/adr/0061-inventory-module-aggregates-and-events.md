# ADR 0061 — Inventory module aggregates and events (Slice 1)

**Status:** Accepted.
**Date:** 2026-05-23

## Context

BRD §6.5 names the Inventory bounded context as the owner of Products, Batches, and StockMovements. Slice 1 is the catalog + batch stock-on-hand pipeline — Reservation linkage to the future Orders module and low-stock alerts to Notifications are deferred.

Inventory is the first module after Identity to ship its own schema + aggregates + outbox. The shape it picks sets precedent for CRM / Orders / Dispatch (Phases 2–4). This ADR records the choices.

Five questions had to be answered:

1. Should StockMovement be an entity inside Batch, or its own aggregate?
2. Should Product own Batches by composition, or reference by ID?
3. How is high-contention concurrency on Batch handled (Inbound + Outbound + Adjustment racing the same row from Order fulfilment + PurchaseOrder receipt)?
4. How is money represented?
5. How is per-tenant SKU uniqueness enforced when soft-delete is allowed?

## Decision

### 1. Three aggregate roots: `Product`, `Batch`, `StockMovement`

Per Vernon IDDD ch.7 ("Aggregates") + ch.10 ("Repositories") — aggregate boundaries are drawn around invariants, not data graphs. The three Inventory aggregates have distinct invariant scopes:

- **Product** — catalogue master. Owns SKU uniqueness (per tenant on live rows), GST rate, classification (DosageForm / HSN / PackSize). Read-mostly: product master changes infrequently; orders + reports read it constantly.
- **Batch** — per-product manufacturing batch. Owns `quantity_on_hand` + the optimistic concurrency token + expiry/recall lifecycle. WRITE-heavy: every Inbound / Outbound / Adjustment mutates it.
- **StockMovement** — append-only ledger row. Owns the immutability invariant (rows are never updated post-write — forensics + GST audit + Schedule-X drug-trail demand this).

Loading Product just to mutate one Batch's quantity would lock a much larger graph than needed; loading Batch to scan its movement history would be O(N) on every read. Each aggregate has its own repository + own UoW boundary.

Batch references Product by `ProductID` (composite-FK at the DB; not by struct embedding). StockMovement references Batch by `BatchID` + denormalises ProductID for ledger reads.

### 2. StockMovement is a separate aggregate, not an entity inside Batch

Three reasons:

- **Lifecycle differs.** Batch mutates `quantity_on_hand` repeatedly + can be soft-deleted; StockMovement rows are written-once and never touched again. Combining them into one aggregate would put append-only + mutable state under the same UoW, defeating the audit-immutability invariant.
- **Read patterns differ.** Batch is read by SKU pickers + reorder dashboards (one or few rows); StockMovement ledger views read 100s of rows ordered by `(occurred_at, id)`. Sharing a load path would cost on every Batch hydration.
- **Vernon IDDD ch.7** — when a relationship/ledger has its own audit lifecycle, it belongs in its own aggregate (the same template ADR 0058 used for `rolehierarchy.Edge`).

The application-tier handler (`LogStockMovement`) writes both same-tx: bump Batch + insert StockMovement. The single-tx atomicity is the handler's responsibility per TDL strict canon; no saga.

### 3. Optimistic concurrency via explicit `version int64` column

Spec required an "xmin pattern" — clarified to the canonical portable variant: an explicit `version bigint NOT NULL DEFAULT 0` on `inventory.batches`, incremented on every UPDATE, with `WHERE version = $current` in the predicate. The adapter treats rows-affected = 0 as `batch.ErrConcurrencyConflict`; the `LogStockMovement` handler runs a small retry loop (capped at 3 attempts).

Why explicit version over Postgres `xmin`:

- **Portability** — `xmin` is Postgres-internal; explicit version travels to any RDBMS.
- **Vacuum-safe** — `xmin` is recycled at vacuum cycles; an explicit column never is.
- **EF Core / Wolverine / TDL canon** — every reference implementation we ported from uses an explicit token.
- **Audit-friendly** — the version number is visible in the row, on the BatchDto, and in integration events. Operations can see "this batch is at v47" + reason about contention rate.

Vernon IDDD ch.10 endorses this as the canonical optimistic-concurrency primitive for write-heavy aggregates.

Retry strategy: re-read + re-apply the movement on conflict. The Batch's invariants (insufficient stock, expired) re-evaluate against the fresh state — a movement that was valid against stale data may now reject with the more-specific error. 3 attempts handles realistic burst contention; pathological hot-row scenarios signal "you need pessimistic locking or a queue" which lives outside Slice 1.

### 4. Money is `int64 paise` per Stripe canon

Every monetary column (`mrp_paise`, `purchase_price_paise`) is `bigint`. Every domain type is `int64`. Every wire type is `integer, format: int64`. NEVER float. NEVER `decimal`/`numeric` at the wire boundary (DB column choice is a separate question — int64 there too because the math is exact-integer at the application boundary; representational fidelity is preserved).

Rationale: every fintech that ships money math at scale (Stripe, Plaid, PayPal, Square) uses integer minor units. Float introduces rounding errors that accumulate over reconciliation reports; decimal types make every multiplication a separate audit. With paise, every arithmetic operation is bounded, reversible, and total-order-equal across the stack.

Wire field naming: `*_paise` suffix so the unit is self-evident. Future multi-currency expansion gets a peer `currency` field + conversion lives in the application layer (never at the DB).

### 5. Per-tenant SKU uniqueness via partial unique index

`inventory.products` has a partial unique index:

```sql
CREATE UNIQUE INDEX uq_products_tenant_sku_live
    ON inventory.products (tenant_id, sku) WHERE NOT is_deleted;
```

Live rows enforce uniqueness; soft-deleted rows can collide so admins can recreate after forensic cleanup. The aggregate's `New` canonicalises SKU to upper-case + trimmed before insert so case + whitespace variants share the same uniqueness slot.

The repository surfaces SQLSTATE 23505 with `ConstraintName == "uq_products_tenant_sku_live"` as `product.ErrSKUTaken`. Other unique violations bubble up as wrapped errors — they signal a different invariant breach and shouldn't masquerade as ErrSKUTaken.

This mirrors the partial-unique-index pattern from `uq_roles_tenant_name` (ADR 0036) and `uq_memberships_person_active` (multi-tenancy.md "Identity model").

### 6. Permissions

Four entries added to `permission.IdentityPermissions`:

- `inventory.catalog.read` — gates Product reads.
- `inventory.catalog.manage` — gates Product create/update/delete.
- `inventory.stock.read` — gates Batch + StockMovement reads.
- `inventory.stock.manage` — gates Batch create + StockMovement insert.

The catalog stays a single closed set across modules per ADR 0036 + ADR 0051 carve-out (single-module type placement) — the resolver + middleware see them as one enum. The DefaultRoleCatalog seed is NOT touched in Slice 1; per-role grants land in a follow-up commit when tenant onboarding learns to apply Inventory permissions to sales/purchase roles. CompanyOwner already satisfies all gates via `Meta.TenantAdmin` + the SuperUser short-circuit.

### 7. Integration events

Five wire-stable V1 events, all `TenantScoped` per Identity's pattern:

- `inventory.product_created.v1`
- `inventory.product_updated.v1` — carries `changed_fields[]` for diff-aware subscribers.
- `inventory.product_deactivated.v1`
- `inventory.batch_added.v1`
- `inventory.stock_movement_logged.v1` — carries `quantity` (signed) + `new_quantity_on_hand` so downstream can reconcile without re-query.

Topic = `inventory.events` (single-topic-per-module per Watermill canon). Aggregate-event drain → `integrationevents.FromDomainEvent` mapper → `writeOutboxEvents` to `inventory.outbox` same-tx as the state change. The `act_*` columns from migration 20260524000001 flow through the same `actclaim.FromContext` mechanism per ADR 0056.

Arch tests mirror identity's:
- Every event must satisfy `TenantScoped`.
- Every `*V{N}` type must be registered in the catalogue.
- Topic alias must match `^inventory\.[a-z][a-z0-9_]*\.v\d+$`.
- No framework imports (Watermill, pgx, jwt).

### 8. Boundary discipline arch test

`internal/inventory/app/` ships its own `TestArch_AppDoesNotImportForbidden` (per ADR 0047) banning imports of `internal/inventory/adapters/db`, `internal/inventory/adapters`, `jackc/pgx/v5`, `pgxpool`, `pgtype`. Drift becomes impossible at PR time.

### 9. Route registration arch test

`internal/inventory/ports/route_registration_test.go` mirrors identity's per ADR 0049 — wires `AddRoutes` against a fresh ServeMux and panics-as-failure on any Go 1.22 pattern overlap. Catches conflicts at unit-test time, not Docker smoke time.

## Consequences

**Positive:**

- Three small, focused aggregates instead of one giant Product-and-batches-and-movements graph.
- Optimistic concurrency works without pessimistic locks — high-contention writes scale horizontally.
- Money math stays exact-integer end-to-end. No reconciliation drift.
- SKU uniqueness invariant lives at the DB; the adapter surfaces it as a typed sentinel.
- Per-aggregate repositories + per-aggregate outbox events keep blast radius narrow.

**Trade-offs:**

- LogStockMovement is a multi-aggregate handler — it owns the retry loop + the same-tx coordination. Slightly more handler complexity than a single-aggregate update.
- Adjustment up vs down currently rides Quantity's sign — slice 2 will add a `Direction` enum to disambiguate at the wire level without overloading the sign.
- StockMovement rows are append-only at the DB (no UPDATE/DELETE policies) but the parent Batch can soft-delete — soft-deleted batches keep their ledger history for audit. Operators querying "live ledger" must JOIN on `inventory.batches.is_deleted = false`.

**Deferred:**

- Reservation linkage to the Orders module (Orders doesn't exist yet).
- Multi-location warehouses (single-tenant single-warehouse assumption baked in).
- Lot tracking beyond batch (per-pack-serial isn't on the slice 1 roadmap).
- Low-stock alerts (Notifications module owns the trigger).
- CSV bulk product upload (operator UX concern; ships when product asks for it).

## Amendment 1 (2026-05-24) — Slice 1 fix-pass closures

Round-1 review of the slice-1 PR surfaced findings that the original draft
had under-specified. Closures landed in the fix-pass; documenting them
here so a future reader sees the seal.

**Closed in fix-pass:**

- **Per-module outbox forwarder.** The original wiring reused the identity
  forwarder against `inventory.outbox`; identity's `ForwardOnce` hardcodes
  `identity.outbox` via its `db.Queries`, silently orphaning every inventory
  event. Fix: shipped `internal/inventory/adapters/OutboxForwarder` (mirror
  of identity's, bound to inventory's sqlc Queries) + wired in
  `cmd/worker/main.go` as a second forwarder goroutine.

- **DefaultRoleCatalog inventory grants.** Slice 1 added the four
  permission strings to the catalogue but `DefaultRoleCatalog` granted
  none to any tenant-tier role except CompanyOwner (who short-circuits
  via `Meta.TenantAdmin`). Every non-Owner membership on a fresh tenant
  received 403 on all 11 new endpoints. Fix: extended the catalog so
  PurchaseManager + SalesManager + OfficeAdministrator carry the
  Inventory grants their role implies (purchase chain → manage;
  sales chain → read-stock; admins → both catalog read + stock read).
  The Slice 1 deferral note above is superseded — wiring shipped.

- **Event-name semantic split.** `ProductDeactivatedV1` was emitted only
  on SoftDelete (which is "soft-deleted", not "deactivated"). Fix: new
  `ProductSoftDeletedV1` event maps from `product.SoftDeleted` domain
  event; `ProductDeactivatedV1` retained and now mapped from a new
  `product.DeactivatedEvent` emitted by `Product.Deactivate()` when
  `is_active` transitions false (BRD §6.5 distinction between
  deactivation — temporary — and soft-delete — terminal).

- **AddBatch UoW + re-check.** Original handler called `products.GetByID`
  then `batches.Add` as two separate txs, leaving a window where the
  parent product could be soft-deleted between read and write. Fix:
  handler now takes `pg.UnitOfWork`, wraps both calls in `WithinTx`,
  and re-reads the product inside the tx (under `FOR SHARE` lock via
  the standard load path) before the batch insert.

- **Optimistic-concurrency CONTENTION test.** The slice-1 test was renamed
  to "SequentialUpdates" by the original author with a self-admitted
  deferral to slice 2. ADR 0061 §3 declares contention testing in
  slice-1 scope; fix shipped a goroutine-racing contention test that
  asserts (a) eventual success of every racer, (b) retry path exercised
  (rows-affected = 0 surfaced for at least one attempt), (c) final
  `quantity_on_hand` = sum of inbounds — proving the retry loop's
  re-read-and-re-apply branch under real contention.

**Deferred-with-rationale:**

- **`actclaim` lift to `internal/common/`.** Slice 1 duplicated the
  ~40 LOC actclaim package into `internal/inventory/app/actclaim/`
  rather than importing identity's. Per CLAUDE.md "Architecture rule 1:
  modules NEVER reference each other's domain/app/ports/adapters" the
  cross-module import was a boundary violation. The duplicate stays
  in place per Fowler "rule of three" (Refactoring ch.12 §"Three strikes
  and you refactor") — early duplication beats premature abstraction.
  When a THIRD bounded context (CRM is next) needs act-claim
  propagation, lift to `internal/common/actclaim/` and switch all three
  to the shared package. Tracking issue: lift on Phase 2 CRM slice.

- **Domain cross-aggregate coupling (`batch` ⇒ `product`,
  `stockmovement` ⇒ both).** Acceptable per ADR 0061 §3 — batch and
  movement carry `product.ID` for type safety + integration-event
  payload mapping. Compounds future-refactor pain only when a batch or
  movement needs to leave the inventory bounded context; for slice 1
  the type-safety value outweighs the cost. Rule-of-three not hit.

- **Composite-index direction (`idx_batches_product`).** Index was
  declared `(product_id, expiry_date ASC, id DESC)` while the query
  orders `(expiry_date DESC, id DESC)`. Postgres can backward-scan
  ASC indexes for DESC ORDER BY at full cost (B-tree is symmetrically
  walkable) so this is a documentation drift, not a perf regression.
  Fix shipped: corrected the index direction to match the query +
  added an EXPLAIN-under-RLS integration test to catch future drift
  (mirror of identity's `keyset_explain_integration_test.go`).

## Amendment 2 (2026-05-24) — Pessimistic locking on Batch (supersedes §3 retry strategy)

The original §3 chose optimistic concurrency for Batch + a 3-attempt
retry loop in `LogStockMovementHandler`. Slice-1 cloud CI exposed the
flaw: the retry-path integration test asserted "retry must fire under
concurrent racers" but the assertion is fundamentally non-deterministic
— on CPU-constrained runners (GOMAXPROCS ≤ 2) Go's scheduler serialises
the racers so no version collisions occur, no retry fires, the test
fails. The test was reflecting a real architectural mismatch: optimistic
concurrency is the wrong primitive for a HOT-ROW COUNTER like
`quantity_on_hand` where contention is the access pattern, not the edge
case.

**Decision:** `BatchRepository.UpdateByID` now acquires a pessimistic
row-level lock (`SELECT ... FOR UPDATE`) before loading the aggregate.
Concurrent transactions block at the lock until the holding tx commits
or rolls back; the DB serializes them by construction. The application
handler has NO retry loop.

**What's retained:** the `version` column + `WHERE version = $expected`
predicate stay in place as defense-in-depth audit signals — if the
check ever fails post-lock, it indicates a programmer error (external
SQL bypass, lock leak) rather than a real race; we still surface
`ErrConcurrencyConflict` so the failure is visible rather than silent.

**Canon:**
- Postgres docs §13.3.2 "Explicit Locking" — row-level locks are the
  canonical primitive for "concurrent access to a single row".
- Stripe payment-ledger pattern — balance updates acquire row locks;
  no optimistic-retry on the hot path.
- DDIA Ch.7 "Transactions" — pessimistic locks beat optimistic retries
  for high-contention workloads where retries thrash without forward
  progress.
- Khorikov, *Pragmatic Clean Architecture* §11 — pessimistic for
  high-contention writes; optimistic for collaborative reads.

**Trade-offs accepted:**
- Writers hold the lock for the duration of the surrounding UoW tx
  (sub-millisecond on the `LogStockMovement` hot path). A crashed
  writer releases the lock on session disconnect — no operator action.
- If a future cross-aggregate UoW holds the batch lock + then waits
  on an external resource, lock duration grows. Mitigation: keep
  cross-aggregate UoW txs short + audited.

**Scope:** This decision applies to `Batch` ONLY. `Product` retains
optimistic concurrency (low-contention catalog rows). Future hot-row
aggregates (Order line items, Lead credit balance) should also lock;
ADR 0059's `LeadCredit` already uses the same pattern (UPSERT under
RLS — different shape, same intent).

**Tests:** the integration test
(`log_stock_movement_contention_integration_test.go`) now asserts:
(a) every racer succeeds, (b) total UoW entries == racers (no retries
possible by construction; assertion catches future regressions),
(c) final `quantity_on_hand` == sum of inbound magnitudes (zero lost
updates). 8 racers / no scheduling assumptions / deterministic.

## References

- BRD §6.5 — Inventory Module spec.
- Vernon, *Implementing Domain-Driven Design* — ch.7 (Aggregates), ch.10 (Repositories).
- Khorikov, *Pragmatic Clean Architecture* §11 — relationships with their own lifecycle deserve their own aggregate.
- ADR 0008 — Watermill outbox + per-module outbox tables.
- ADR 0036 — Permission model (closed catalogue).
- ADR 0038 — Cursor (keyset) pagination.
- ADR 0040 — Search via pg_trgm.
- ADR 0046 — OpenAPI spec-first.
- ADR 0047 — Layer boundary discipline.
- ADR 0049 — URL design rules + route-arch gate.
- ADR 0050 — OpenAPI as code-of-record + spec/code drift CI gates.
- ADR 0056 — Impersonation context propagation (act_* columns).
- ADR 0058 — Role hierarchy as join-table aggregate (precedent for ledger-as-its-own-aggregate).
- Stripe API docs — money as integer minor units canon (`amount: int64` in cents/paise/etc).


## Fitness function

`TestArch_AggregatesHaveFactoryAndUnmarshal + TestArch_RepositoriesHaveUpdateByIDFn` (in `internal/architecture/`).

Inventory aggregates (batch, product, stockmovement) satisfy the factory pattern; stockmovement append-only exception is documented in the UpdateByID test.
