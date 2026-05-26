// Package orders is the Orders bounded context — quotations, orders,
// invoices, credit notes, payments.
//
// Layout per CLAUDE.md "Three unbreakable rules":
//
//	internal/orders/
//	├── domain/                 entities, VOs, repository interfaces
//	│   ├── quotation/          quote draft + revisions + approval
//	│   ├── order/              the state-machine aggregate
//	│   ├── invoice/            gapless-numbered tax invoice
//	│   ├── creditnote/         gapless-numbered reversal
//	│   └── payment/            token / full / refund receipt
//	├── app/                    command + query handlers
//	├── ports/                  HTTP + event subscribers
//	├── adapters/               pgx/sqlc + outbox
//	└── integrationevents/      framework-neutral wire records
//
// Per ADR 0063 (the load-bearing decision doc for this module):
//
//   - Five aggregates, NOT one fat Order. Quotation + Order + Invoice
//     + CreditNote + Payment each carry their own invariant scope +
//     lifecycle (mutable, immutable, append-only — different shapes).
//   - State-based persistence per ADR 0003 + 0035. NO event sourcing.
//     The .NET parent uses Marten event streams for Orders; this Go
//     port uses a state column + integration events drained to the
//     outbox. The outbox IS the audit ledger.
//   - Order has a strict 10-state state machine. Invalid transitions
//     return [order.ErrInvalidTransition]. Cancellation is reachable
//     from any non-terminal state.
//   - Invoice + CreditNote numbering is gapless per (tenant_id,
//     financial_year, kind) via row-UPDATE-returning, NEVER via
//     Postgres `nextval()` (which would burn numbers on rollback).
//   - Fulfillment "saga" is the union of subscribers that route
//     response events back to Order state transitions. There is no
//     `Saga<T>` and no `orders.sagas` table — Order's own state column
//     IS the saga state.
//
// Cross-aggregate references are by ID only (composite-FK at the DB).
// Cross-module references are via integration events on the bus, per
// ADR 0001 modular-monolith canon.
package orders
