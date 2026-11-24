# ADR 0072 — Dispatch is one database transaction: stock, order and consignment note change together, or none of them change

**Status:** Accepted (owner decision, 2026-11-24).
**Date:** 2026-11-24
**Amends:** [ADR 0063](0063-orders-module-state-based-aggregates-and-saga.md) §4 (the `order_packed` fan-out and the order moving to `dispatched` when the consignment note is created) and [ADR 0071](0071-data-is-retained-erasure-is-deferred.md) (the replay note on the order-lifecycle handlers).

## Context

The owner's words: *"It should be transactional process."* Stock, order and consignment note change together at dispatch, or none of them change.

Before this decision the goods and the books could disagree in four ways (review G2d B4, G2e):

* Inventory deducted stock **asynchronously** on `orders.order_packed.v1` (`ConsumeStock`), so stock fell at the pack, before the goods left, and a consume that timed out left a dispatched order whose stock was never deducted. The dead letter was kept but no route could repair it.
* `Order.AttachConsignment` moved the order to `dispatched` when the note was **created**, while the goods were still in the warehouse.
* `MarkDispatched` asked its gates (invoice, order state, expiry) **outside** the note's transaction, so the answers could change before the note moved.
* The consume and the release read the order's outstanding hold **before** their transaction. Two workers acted on one snapshot and wiped a hold another order keeps on the same batch.

Nothing is live and no real data exists, so there is no backfill, no compatibility shim and no legacy tolerance.

## Decision

`POST /api/v1/dispatch/consignment-notes/{id}/dispatch` runs **one tenant-scoped transaction**, in this lock order, and commits once. Any error rolls back all of it.

1. Lock the note (`FOR UPDATE`). Already dispatched with the same docket: success, nothing touched, no second deduction. Another docket: the existing refusal. A docket the note would refuse is refused here, before anything moves.
2. Lock the order (`FOR UPDATE`) and move it `invoiced` to `dispatched` (not invoiced: 409 `order_not_invoiced`; any other state, including a cancelled order that kept its invoice: 409 `order_not_dispatchable`).
3. Lock the order's batches **in batch-id order, then read the outstanding hold inside the transaction.**
4. No outstanding hold: **409 `order_stock_not_held`** (goods never leave without a deduction). A hold that does not **equal** the quantity the order confirmed, product by product (less, more, or a product the order does not list): **409 `order_stock_mismatch`** (the Orders step returns the confirmed quantities from the order it just locked; the Inventory step compares them with the hold it read under the batch locks, before anything moves). A held batch past its printed expiry on the company's business day: 409 `order_stock_expired`, judged by the one expiry rule of the Inventory batch.
5. For each batch: release the hold and ship it (outbound), writing both movement rows.
6. The order is `dispatched` and linked to its consignment note (the async attach of the same note stays a no-op).
7. The note is `dispatched`, its event in the outbox.

The events of the same transaction are the two stock movements per batch and the note's `dispatch.consignment_note_dispatched.v1`. The order's own transition writes no outbox row: Orders suppresses its bare `AdvancedEvent`, as for every order transition except confirm, pack and cancel.

### Ports, not imports

Dispatch owns two narrow ports and the composition root binds them in `cmd/internal/bridges` (`OrderShipper` to the Orders `DispatchOrderHandler`, `StockShipper` to the Inventory `ShipOrderStockHandler`); modules never import each other. They **replace** the old read ports (`OrderDispatchChecker`, `StockExpiryChecker`, Inventory's "ever reserved is expired" query), so there is one expiry authority, the batch's own rule applied when the outbound is made.

### The trap: `WithinTx` never joins

`pg.Transactor.WithinTx` and `WithinTenantTx` **always begin a new transaction**; they do not join one already in ctx. The repositories join (`pg.TxFromContext`); the transactor does not. A step that called an existing handler built on `WithinTx` from inside the dispatch transaction would silently take a second connection: not atomic, and a deadlock on a small pool. `WithinTx` is **not** changed globally. Instead:

* the steps run inside the note's `UpdateByID` closure, called with the ctx of **the** transaction, and use only repositories (which join);
* each bridge **fails closed** with `ErrNoActiveTx` when its ctx carries no transaction (the same idea as `ErrNoActiveTx` in the Inventory outbox writer);
* the proof "dispatch completes on a ONE-connection pool" catches any nested transaction: it would wait for a second connection and run into its deadline.

### One lock-then-read routine for the hold

The release (cancellation) and the shipment (dispatch) share one private routine in Inventory: read which batches the order holds (only to know which rows to lock), lock them in batch-id order, then read the outstanding hold again **under the locks**. A second worker that drained the hold while this one waited has committed when the lock is granted, so the second read is empty. Two concurrent runs drain a hold once and never touch another order's hold; two orders that hold the same batches in opposite line order queue instead of deadlocking. The release stays replay-safe (ADR 0071): a late run drains only what is still held.

### What changes around it

* `Order.AttachConsignment` only **links** the note. `Order.MarkDispatched` is the one edge into `dispatched`.
* The consignment saga subscribers no longer invoice or dispatch: `consignment_note_created` links the note; `consignment_delivered` links, then delivers an order that must already be dispatched (otherwise a conflict that goes to the dead letters for a person).
* `orders.order_packed.v1` **no longer touches stock.** The `ConsumeStock` subscriber, its registration, its wiring and its tests are deleted. The note creation and the auto-invoice on the pack stay.
* The credit-note rules (`invoiced` or `dispatched` prior state) are unchanged: a cancel after the goods left still mints its note.

## Consequences

* **Stock falls at dispatch, not at pack.** Between the pack and the dispatch the units stay reserved on their batches.
* **Cancelling an order after the goods left keeps the stock deducted** (the release finds no hold). Putting the units back is a return, round G8.
* **A reserve that was lost leaves an order with no hold.** Such an order can still be packed (round G5 closes that, C9), but it can **never be dispatched**: the dispatch is refused with `order_stock_not_held` and nothing changes.
* **A hold that differs from what was ordered is refused** (review G2g Q8/M8): the rule is equality, so a lost or partial reservation (less) and a reservation applied twice (more) both stop the goods, with `order_stock_mismatch`, and nothing changes. Round G5 should apply the same rule at the pack (C9).
* The timed-out consume (review G2d B4) is gone by construction: there is no consume event to time out.
* **Proofs** (real Postgres; the production bus where events are involved), in `cmd/api/dispatch_transaction_e2e_test.go`: success changes everything together; a failure injected after the stock step, the order step, the note step and at the outbox insert changes nothing; the same-docket retry deducts nothing twice; concurrent dispatches of one note deduct once; dispatch racing cancel is shipped or released, never both and never neither; concurrent dispatch and releases with more than one worker leave the other order's hold intact; opposite line order does not deadlock; dispatch completes on a one-connection pool; no hold and an expired hold are 409 with nothing changed; a late or replayed `order_packed` changes no stock.

## Fitness function

`TestArch_DispatchStepsJoinTheDispatchTransaction` (in `internal/architecture/`) gates the shape of the trap: the Inventory shipment step and the Orders dispatch step contain no `WithinTx`, `WithinTenantTx`, `WithinTxPgx`, `WithinTxPgxTenant` or unit of work; `MarkDispatchedHandler.Handle` opens exactly one tenant transaction; both composition-root bridges check `pg.TxFromContext` so neither step runs outside it. The behavioural proofs listed above (the one-connection pool and the failure injected at each step) catch what the text cannot.
