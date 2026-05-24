# 0059 — Platform module: aggregates, events, and concurrency

**Status:** Accepted

**Date:** 2026-05-23

**Context window:** Phase 2 Slice 1 — opens the Platform bounded context
(verification → marketplace pipeline). First non-Identity module in the
Go rebuild; sets the precedent for all subsequent modules (CRM, Orders,
Inventory, Dispatch, Tasks, Notifications).

## Context

The .NET parent's `Modules.Platform` (BRD §6.2) owns four aggregates —
`UnverifiedContact`, `VerificationCall`, `PlatformLead`, `LeadCredit` —
covering the LeadKart platform team's lead-sourcing pipeline and the
tenant-facing marketplace. The Go rebuild needs:

1. Aggregate boundaries that match the actual write patterns (avoid
   forcing two aggregates into one TX, or shoving lifecycle data into the
   wrong aggregate).
2. A concurrency story for `LeadCredit` — purchase rates burst, multiple
   handlers can race on the same balance row (BRD §4.2: one credit = one
   permanent purchase, no refunds, no expiry).
3. An RLS posture for `platform_leads` that supports marketplace browse
   across tenants WITHOUT giving tenants visibility into the verification
   pipeline.
4. Frozen integration-event contracts so the downstream consumers (CRM
   in this slice; Notifications + future analytics in later slices) can
   wire against stable shapes BEFORE the producer ships.

## Decision

### Four aggregates, one bounded context

| Aggregate | Tenant-scoped? | Persistence | Why a separate aggregate |
|---|---|---|---|
| `UnverifiedContact` | NO (Platform) | state-based | Carries the Lead Agent's work queue + BRD §5 form fields. State machine (`New → InCall → Verified | Rejected | Busy`) is owned here. |
| `VerificationCall` | NO (Platform) | state-based, append-only | Audit-load-bearing call log. Each call is an immutable event row with outcome + optional callback window. Separate aggregate because (a) lifetime is per-call not per-contact, (b) tests routinely add multiple calls to the same contact without state-machine churn. |
| `PlatformLead` | NO for writes, READ-EXPOSED for marketplace | state-based | The marketplace listing — projected from a Verified `UnverifiedContact`. `sold_to_tenant_id` is the only post-creation mutation (one-way to terminal `Sold`). |
| `LeadCredit` | YES | state-based + optimistic concurrency | One row per tenant. Burst-mutated under purchase load → optimistic version per .NET ADR-015. |

**Why `UnverifiedContact` and `VerificationCall` are separate aggregates
even though they're owned together by the Lead Agent's workflow:**
TDL canon (Vernon IDDD ch. 10 + Wild Workouts Nov 2025): aggregate
boundaries follow consistency requirements, not entity proximity. A call
outcome does NOT need to mutate the contact in the same TX — the contact
state transition (`InCall → Verified|Rejected|Busy`) is the outcome
itself; the call row is the historical record. Forcing them into one
aggregate would mean every call-log append re-validates contact
invariants for no reason. Two aggregates, two repository methods, one
handler orchestrates both on its UoW transaction.

### `LeadCredit` concurrency: integer version, not Postgres xmin

The .NET parent uses Postgres `xmin` via EF Core's `IsRowVersion` mapping
(ADR-015). For Go + sqlc + pgx we choose an **integer `version` column**
with the same retry-on-conflict semantics:

```sql
ALTER TABLE platform.lead_credits ADD COLUMN version bigint NOT NULL DEFAULT 0;
-- update: UPDATE ... SET balance=$1, version=version+1 WHERE id=$2 AND version=$3
-- 0 rows affected = optimistic conflict
```

Rationale:
- `xmin` is a system column that pgx exposes via `pgtype.XID8` but sqlc
  treats it as just another column. The `xmin` value changes on every
  UPDATE invisibly, so the SET clause cannot include it (Postgres
  rejects). The check-then-set pattern requires reading `xmin` in
  SELECT, then `WHERE id=? AND xmin=?` in UPDATE. Workable but adds
  uuid-typing friction sqlc handles awkwardly.
- An explicit `version bigint` column is the universally idiomatic shape
  across sqlc / GORM / DBeaver tooling, and it appears in the row
  signature so handlers + tests can branch on it without raw-SQL escape
  hatches. JPA/Hibernate `@Version`, EF Core `[ConcurrencyCheck]`,
  Sequelize `version: true`, Doctrine `@Version` — every mainstream ORM
  defaults to an explicit numeric version field for the same reason.

Handler-side: on `ErrLeadCreditConflict` (the typed sentinel the
repository returns when 0 rows are affected), the command handler
retries up to 3 times with a small jitter (~10ms) before surfacing
HTTP 409 to the client. Per .NET ADR-015 + Polly canon.

### Marketplace browse: RLS exception for unsold `platform_leads`

`platform.platform_leads` is the only table in the slice with an RLS
posture that diverges from the LeadKart canon (`tenant_id =
app.current_tenant() OR app.is_platform()`). The marketplace browse
endpoint serves rows to ANY authenticated tenant — the whole product
proposition is "verified leads available for purchase across the
platform." The RLS shape:

```sql
ALTER TABLE platform.platform_leads ENABLE ROW LEVEL SECURITY;
ALTER TABLE platform.platform_leads FORCE ROW LEVEL SECURITY;

-- Browse is open to anyone (the row predicate is on sale state, not tenant)
CREATE POLICY platform_leads_select_unsold ON platform.platform_leads FOR SELECT
    USING (sold_to_tenant_id IS NULL OR sold_to_tenant_id = app.current_tenant() OR app.is_platform());

-- Writes are platform-only (verification + topup pipeline)
CREATE POLICY platform_leads_write ON platform.platform_leads FOR ALL
    USING (app.is_platform()) WITH CHECK (app.is_platform());
```

The SELECT policy reads "any tenant can see UNSOLD leads, the purchaser
keeps seeing their own purchased leads, platform sees everything." The
WRITE policy keeps verification + purchase mutations in the platform-
operator (or platform-elevated-during-purchase) lane. The purchase
command handler runs under `TxScopePlatform` for the brief window of
the UPDATE — same shape as the existing `RegisterTenant` handler.

Marketplace filtering (BRD §4.3) is implemented via:
- `text[]` columns + GIN indexes for `product_ranges` + `dosage_forms`
- B-tree composite indexes on `(state, city, business_type)` etc.
- Numeric range index on `order_value_band` (enum text column).

No JSONB filtering — per `database.md` "JSONB rule: never in WHERE
clauses; filter columns become indexed SQL columns."

### Integration event contracts — frozen in this slice

```
LeadPurchasedV1                — TenantScoped (consumed by CRM in Phase 2.2)
LeadVerifiedV1                 — PlatformEvent (no consumer in Slice 1; reserved)
UnverifiedContactCreatedV1     — PlatformEvent (audit + future analytics)
VerificationCallLoggedV1       — PlatformEvent (audit + future analytics)
LeadCreditAdjustedV1           — TenantScoped (tenant dashboard + audit)
```

All five carry primitive fields only (UUIDs as strings on the wire via
the existing serializer; `int64` paise for money — NEVER float; ISO 8601
strings for timestamps). Per `messaging.md` "Composition, not
inheritance": no abstract base record, no nested domain VOs. `LeadSnapshot`
on `LeadPurchasedV1` + `LeadVerifiedV1` is a plain struct holding the
BRD §5 fields as primitives — CRM creates its `CrmLead` aggregate from
the snapshot without calling back to Platform (Udi Dahan event-autonomy
rule per `never-do.md` Messaging).

Renames + field-drops are V2-only (full V1 retained for outbox drain
per `messaging.md` "Event versioning — breaking change = new V2 record").

### Outbox + forwarder: own table, own topic, own forwarder

`platform.outbox` is a sibling of `identity.outbox` — same shape, same
RLS+FORCE posture, same forwarder skeleton. Watermill destination:
`"platform.events"`. The platform forwarder is a copy of the identity
forwarder with a different `db.Queries` (because sqlc package generation
is per-schema in our setup). When the third bounded context lands
(CRM), we'll evaluate whether to extract a generic forwarder against an
abstract "outbox table" — premature in the two-module state.

## Consequences

**Positive:**
- Aggregate boundaries match write patterns; each command handler is
  ~50 LOC orchestrating one or two aggregate transitions.
- `LeadCredit` optimistic-version pattern is sqlc-friendly + universally
  recognised; future maintainers don't need to understand pgx XID8.
- Marketplace browse works with a single SELECT policy that's auditable
  in a CI gate (the migration round-trip test asserts the policy text).
- CRM team can wire against `LeadPurchasedV1` immediately — the contract
  is frozen here, not contingent on Platform's internal refactors.

**Negative:**
- Per-module outbox duplication. ~150 LOC of forwarder code mirrored in
  `internal/platform/adapters/`. Acceptable for a two-module slice;
  factor out at three modules per the rule-of-three.
- Marketplace SELECT policy is the first non-uniform RLS in the system.
  Schema integrity test asserts the exception inline so a future "make
  all RLS uniform" sweep can't silently delete it.
- LeadAgent's "calls + outcome" workflow is two REST calls (POST call,
  POST verify/reject). Frontend UX may want a combined endpoint later;
  added in Slice 2 as a thin facade over the two-aggregate write if
  product asks.

**Deferred to Slice 2 / 3:**
- Reminders from busy-callback (Notifications module).
- Marketplace HybridCache wrapping (ADR 0042 SearchResults profile).
- `LeadCredit` running-balance read model via outbox subscriber
  (ADR 0041 pattern). v0.2 reads balance directly off the aggregate row.
- CSV/Excel bulk lead upload (BRD §11 "Open Items" — Phase 1 high
  priority but separate slice).
- External GST status verification API call on `MarkVerified`
  (BRD Appendix B.2 — Phase 2 enhancement).

## Amendment — review-pass (2026-06-01)

Independent reviewer flagged 5 CRITICAL + 7 HIGH + 6 MEDIUM findings on
the first-pass Slice 1 implementation. Fix-pass closure:

### Wire-contract drift (CRITICAL — frozen by amendment)

The frozen brief specified `TenantID string`. First-pass shipped
`TenantIDValue uuid.UUID` because Go-idiomatic typing felt cleaner.
Cross-language consumers (CRM subscriber's local mirror, future
non-Go subscribers) need the bare wire string. **All UUID-shaped
fields on Platform integration events are typed as `string` on the
wire AND in Go.** The runtime conversion + outbox metadata layer
handles real `uuid.UUID` round-trips.

Field renames:
- `LeadPurchasedV1.TenantIDValue → LeadPurchasedV1.TenantID`
- `LeadCreditAdjustedV1.TenantIDValue → LeadCreditAdjustedV1.TenantID`

The `TenantScoped` interface accessor is `TenantIDString()` (was
`TenantID()` → uuid.UUID) — the wire field's name (`tenant_id`) is
unchanged.

### Optimistic-concurrency retry backoff (CRITICAL)

The ADR said "retries up to 3 times with a small jitter (~10ms)".
First-pass shipped a tight loop with NO sleep — defeating
thundering-herd protection. Fix-pass adds
`time.Sleep(rand.N(10*time.Millisecond))` between attempts in both
`PurchaseLeadHandler.Handle` and `TopupLeadCreditsHandler.Handle`
(`internal/platform/app/command/{purchase_lead,topup_lead_credits}.go`).
Test seam: `jitterDuration` package-level var swappable in tests.

### `platform.outbox.tenant_id` becomes NULLABLE (CRITICAL — C3)

The first-pass schema declared `tenant_id uuid NOT NULL` + the writer
passed `uuid.Nil` for Platform-scoped events. That fabricates a
sentinel UUID downstream subscribers cannot distinguish from a real
tenant FK. **Migration 20260601000002** drops the NOT NULL constraint,
backfills existing `uuid.Nil` rows to NULL, and adjusts the RLS
policies (SELECT + INSERT) to allow `tenant_id IS NULL AND
app.is_platform()`. The forwarder reads the column's `Valid` flag +
omits the `tenant_id` Watermill metadata header when NULL — matching
the wire shape downstream subscribers expect.

### Defense-in-depth `RequirePlatform` on platform-only routes (CRITICAL — C5)

First-pass shipped `RequirePermission(Manage)` alone on
`POST /api/v1/platform/unverified-contacts`, the LIST endpoint, and
`POST /api/v1/platform/lead-credits/topup`. A tenant role with the
Manage permission (misconfiguration) could reach the platform pipeline.
Fix-pass layers `RequirePlatform` OUTSIDE the permission gate on all
five `/unverified-contacts/*` routes + the topup route — mirroring
identity's `/v1/platform/*` pattern.

### Verify-handler terminal-state guard (HIGH — H11)

First-pass let a double-verify against an already-Verified contact
quietly emit a SECOND `LeadVerifiedV1` (with a fresh `leadID` →
phantom PlatformLead in CRM). New `command.ErrContactAlreadyTerminal`
sentinel; handler refuses with HTTP 409. Verify-after-reject also
maps to the same sentinel (refuse rather than mask as `ErrInvalid`).

### Marketplace SELECT omits PII (HIGH — H12)

`MarketplaceBrowse` SELECT list now excludes `email`, `mobile_e164`,
`gst_number`, `pan_number`, `street`. The PII columns remain on the
row + remain readable via `GetByID` post-purchase under the buyer's
tenant scope (the only legitimate consumer post-sale). The browse
SELECT list is tested via
`TestPlatformLeadRepository_MarketplaceBrowse_OmitsPII` — a
regression that adds a `SELECT *` (or re-introduces a PII column)
fails CI.

### Event-suppression mapper contract (HIGH — H6)

The mechanical mapper `integrationevents.FromDomainEvent` returns
`(nil, nil)` for 5 domain event types whose integration counterparts
are emitted directly by handlers (see mapping.go godoc). Contract is
now bijectively enforced via
`TestArch_FromDomainEvent_HandlesAllRegisteredDomainEvents` — adding
a new domain event without a mapper case (or documented suppression)
fails CI.

### Modern-Go sweep (HIGH — H7, H8)

`for attempt := 0; attempt < N; attempt++` → `for range N` (Go 1.22+)
across `purchase_lead.go` + `topup_lead_credits.go`. `interface{}`
→ `any` in `ports/http.go:writeJSON`.

### `mustParseUUID` removed from request path (MEDIUM — M13)

`internal/platform/app/command/helpers.go` deleted — the panic-on-
malformed-UUID helper was an anti-pattern per CLAUDE.md
"MustNewX init-time + tests only". Handlers now emit string-shaped
domain IDs onto wire events directly (matches the C4 wire-contract
amendment above; no parse step needed).

### Deferred / acknowledged

- **M15** — `platform_leads.created_by_membership_id` audit column is
  not shipped in Slice 1. The verified-by + the source contact's
  created-by chain together cover the audit trail; explicit
  `created_by` lands when a "manually-created lead" flow opens
  (Slice 3+).
- **M16** — Outbox `Topic = "platform.events"` is the canonical
  Watermill destination; route is by `event_type` metadata header,
  not topic-per-event-type. Mirrors identity's pattern; no change.
- **M18 / per-module outbox** — Rule-of-three deferral honored. The
  duplication will land in a shared substrate when the CRM module's
  forwarder ships (Phase 2.2).

### Test fixture rollback semantics

`platformtest.NewFakeUnitOfWork(fakes...)` now snapshots each
registered fake's state at WithinTx entry + restores on closure
error — modelling Postgres ROLLBACK so unit tests catch
"loser-was-debited" regressions (H10). The zero-value
`FakeUnitOfWork{}` continues to work for closure-only flows.

## References

- `BRD.md` §3 (Roles), §4.2-4.5 (Marketplace, Stages, Callback),
  §5 (Lead Form locked fields), §6.2 (Platform Module spec)
- `.NET` `ADR-015` (Optimistic concurrency — retry on conflict)
- `.NET` `.claude/rules/multi-tenancy.md` (RLS policy shapes + platform
  bypass GUC)
- `.NET` `.claude/rules/messaging.md` (Integration event vocabulary +
  Udi Dahan autonomy rule)
- ADR 0006 (Multi-tenancy RLS), ADR 0008 (Watermill outbox), ADR 0027
  (Outbox doubles as audit), ADR 0038 (Cursor pagination), ADR 0042
  (Cache TTL strategy), ADR 0046 (OpenAPI spec-first), ADR 0047
  (Layer-boundary discipline), ADR 0049 (URL design + route arch gate),
  ADR 0050 (OpenAPI as code-of-record + drift gates), ADR 0051
  (Single-module type placement)


## Fitness function

`TestArch_AggregatesHaveFactoryAndUnmarshal + TestArch_RepositoriesHaveUpdateByIDFn` (in `internal/architecture/`).

Platform aggregates (leadcredit, platformlead, unverifiedcontact, verificationcall) satisfy the factory pattern; leadcredit + verificationcall append-only exceptions are documented in the UpdateByID test.
