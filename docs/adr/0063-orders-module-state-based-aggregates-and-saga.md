# ADR 0063 — Orders module: state-based aggregates, Postgres-sequence invoice numbering, outbox-correlation fulfillment saga

**Status:** Accepted
**Date:** 2026-05-26
**Related:** ADR 0003 (state-based persistence default), ADR 0035 (event-sourcing deferred), ADR 0008 (Watermill outbox), ADR 0027 (outbox-as-audit), ADR 0047 (layer-boundary discipline), ADR 0062 (TDL test pyramid + per-aggregate fake canon).
**Supersedes for Go-port purposes:** the .NET BRD §6.4 Marten event-sourcing prescription for Orders + §A-010 Wolverine `Saga<T>` prescription. Both are state-based here.

## Context

BRD §6.4 names Orders as the most state-rich module: a 10-step happy-path lifecycle from quotation through delivery, plus a cancellation branch with cross-module compensating actions (unreserve stock, cancel invoice, issue credit note, cancel consignment). The .NET implementation uses **Marten event sourcing** on the Order stream + Wolverine `Saga<T>` for fulfillment coordination.

Per ADR 0035 the Go port **does not** event-source at v0.1 — there is no Marten-equivalent for Go, and building per-aggregate ES (event store, projections, upcasters, snapshots) costs more than the audit/replay benefits when state-based + outbox-as-audit already covers the regulatory + reporting needs.

Per ADR 0008 + 0027 the outbox is the audit ledger. Every state transition emits an integration event that gets persisted in the per-module `orders.outbox` table inside the same tx as the state change, then forwarded to subscribers. Reconstructing the historical sequence of state changes for a given order is a `SELECT * FROM orders.outbox WHERE tenant_id = $1 AND payload->>'order_id' = $2 ORDER BY id` away.

This ADR resolves five questions specific to Orders that ADR 0035 left open:

1. How are quotations modelled — child entities of Order or a separate aggregate?
2. How is the order state machine shaped, given that the .NET stream had 11 events?
3. How is gapless invoice numbering enforced under concurrency without an event stream?
4. How is the order-fulfillment saga (cancellation compensation, stock-reservation handshake) coordinated without `Saga<T>`?
5. How is the Marten "inline projection" pattern (e.g. `OrderSummary`) reproduced for read models?

## Decision

### 1. Five aggregate roots: `Quotation`, `Order`, `Invoice`, `CreditNote`, `Payment`

Per Vernon IDDD ch.7 — aggregate boundaries follow invariant scope, not data graphs. The five Orders aggregates have distinct invariant scopes + lifecycles:

| Aggregate | Owns | Lifecycle |
|---|---|---|
| `Quotation` | Items[], unit prices, line totals, customer lead reference, revision chain | Many revisions over hours/days; one terminal: `Approved` or `Rejected`. Once approved, copied into Order. |
| `Order` | State machine (10 steps), references to approved Quotation + Invoice + ConsignmentNote, cancellation reason | Days to weeks. Terminal: `Complete` or `Cancelled`. |
| `Invoice` | InvoiceNumber (gapless), TaxDetails, IssueDate, references Order | Append-only — once issued, never updated. Cancellation produces a CreditNote rather than mutating the Invoice. |
| `CreditNote` | CreditNoteNumber (gapless), Reason, references Invoice | Append-only — once issued, immutable. |
| `Payment` | Method, ReferenceNumber, AmountPaisa, ReceivedAt, kind (Token \| Full \| Refund) | Append-only — every payment receipt is a new row. Order's `paid_amount` is a derived sum. |

Why five over one fat Order:

- **Lock contention.** Collapsing Invoice + Payment into Order means a customer paying their balance racks the same row-lock as warehouse marking the order packed. Splitting lets the financial side scale independently of fulfillment.
- **Immutability invariants.** Invoice + CreditNote + Payment are append-only (GST audit + Schedule-X drug-trail demand it). Keeping them in their own aggregates makes "never updated" a type-level property — there is no mutator method, just `New` + `UnmarshalFromDB`.
- **Marten parity at the read side.** The .NET parent reads `OrderSummary` (a projection that joins all five concepts). Go replicates this via a read-only sqlc query that joins the five tables — same shape, different storage.

Quotation references the source `crm.crm_leads` row by `CustomerLeadID` (composite-FK at the DB). Order references Quotation by `ApprovedQuotationID`. Invoice/CreditNote/Payment reference Order by `OrderID`. All tenant-scoped.

### 2. Order state machine — ten states, explicit columns, integration-event emission per transition

Per BRD §6.4 the happy path is:

```
QuotationDraft → QuotationApproved → TokenPaid → Confirmed →
Packed → Invoiced → Dispatched → Delivered → Complete
```

Plus the cancellation branch (entered from any state except `Complete`):

```
… → Cancelled
```

Materialised as a strict state machine on `orders.orders.state`:

| From | Valid transitions | Side-effects |
|---|---|---|
| `quotation_draft` | `quotation_approved`, `cancelled` | Approving copies items snapshot into Order; Cancel from this state is "abandon quote" |
| `quotation_approved` | `token_paid`, `cancelled` | TokenPayment aggregate Add must succeed first |
| `token_paid` | `confirmed`, `cancelled` | Confirm publishes `OrderConfirmedV1` → Inventory subscriber reserves stock |
| `confirmed` | `packed`, `cancelled` | Packing is a manual transition by warehouse staff |
| `packed` | `invoiced`, `cancelled` | Invoice aggregate Add must succeed first (gapless number assigned) |
| `invoiced` | `dispatched`, `cancelled` | Cancel from `invoiced` ALSO mints a CreditNote (financial reversal); see §4 |
| `dispatched` | `delivered`, `cancelled` | Dispatched implies a ConsignmentNote exists |
| `delivered` | `complete` | Complete requires full payment received |
| `complete` | — (terminal) | No transitions out |
| `cancelled` | — (terminal) | No transitions out; compensation events drive cleanup |

Invalid transitions return `order.ErrInvalidTransition`. Self-transitions are no-ops (return nil, emit no event).

The state column is `text NOT NULL` with a CHECK constraint enumerating the values — same shape as `crmlead.stage`. Every transition emits a typed domain event (`OrderConfirmedEvent`, `OrderPackedEvent`, …); the adapter drains them to the outbox in the same tx; the forwarder publishes them as integration events (`orders.order_confirmed.v1`, `orders.order_packed.v1`, …).

Why this beats event sourcing for the audit need: the outbox row IS the audit entry. Reconstructing "what happened to this order in what order" is a single SQL query on `orders.outbox WHERE payload->>'order_id' = $1 ORDER BY id`. There is no projection-rebuild story to maintain, no snapshot-tuning, no upcaster pipeline. The downside (no "as-of-date" temporal querying) is acceptable per ADR 0035 decision criteria.

### 3. Gapless invoice numbering via Postgres `BIGSERIAL` per (tenant_id, financial_year, kind)

BRD §A-014 + GSTR-1 require invoice / credit-note / cancellation-note numbers to be sequential + gapless within a financial year. Three immutable series:

- `INV/YYYY-YY/{seq}` — Invoice
- `CDN/YYYY-YY/{seq}` — Credit Note (invoice reversed after delivery)
- `CN/YYYY-YY/{seq}` — Cancellation Note (invoice cancelled before delivery)

v0.2 ships **per-tenant per-FY per-kind sequences** (NOT global; BRD §A-014 deferred per-tenant to "Phase 2" but Go starts there because adding per-tenant scope later is a destructive migration). Schema:

```sql
CREATE TABLE orders.invoice_number_sequences (
    tenant_id      uuid NOT NULL,
    financial_year text NOT NULL,    -- "2026-27"
    kind           text NOT NULL,    -- 'invoice' | 'credit_note' | 'cancellation_note'
    last_used      bigint NOT NULL DEFAULT 0,
    updated_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, financial_year, kind)
);

-- Allocation is a single-row UPDATE inside the surrounding tx:
UPDATE orders.invoice_number_sequences
   SET last_used  = last_used + 1,
       updated_at = now()
 WHERE tenant_id = $1 AND financial_year = $2 AND kind = $3
RETURNING last_used;
-- INSERT … ON CONFLICT DO NOTHING then UPDATE on the first call per (tenant, fy, kind).
```

Why a UPDATE-on-row, NOT `nextval()` on a Postgres SEQUENCE: SEQUENCEs are global + non-transactional — a rolled-back tx still burns the number, producing GAPS the GSTR-1 audit will flag. A row-update inside the surrounding business tx rolls back if the parent tx rolls back, preserving gaplessness under both happy path and failure path.

Concurrency on the same (tenant, fy, kind) row is serialised by Postgres row-lock; throughput is the rate at which the tenant invoices things (not a bottleneck — even a 10k-invoice/day tenant peaks at <1/sec on average). High-contention dispatch flows happen across DIFFERENT invoice rows; only the sequence row contends.

Format string `INV/2026-27/00047` is rendered in the application layer from `(kind, financial_year, last_used)`. The persisted row carries the formatted string as a denormalised `invoice_number` column for GSTR-1 export readiness.

### 4. Order fulfillment saga via outbox correlation (not `Saga<T>`)

The .NET parent uses Wolverine `Saga<T>` for cross-module coordination of:

```
OrderConfirmed → reserve stock (Inventory) → StockReadyForDispatch  → InTransit
                                        ↓
                                  StockReservationFailed → notify user
OrderCancelled → fire compensation events based on current state
```

Wolverine `Saga<T>` is a stateful long-running process with its own persistent table tracking the saga's state. Go has no equivalent — but it has **outbox + integration events + correlation IDs**, which is the lower-level primitive Wolverine sagas are built on.

The pattern: a "saga" in the Go port is a NAMED CORRELATION FLOW, not a stored process. The Order aggregate emits the integration event (`OrderConfirmedV1`); the Inventory module's subscriber consumes it + responds with another event (`StockReservedV1` or `StockReservationFailedV1`); the Orders module subscribes to those response events + transitions its own state (publishes `StockReadyForDispatchV1` on success, transitions to a stuck/operator-attention state on failure).

The "saga state" lives where it belongs — on the `Order` aggregate's own state machine. The "saga coordinator" is the union of subscribers that route response events back to Order state transitions.

Why this works for Orders' flow:

- The longest correlation flow is `OrderConfirmed → StockReserved → ReadyForDispatch` — three events across two modules. Wolverine's `Saga<T>` is overkill for this; explicit subscribers are clearer.
- Cancellation compensation is **branching** (different compensating actions based on current state at cancel-time), not **sequenced** (no order-dependent multi-step rollback). Each compensating event is its own subscriber: `OrderCancelledV1` → Inventory's `unreserve` subscriber; `OrderCancelledV1` → Invoice's `mint CreditNote` subscriber (when `state >= invoiced`); `OrderCancelledV1` → Dispatch's `cancel consignment note` subscriber (when `state >= dispatched`). Each subscriber is independently idempotent + receives its own delivery.
- Timeouts (BRD §6.4 — `StockPending > 24h → alert`; `InTransit > 14 days → DeliveryOverdue`) are river periodic jobs that scan the Orders table for stuck states, not saga-scheduled callbacks. v0.2 ships the periodic-job framework + leaves alerting for v0.3.

A correlation_id (the original OrderConfirmedV1's message UUID) flows through every downstream event's metadata header so observability + audit-log queries can stitch the chain. The existing `messaging.HeaderCorrelationID` middleware (ADR 0056) handles this transparently.

Saga state is NOT in a separate table. There is no `orders.sagas` table. The Order aggregate's `state` column IS the saga state. The integration events ARE the saga messages. The subscribers ARE the saga handlers. **Zero new infrastructure.**

### 5. Read-side `OrderSummary` via sqlc JOIN query, not Marten projection

The .NET parent reads `OrderSummary` (Marten inline projection — strong consistency, rebuilt on every event). Go ships an `order_summary` sqlc query that JOINs `orders.orders` + `orders.invoices` + `orders.payments` + `orders.consignment_notes` (foreign-key) into a flat row. The query runs at read time; cache via HybridCache (per ADR 0042 dashboard profile — 1 min L1 / 5 min L2 + jitter).

There is NO denormalised read table at v0.2. If the JOIN becomes a bottleneck under measured load, ADR 0041 (CQRS read models via subscribers) provides the path: spin up an `order_summary` subscriber that materialises a denormalised view table. Don't pre-build that complexity.

## Consequences

**Positive:**

- Five focused aggregates + five repositories — every file under ~300 lines, every aggregate's invariant set fits in one head.
- Audit covered by outbox (ADR 0027) — no event-store layer to maintain.
- Gapless invoice numbering survives crash-recovery + rollback because allocation is row-update inside the business tx, not a non-transactional `nextval()`.
- Saga coordination uses ONLY primitives already in the project (outbox, Watermill, correlation_id middleware). No new infrastructure.
- Per-aggregate `FakeRepository` per ADR 0062 — every business rule testable without Postgres.

**Negative:**

- Cancellation compensation logic is spread across multiple subscribers (one per affected module). Tracing "what happens when an order at state=invoiced cancels" requires reading three subscriber files instead of one saga file. Mitigation: ADR 0063's §4 + each subscriber's doc-comment cross-references the others; an integration test exercises the full cancellation fan-out.
- No replay-driven historical state reconstruction. If the business asks "what did this order look like on June 3rd?", the answer comes from outbox audit, not aggregate-replay. Acceptable per ADR 0035 (revisit if temporal-querying becomes a core feature).
- Per-tenant invoice sequences mean a tenant cannot share counter-state across instances (no v0.3 read replica can issue invoice numbers). Already true for the .NET parent at v0.1 scope.

## Alternatives considered

1. **Build a pgx-based event store for Orders matching .NET.** Rejected per ADR 0035: cost too high without library support, outbox already covers the audit need, no measured demand for temporal queries.
2. **Use a Postgres SEQUENCE for invoice numbers.** Rejected: non-transactional, burns numbers on rolled-back tx → gaps → GSTR-1 audit failure.
3. **Build a `Saga<T>`-equivalent in Go (esja or similar).** Rejected: subscribers + correlation_id are the underlying primitives anyway; explicit subscribers are clearer than implicit state machines.
4. **Collapse Quotation into Order.** Rejected: quotation revisions are their own audit need; collapsing forces a JSON revision-history column on Order that breaks the "Order owns confirmed-state, Quotation owns proposal-state" invariant separation.

## Sources

- BRD §6.4 — Orders module Marten + Saga specification (the prescription this ADR adapts for Go).
- BRD §A-014 — Invoice numbering / GSTR-1 gapless rule.
- ADR 0003 — state-based persistence default.
- ADR 0035 — event-sourcing deferred (the load-bearing precedent).
- ADR 0027 — outbox-as-audit.
- ADR 0008 — Watermill outbox + forwarder.
- [Brandur Leach — There's always an events table](https://brandur.org/fragments/events) — outbox-as-audit canon.
- [Vernon — Implementing Domain-Driven Design ch.7 + ch.10](https://www.informit.com/store/implementing-domain-driven-design-9780321834577) — aggregate boundary rules.
- [Stripe / Square / Shopify Order — public docs] — five-aggregate decomposition reference (no public source — internal observation from FAANG-canon implementations).

## Fitness function

Two mechanical assertions land as arch tests at the Orders-slice ship:

1. `TestArch_OrdersAggregatesDoNotImportPgx` — `internal/orders/domain/**` cannot import `pgx`, `pgxpool`, `pgtype`, or `internal/orders/adapters/db`. Per ADR 0047 layer-boundary discipline.
2. `TestArch_OrdersInvoiceNumberHasGaplessAllocation` — `internal/orders/adapters/invoice_number_repository_pg.go` allocates via `UPDATE … RETURNING last_used` inside a UoW closure; NEVER via `nextval()` on a SEQUENCE. Catches the obvious-but-tempting wrong choice.

Aggregate-count, state-machine-shape, and saga-correlation-flow are convention-only — not mechanically expressible; this ADR is the canon.
