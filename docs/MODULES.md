# Module status — Phase 2 in flight

> **Last updated:** 2026-05-26
> **Tracking:** the rebuild plan + BRD §6 module breakdown vs. branches pushed for review.

This page lists each of the 8 bounded contexts + cross-cutting building blocks, what's shipped, what's on a review-pending branch, and what's still TODO. Update on every merge.

---

## Top-level scorecard (post-Identity)

| Module | Phase % | Latest delivery |
|---|---|---|
| **Identity** | 98% | shipped on `main` (v0.2 tag) |
| **Platform** | 85% | Phase A.1 PR open: `feature/platform-tenant-registered-subscriber` (TenantRegistered → init lead credits) |
| **CRM** | 75% | Phase A.2 PR open: `feature/crm-reminder` (Reminder aggregate + callback subscriber + mature-lead cron) |
| **Inventory** | 55% → 75% | Phase A.3 PR pending: `feature/inventory-alerts-fefo-refdata` (FEFO + expiry alerts + GST defaults) |
| **Orders** | 0% → 35% | Phase B.1+B.2 PR open: `feature/orders-domain-skeleton` (ADR 0063 + 5 aggregates domain layer) |
| **Dispatch** | 0% → 30% | Phase C.1 PR open: `feature/dispatch-domain-skeleton` (ConsignmentNote aggregate + OrderPacked subscriber) |
| **Tasks** | 0% → 30% | Phase C.2 PR pending: `feature/tasks-workitem-bootstrap` (WorkItem aggregate + auto-create subscribers) |
| **Notifications** | 0% → 25% | Phase D.1 PR open: `feature/notifications-domain-skeleton` (Notification aggregate + dedup primitive) |

Cross-cutting:

| Building block | Status | Latest |
|---|---|---|
| Pincode reference data | Domain + Reader + 8-metro fake | `feature/pincode-refdata` |
| ProductCategoryGstDefault | Pending (in A.3) | `feature/inventory-alerts-fefo-refdata` |
| OpenAPI spec | Drifting against new modules | per-module slice will catch up |

---

## Identity (`internal/identity/...`)

**Status:** v0.2 ships. 98% complete per the master plan.

Shipped on `main`:
- Tenant, Person, Membership, Role, RefreshToken, PermissionRequest, Impersonation, RoleHierarchy aggregates.
- 60+ HTTP routes; 5 subscribers; closed-set permission catalog.
- Outbox + forwarder + AuditMiddleware.

Outstanding (post-Phase-2 polish):
- MFA / step-up auth (BRD §11 — deferred until 100s of paying clients).
- Google OAuth login (BRD §11 — architecture-ready).

---

## Platform (`internal/platform/...`)

**Owns:** UnverifiedContact, VerificationCall, PlatformLead, LeadCredit.

Shipped on `main`:
- All 4 aggregates with strict state machines.
- 9 HTTP routes (Lead Agent flow + tenant marketplace browse + purchase + lead-credits balance/topup).
- Outbox forwarder; per-aggregate FakeRepository.

Phase A.1 PR open — **`feature/platform-tenant-registered-subscriber`**:
- TenantRegisteredV1 subscriber → InitialiseLeadCredits handler.
- Zero-balance row created at tenant registration time.
- 7 unit tests + 2 integration tests (full Watermill router path).

Outstanding:
- Bulk lead upload (BRD §11 high-priority — CSV/Excel + validation + error report).
- Verification-call audit-log exposure on a Lead Agent's queue HTTP route.

---

## CRM (`internal/crm/...`)

**Owns:** CrmLead, CallLog, Reminder, AssignmentHistory.

Shipped on `main`:
- CrmLead with stage + temperature state machines.
- CallLog + AssignmentHistory append-only aggregates.
- 8 HTTP routes + LeadPurchased subscriber + OpenAPI.

Phase A.2 PR open — **`feature/crm-reminder`** (Agent A.2 work):
- Reminder aggregate with 3 factories (callback / mature_lead / manual).
- CallLogged subscriber auto-creates callback reminders.
- Daily river job for 3-month mature-lead scan (BRD §4.7).
- 4 new HTTP routes + 3 new V1 integration events.
- CallLoggedV1 extended with optional callback window fields.

Outstanding:
- AssignmentHistory query endpoints (BRD §6.3 — "who owned this lead in Q3").
- Bulk reassignment / round-robin auto-assign.
- `search_leads_view` projection (ADR 0041 CQRS read model).

---

## Orders (`internal/orders/...`)

**Owns:** Quotation, Order, Invoice, CreditNote, Payment.

Status: 0% on `main`; foundation slice on `feature/orders-domain-skeleton`.

Phase B.1 + B.2 PR open — **`feature/orders-domain-skeleton`**:
- **ADR 0063** — state-based aggregates (NOT Marten ES per ADR 0035), Postgres-sequence-style invoice numbering (UPDATE RETURNING, never `nextval`), outbox-correlation saga (no `Saga<T>`).
- **Quotation** aggregate — draft/revised/approved/rejected; revision chain as append-only Items snapshot.
- **Order** aggregate — 10-state strict state machine; cancellation reachable from any non-terminal; CancelledEvent carries PriorState for compensation routing.
- **Invoice** aggregate — append-only; per-order partial-unique invariant; HSN tax breakdown for GSTR-1.
- **CreditNote** aggregate — covers both `credit_note` (post-delivery return) + `cancellation_note` (pre-delivery cancellation) via Kind discriminator.
- **Payment** aggregate — append-only; token/full/refund; external-reference dedup.
- **InvoiceNumber** primitive — gapless allocation per (tenant, FY, kind); FromDate() resolves Indian FY (Apr–Mar).
- Per-aggregate `FakeRepository` in `<aggregate>test/` packages per ADR 0062.

Outstanding (follow-up slices on the same branch or separate):
- B.3: app-layer command handlers for the 10 state transitions.
- B.4: pgx adapters + migrations.
- B.5: OrderFulfillmentSaga subscribers (cancellation compensation fan-out per ADR 0063 §4).
- B.6: HTTP routes + OpenAPI.

---

## Inventory (`internal/inventory/...`)

**Owns:** Product, Batch, StockMovement.

Shipped on `main`:
- All 3 aggregates with optimistic-version on Batch.
- 10 HTTP routes (product CRUD + batch CRUD + stock movements).
- ADR 0061 docs the slice-1 decisions (5-question ADR).

Phase A.3 PR pending — **`feature/inventory-alerts-fefo-refdata`** (Agent A.3):
- FEFO query (`ListFefoForProduct`).
- Expiry-scan river job (90-day BRD default).
- Reorder-scan river job.
- ProductCategoryGstDefault reference data (`shared.product_category_gst_defaults`).
- New ProductCategory + reorder_level + expiry_alert_threshold_days columns on products.

Outstanding:
- Quarantine + write-off operator endpoints.
- Bulk product/batch upload (BRD §11 medium-priority).
- Stock reservation subscriber on OrderConfirmedV1 (lands with B.5 saga slice).

---

## Dispatch (`internal/dispatch/...`)

**Owns:** ConsignmentNote.

Status: 0% on `main`; foundation slice on `feature/dispatch-domain-skeleton`.

Phase C.1 PR open — **`feature/dispatch-domain-skeleton`**:
- ConsignmentNote aggregate with 5-state strict machine (`pending → dispatched → in_transit → delivered | failed`).
- Carrier-skip-in-transit path supported (dispatched → delivered directly).
- 5 integration events (`dispatch.consignment_note_created/dispatched/in_transit/delivered/failed.v1`).
- ConsignmentDeliveredV1 is the ADR 0063 §4 saga input — Orders subscriber will route off it.
- OrderPacked subscriber (LOCAL MIRROR pattern until Orders integrationevents lands on the same branch).
- End-to-end test proves: envelope → subscriber → command → aggregate → fake-repo-drain (DRAINED event verification).

Outstanding:
- C.1.2: pgx adapter + migration + outbox forwarder.
- C.1.3: Carrier-status webhook receiver (HTTP route consuming external carrier callbacks).
- C.1.4: 4 HTTP routes (list / get / mark-dispatched / mark-failed).

---

## Tasks (`internal/tasks/...`)

**Owns:** WorkItem.

Status: 0% on `main`; foundation slice pending on `feature/tasks-workitem-bootstrap`.

Phase C.2 PR pending — **`feature/tasks-workitem-bootstrap`** (Agent C.2):
- WorkItem aggregate with Type / Priority / State enums per BRD §6.8.
- Auto-creation subscribers (CallLogged → CallbackReminder; CrmLeadConverted → FollowUp).
- Auto-completion via source-entity tracking.
- Hierarchy-gated assignment using existing `identity.role_hierarchy_edges`.
- Overdue scan job (every 15 min per BRD).
- Purge job (3-month retention per BRD).
- HTTP routes + dashboard counts query.

---

## Notifications (`internal/notifications/...`)

**Owns:** Notification.

Status: 0% on `main`; foundation slice on `feature/notifications-domain-skeleton`.

Phase D.1 PR open — **`feature/notifications-domain-skeleton`**:
- Notification aggregate with unread/read/dismissed lifecycle.
- 9-category closed catalogue (lead_assigned, order_confirmed, work_item_overdue, etc).
- All-or-none source-fields invariant (sourced notification has full triple; manual has all empty).
- FakeRepository with in-memory 5-min dedup window + UnreadCount + bulk MarkAllRead.

Outstanding:
- D.2: pgx adapter + `notifications.notifications` migration.
- D.3: subscriber-decides handlers — one per (source-module, event-type, recipient-rule) row in BRD §6.9.
- D.4: HTTP routes (inbox / unread-count / mark-read / dismiss).
- D.5: coder/websocket real-time push (ADR 0016).

---

## Cross-cutting building blocks

### Reference data (`internal/common/refdata/...`)

- **`pincode/`** — domain + Reader + 8-metro FakeReader. PR open: `feature/pincode-refdata`. PG adapter + India Post seed migration in a follow-up slice.
- **`gst_defaults/`** — pending (in Agent A.3's branch).

Existing VOs (already on `main`):
- `internal/common/pan/` — PAN VO + checksum.
- `internal/common/gst/` — GSTIN VO + checksum + GSTIN↔PAN cross-validation.
- `internal/common/druglicence/` — drug licence number VO.
- `internal/common/postaladdress/` — Address VO with India-Post pincode validation.

### Messaging infrastructure (already on `main`)

- `internal/common/messaging/` — Watermill router + middleware stack (Recoverer + TraceContext + CorrelationID + TenantContext + Idempotency + Audit + Retry per ADR 0008).
- `internal/common/messaging.IdempotentReceiver` — envelope-ID dedup via `identity.processed_messages`.

### Background jobs (already on `main`)

- `internal/common/jobs/` — river client + migration helper.
- One existing job: `audit.PurgeJob` (7-year retention enforcement, daily).
- Phase A.2/A.3/C.2 each add 1–2 daily jobs.

---

## Open PRs (review queue)

In dependency-order for merging:

1. `feature/pincode-refdata` — tiny, no-deps. **Merge first.**
2. `feature/platform-tenant-registered-subscriber` — Phase A.1; no-deps.
3. `feature/crm-reminder` — Phase A.2; no-deps.
4. `feature/inventory-alerts-fefo-refdata` — Phase A.3; pending agent.
5. `feature/orders-domain-skeleton` — Phase B.1+B.2; pure-domain, no-deps. **Merge before Tasks + Dispatch follow-ups.**
6. `feature/dispatch-domain-skeleton` — Phase C.1; references orders.* by string-typed OrderID alias; no compile-time dep.
7. `feature/tasks-workitem-bootstrap` — Phase C.2; pending agent.
8. `feature/notifications-domain-skeleton` — Phase D.1; pure-domain, no-deps.

After all 8 land:
- B.3+ (Orders app/adapters/HTTP/saga).
- C.1.2+ (Dispatch adapter/webhook/HTTP).
- D.2+ (Notifications adapter/subscribers/push).
- ProductCategoryGstDefault SuperAdmin write endpoint.

---

## Doctrine references

- [`docs/doctrine/tdl_canon.md`](doctrine/tdl_canon.md) — thought-process canon.
- [`docs/adr/0062-tdl-test-pyramid-fakes-canon.md`](adr/0062-tdl-test-pyramid-fakes-canon.md) — per-aggregate FakeRepository rule.
- [`docs/adr/0063-orders-module-state-based-aggregates-and-saga.md`](adr/0063-orders-module-state-based-aggregates-and-saga.md) — Orders module decision doc.
- `CLAUDE.md` — project quick-start map.
- `BRD.md` (links to `../LeadKart/BRD.md`) — authoritative business spec.
