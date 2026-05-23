# ADR 0060 — CRM module: aggregate boundaries, lead stage + temperature model, lead-purchased idempotency, filter-index strategy

**Status:** Accepted
**Date:** 2026-06-02

## Context

CRM is the third bounded context to land after Identity (v0.2) and Platform (Phase 2 first slice). It owns the tenant-side workflow that converts a purchased lead into either a converted customer or a documented loss. Per BRD §6.3 the CRM module owns four aggregates over time — `CrmLead`, `CallLog`, `Reminder`, `AssignmentHistory`. Slice 1 ships three of the four (Reminder defers to Slice 2 — Notifications module will eventually own the cron/delivery side, CRM only emits the reminder-requested signal).

Inputs the module consumes from elsewhere:

- `platform.lead-purchased.v1` (Platform-emitted, frozen contract) — every tenant lead originates here. Each purchase is final + irrevocable per BRD §4.2.
- `identity.tenant_suspended.v1` (Identity-emitted) — CRM SHOULD pause lead operations downstream of this; slice 1 records the signal but doesn't enforce.
- `identity.membership_deactivated.v1` (Identity-emitted) — drives the eventual auto-reassignment workflow (slice 2+).

Constraints inherited from preceding ADRs:

- **ADR 0001** modular monolith. CRM lives at `internal/crm/...` as a sibling of `internal/identity/`. NO cross-module imports of another module's `domain/`, `app/`, `ports/`, `adapters/` — only Watermill integration events.
- **ADR 0002** hexagonal + DDD. Aggregates carry invariants + emit domain events; repositories live behind interfaces in `domain/`.
- **ADR 0003** state-based persistence. No event sourcing — `crm.crm_leads` is a row table; lifecycle transitions update columns + emit integration events.
- **ADR 0004 / 0037** sqlc + pgx + squirrel + goose; sqlc-generated code lives in its own `internal/crm/adapters/db` subpackage.
- **ADR 0006** Postgres RLS + `SET LOCAL app.tenant_id`. Every CRM table tenant-scoped + RLS+FORCE.
- **ADR 0007** stdlib `net/http` ServeMux 1.22+.
- **ADR 0027 / 0008** outbox-as-audit + Watermill SQL forwarder. The module owns its own `crm.outbox` table.
- **ADR 0036** closed-set permission catalog. CRM extends `IdentityPermissions` with a `Crm` namespace (Slice 1 introduces 4 names; the cross-module catalog lives in the identity package because the permission resolver + JWT-claim wiring lives there).
- **ADR 0038** keyset pagination. The lead-list endpoint uses `(created_at DESC, id DESC)` as the default sort tuple, with composite partial indexes covering `(tenant_id, stage)` and `(tenant_id, assignee_membership_id)`.
- **ADR 0040** pg_trgm now, FTS later. Slice 1 ships exact filters + GIN on text[]; full-text search on `contact_name` is a Slice 2 follow-up.
- **ADR 0041** CQRS read models via subscribers. CRM read model (denormalised `search_leads_view`) is Slice 2.
- **ADR 0044** enumeration-safe 404. Lead IDs are UUIDv7 (non-guessable) so 404 vs 403 is a non-issue at slice 1.
- **ADR 0046** spec-first OpenAPI. Every Slice 1 route ships in `api/openapi.yaml` under the `CRM` tag.
- **ADR 0047** layer-boundary discipline. CRM `app/` MAY NOT import `adapters/db`, `pgx`, `pgxpool`, `pgtype`, or `adapters/`. Arch test mirrors the identity-side gate.
- **ADR 0049** URL design + route-arch test. New `internal/crm/ports/route_registration_test.go` asserts the mux registers without pattern conflicts. `internal/crm/ports/route_spec_test.go` asserts every `mux.Handle` has a matching operation in `api/openapi.yaml`.
- **ADR 0050** OpenAPI as code-of-record. Spec drift gate (per-module mirror of the identity test) protects the CRM surface.
- **ADR 0056** impersonation context propagation. The CRM outbox writer reads the same `actclaim.FromContext(ctx)` accessor identity uses; the act_* columns propagate identically.

Non-goals for Slice 1:

- **Reminder aggregate + 3-month mature-lead job** — Reminders belong to Notifications module per BRD §4.6 / §4.7. Slice 2 emits a `crm.reminder-requested.v1` event from the CallLog handler when a callback window is set; Notifications hosts the scheduler.
- **AssignmentHistory query endpoints** — Slice 1 writes the audit log only. Current assignee is exposed on `CrmLead.AssigneeMembershipID`; full history reads land in Slice 2 once UX demands "who owned this lead in Q3".
- **Bulk reassignment / round-robin auto-assign** — Slice 2.
- **Search projection (`search_leads_view`)** — Slice 2 per ADR 0041; Slice 1 lives off the live `crm.crm_leads` table.
- **Order conversion side-effects** — Slice 1 emits `CrmLeadConvertedV1` but does NOT mutate Orders. Orders module consumes the event when it lands; until then the event is published + consumed by no one (harmless — the outbox forwarder still drains it).

## Decision

### Aggregate boundaries — three, not one

Three aggregates instead of one fat `CrmLead`:

| Aggregate | Owns | Rationale |
|---|---|---|
| `CrmLead` | Lead profile + stage + temperature + current assignee | Single source of truth for "what is this lead's state right now". Reads dominate writes; profile fields change rarely. |
| `CallLog` | Per-call record (outcome, notes, actor, occurred-at) | Append-only — never updated. Lead-bound but independent: a lead with 50 calls has 50 rows, not one fat aggregate. |
| `AssignmentHistory` | Append-only assignment audit | Same reason as CallLog — append-only + queryable independent of the lead aggregate. The latest entry IS the current assignee. |

Why three vs one:

- **Lock contention**: collapsing CallLog into CrmLead means every call-logged event acquires the lead's row-lock, serialising teams' calling activity. Splitting lets one team member call while another logs an interaction in parallel.
- **Aggregate-size discipline** (kgrzybek + Khorikov canon): `CrmLead` stays under ~250 lines / 7 fields. Bundling all activity history would push it past every sniff-test threshold.
- **Vernon ch.10**: "small aggregates, eventually consistent between aggregates". Cross-aggregate writes use the outbox + a UoW or sequential ops — Slice 1 keeps each command single-aggregate to avoid premature UoW complexity.

CallLog + AssignmentHistory are append-only — they have an `Add(ctx, x)` method but NO `UpdateByID`. Repository contract is intentionally minimal.

### Stage state machine + independent temperature axis

Per BRD §4.4 the lead lifecycle is:

```
New → Contacted → Interested → Negotiation → Converted (terminal)
                                          ↘ Lost      (terminal)
```

**Independent** temperature axis:

```
Hot | Warm | Cold | Dead
```

Validation rules:

- The state machine is strict (no skips, no backtracking). `New → Negotiation` rejected; `Converted → New` rejected. Idempotent on self-transitions (`Contacted → Contacted` is a no-op, returns nil, emits no event).
- Terminal states (`Converted`, `Lost`) accept NO outgoing transitions of any kind. Attempts return `ErrInvalid`.
- Temperature is independent — `Hot → Cold` is valid at any non-terminal stage. Terminal stages still freeze temperature too (a converted lead's temperature stops mattering).
- `Dead` temperature does NOT auto-Lose the lead — a sales executive may mark dead temperature WITHOUT closing the lead (e.g. "lost contact for now, may revive in Q4"). Auto-lose is product UX layered on top.

The two axes live in separate state-mutation methods on the aggregate (`ChangeStage`, `ChangeTemperature`) emitting separate events. Mirrors the .NET parent's `LeadStageChanged` / `LeadTemperatureChanged` event split.

### Idempotency model for the lead-purchased subscriber — `purchase_id` natural key

Per BRD §4.2 every lead purchase is final + irrevocable. The Platform emits `platform.lead-purchased.v1` with a UUIDv7 `PurchaseID`. CRM's subscriber MUST be safely retriable per ADR 0008 at-least-once delivery semantics.

Two layers of dedup:

1. **Watermill `IdempotentReceiver`** dedupes by message ID — handles outbox-forwarder-side retries. Production canon, already wired per `internal/common/messaging`.
2. **Natural-key dedup at the aggregate layer** — `crm.crm_leads.source_purchase_id` is `uuid` + `UNIQUE` (NULL-safe; multi-source leads might omit it but at slice 1 every lead has one). The subscriber's flow is:
   - Decode the V1 envelope.
   - Check `repo.GetBySourcePurchaseID(ctx, purchaseID)` — if a CrmLead already exists with this purchase ID, return nil (no-op).
   - Otherwise mint a fresh `CrmLead` from the snapshot + persist.

The natural-key layer survives broker replays AND outright outbox-stream-replay scenarios (e.g. cold rebuild of the read model). The IdempotentReceiver layer alone wouldn't survive because its dedup table is process-local; the natural key is durable + universal.

`PurchaseID` is the right natural key (not `PlatformLeadID`) because a tenant could in principle re-purchase the same lead later (different purchase, same source profile). Each purchase is its own CrmLead row; the natural-key is the **commercial transaction** not the **source profile**.

### Filter-index strategy

BRD §6.3 lists the indexed columns. Slice 1 indexes:

| Index | Type | Purpose |
|---|---|---|
| `idx_crm_leads_tenant_stage_created` | btree composite | `WHERE tenant_id=? AND stage=? ORDER BY (created_at, id) DESC` — default list + filter-by-stage |
| `idx_crm_leads_tenant_assignee_created` | btree composite | `WHERE tenant_id=? AND assignee_membership_id=? ORDER BY (created_at, id) DESC` — "my leads" view |
| `idx_crm_leads_tenant_temperature` | btree partial | filter-by-temperature; partial on temperature != 'dead' for the dashboard hot-list |
| `idx_crm_leads_tenant_pincode` | btree | geographic filter |
| `idx_crm_leads_tenant_business_type` | btree partial | filter-by-business; partial on business_type IS NOT NULL |
| `idx_crm_leads_product_ranges_gin` | GIN on `text[]` | multi-select product range filter |
| `idx_crm_leads_dosage_forms_gin` | GIN on `text[]` | multi-select dosage form filter |
| `idx_crm_leads_purchase_id` | UNIQUE | idempotency lookup for the subscriber |
| `idx_crm_leads_contact_name_trgm` | GIN trgm | name search (ADR 0040; slice 1 supports a `name` filter, full-text comes in slice 2) |

Discipline mirrors ADR 0038: the list endpoint's EXPLAIN-under-RLS test (planned for Slice 2 once we have realistic row counts) asserts the planner picks `idx_crm_leads_tenant_stage_created` / `_assignee_created` instead of a seq-scan.

Non-filterable supplementary fields (per BRD §6.3) land in JSONB `extra_profile`: `street`, `gst_number`, `pan_number`, `email`, `notes`, `has_pan`. The JSONB rule (database.md): never in WHERE clauses — filters go on dedicated indexed columns.

### Permissions added

Per the slice brief, four new permission constants extend `IdentityPermissions.Crm` in `internal/identity/domain/permission/`:

| Permission | Granted to (default seed) | Purpose |
|---|---|---|
| `crm.leads.read` | every tenant sales role | list + get lead |
| `crm.leads.assign` | SalesManager, CompanyOwner, OfficeAdministrator | manual reassignment |
| `crm.leads.manage` | SalesExecutive (when assigned), SalesManager, CompanyOwner | stage/temperature/call/convert/lose |
| `crm.leads.read_all` | SalesManager, CompanyOwner | overrides "only my assigned leads" filter |

`Crm.Leads.Manage` is granted on a *role* basis at v0.2 (no per-lead ACL); BRD §4.9 calls for "Sales Manager or above can reassign" + "assigned executive sees complete lead history". Per-membership lead-ownership gating (sales exec sees only their own unless ReadAll) is enforced HANDLER-side via JWT inspection — the middleware grants the route, the handler filters the query.

`Crm.Leads.ReadAll` is the FAANG-canon shape (Auth0 "read:resources_any" vs "read:resources_mine"): explicit additive permission, no inheritance machinery.

### Integration events emitted

Per the slice brief. Wire alias: `crm.{event}.v1`. All seven Slice 1 events are `TenantScoped`. Each event carries `LeadID` + `TenantID` + actor membership ID + occurred-at.

The mapping from domain events to integration events lives in `internal/crm/integrationevents/mapping.go` — sibling to the identity-side `FromDomainEvent`. The CRM mapper handles ONLY CRM domain types; identity-emitted events the CRM module CONSUMES come in already in V1 wire shape via the inbox.

The repository's `Add` / `UpdateByID` methods drain `agg.PullEvents()` → `mapDomainEvents` → `writeOutboxEvents(ctx, tx, crm.outbox)`. Same shape as identity.

### CrmLead `source_purchase_id` is nullable

CrmLeads created via the lead-purchased subscriber have a non-null `source_purchase_id`. Future paths (manual import, bulk upload — slice 2+) may insert leads without one. The column is `uuid NULL UNIQUE`. NULL allows multiple rows; the unique constraint applies only when populated. Postgres handles this natively (partial uniqueness on `WHERE source_purchase_id IS NOT NULL`).

## Consequences

**Positive:**

- Three-aggregate split contains row-lock pressure + keeps each aggregate inside the kgrzybek size budget.
- Natural-key idempotency (PurchaseID UNIQUE) survives any retry scenario — broker replay, cold-rebuild, manual re-trigger.
- Stage state machine + independent temperature axis matches the .NET parent's vocabulary exactly + matches the BRD's product-side promise.
- Permission catalog extension via a fresh `IdentityPermissions.Crm` namespace stays additive — no risk to identity v0.2 shape.
- Slice 1 ships the complete intake-to-disposition flow. Sales executives can do real work the day after merge.

**Negative:**

- Three aggregates means three repository interfaces + three pg adapters — more boilerplate than a fat aggregate would have. Mitigated: append-only repos only need `Add`, not `UpdateByID`.
- Per-handler "is this lead assigned to me" gating runs in the query handler instead of in middleware — slightly more code per endpoint than a single `RequirePermission` line. Mitigated: the helper lives in `app/command/` and is called once per mutating handler.
- CrmLead JSONB `extra_profile` is queryable (`?>>` operators work) but the discipline rule bans it. A future engineer who tries to add a WHERE clause on `extra_profile->>'email'` will be caught at review + EXPLAIN test.

## Alternatives considered

1. **One fat `CrmLead` aggregate carrying CallLogs + AssignmentHistory.** Rejected for the Vernon ch.10 reasons + the row-lock contention. The .NET parent's BRD §6.3 explicitly lists the four aggregates separately; the Go port preserves the split.

2. **Use `PlatformLeadID` as the natural idempotency key.** Rejected. A tenant CAN purchase the same lead twice over time (different commercial transactions, same source profile). `PurchaseID` is the commercial primitive, `PlatformLeadID` is the source-data primitive. Stripe payment-intent / checkout-session canon: dedup on the transaction, not on the resource.

3. **State machine via a `lead.transition(from, to)` method.** Rejected — caller has to KNOW the current state to call it, which is a layering smell. The aggregate-method shape (`Contact()`, `Interest()`, `EnterNegotiation()`, `Convert()`, `Lose(reason)`) reads as ubiquitous-language verbs + the aggregate enforces its own current-state invariant. Matches the .NET parent + Vernon IDDD ch.7 canon.

4. **One reusable "lifecycle" aggregate generic in state-machine type.** Rejected as a v0.4+ refactor candidate when we have 4-5 aggregates with similar shapes. Premature now.

5. **Per-lead ACL table for fine-grained ownership.** Rejected. BRD §6.3 calls for role-based access; per-lead ACL is a future product feature, not v0.2. The handler-side "filter by assignee if not ReadAll" pattern matches Auth0 / Stripe + costs ~10 lines.

6. **Reminders aggregate inside CRM.** Rejected per BRD §6.6 (Reminders are a Notifications-module concern). CRM is the EMITTER of "I want a reminder for this lead at this time" (slice 2 event); Notifications hosts the cron + delivery. Mixing them duplicates state.

## Sources

- ADR 0001 (modular monolith); ADR 0002 (hexagonal + DDD); ADR 0008 (Watermill + outbox); ADR 0036 (permission catalog); ADR 0038 (keyset pagination); ADR 0041 (CQRS read models); ADR 0044 (enum safety); ADR 0046 (spec-first OpenAPI); ADR 0047 (layer-boundary); ADR 0049 (URL + route arch); ADR 0050 (OpenAPI as code-of-record); ADR 0056 (impersonation propagation).
- BRD §4.1 (lead definition), §4.2 (purchase irreversibility), §4.4 (stages + temperature), §4.9 (reassignment), §6.3 (CRM module).
- Vernon, *Implementing Domain-Driven Design* ch.7 (entities + invariants) + ch.10 (aggregate size discipline).
- Kamil Grzybek, *Modular Monolith with DDD* — `Meetings` aggregate as the size-discipline reference (~350 lines).
- Khorikov, *Pragmatic Clean Architecture* — application layer as pure orchestration; the per-handler-filter pattern for sales-exec "see only my assigned".
- Stripe API canon — natural-key idempotency on commercial transactions (`payment_intent_id`, not `customer_id`).
- LeadKart .NET — `LeadKart.Modules.Crm.IntegrationEvents.CrmIntegrationEvents` (`crm.lead-converted.v1`, `crm.lead-assigned.v1`, etc.) — Go port preserves wire-alias compatibility.
