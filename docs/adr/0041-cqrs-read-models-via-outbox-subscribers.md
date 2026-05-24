# ADR 0041 — CQRS read models via outbox subscribers

**Status:** Accepted
**Date:** 2026-05-18

## Context

LeadKart's write model is a normalized DDD aggregate hierarchy (Tenant, Person, Membership, Role, …) persisted under RLS-scoped Postgres. This shape is correct for the write path — it enforces invariants, captures domain events, and keeps the schema readable.

It's progressively wrong for the read path as we add more search-shaped surfaces:

- **Per-tenant user list with search** needs `tenant_memberships` joined with `persons` and `roles` and `permissions` and `role_assignments` — 5 tables for one row in the UI list.
- **Cross-tenant operator search** runs cross-tenant queries with `app.is_platform()` bypass; the same JOIN explodes by a factor of N tenants.
- **Audit log read** queries the outbox + the audit_log_entry tables with filters on action + tenant + person + occurred_at — composite indexes work but the projection still happens per query.
- **Phase 2 lead search** will need leads joined with persons (creator) + memberships (assigned-to) + products + status history — 4-5 tables for one row in a list.

Computing this denormalization on every read is fine at v0.2 scale (1 tenant, 10s of rows per resource). It becomes the dominant cost path by Phase 2 when leads land. The naive answer is "add more indexes"; the canon answer is "maintain a search-shaped read model maintained eventually-consistently from the write model".

LeadKart already has the substrate for the canon answer:

- **Outbox pattern** ([ADR 0008](0008-messaging-watermill.md), [ADR 0027](0027-audit-log-outbox-doubles-as-audit.md)) — every aggregate write writes both state AND a domain event to `identity.outbox` in the same transaction. Watermill SQL forwarder publishes these to the message bus.
- **Watermill subscribers** — multiple subscribers can consume the same event stream and project into different read models. Already used for the integration-event publisher.

This ADR formalises **using the existing outbox + subscriber substrate to maintain CQRS read models**, codifies the patterns + pitfalls, and seeds the precedent for Phase 2+ projections.

Constraints inherited from preceding ADRs:

- ADR 0001 — modular monolith; CQRS happens within a module, not across modules.
- ADR 0002 — domain layer is framework-free; read models live in `adapters/` per module.
- ADR 0003 — state-based persistence; no event sourcing. Read models can be rebuilt from current write-model state on demand if the projection drifts.
- ADR 0008 — Watermill SQL outbox + forwarder.
- ADR 0027 — outbox doubles as audit log; same table feeds projections.

Non-goals:

- Event sourcing. We rebuild read models from the *current* write-model state, not from a replayable event log.
- Cross-module read models. Each module owns its read models; the leads-search projection is in the CRM module, not in a shared kit.
- Strongly-consistent reads (read-your-writes after a mutation). Read models are eventually-consistent with the write model; the lag is single-digit milliseconds at normal load but can grow under outbox-forwarder backpressure.

## Decision

**Maintain denormalized read-model tables via Watermill subscribers consuming the outbox event stream. Each module owns its projections. Reconciliation jobs catch drift.**

### The pattern

```
Write side (existing)                        Read side (this ADR)
──────────────────                          ───────────────────────
                                            
┌──────────────┐                            ┌─────────────────────────────┐
│ leads        │  ───┐                      │ search_leads_view           │
│ (write model)│     │ same-tx INSERT       │ (read model)                │
└──────────────┘     │                      │                             │
                     ▼                      │  - denormalized columns     │
                ┌─────────┐                 │  - flattened FKs            │
                │ outbox  │                 │  - search-shaped indexes    │
                └─────────┘                 │  - RLS+FORCE if tenant-     │
                     │                      │    scoped                   │
                     │ poll + publish       └─────────────────────────────┘
                     ▼                                 ▲
                ┌─────────┐                            │
                │ Kafka / │  ────► Subscriber ────► UPSERT
                │watermill│        (Watermill          / DELETE
                │  bus    │         handler)
                └─────────┘
```

### Per-module file layout

```
internal/crm/
├── domain/lead/                                # write model — Lead aggregate
├── adapters/
│   ├── lead_repository_pg.go                   # writes leads + outbox
│   └── search_lead_view_pg.go                  # writes to search_leads_view from subscriber
├── ports/
│   └── subscribers/
│       └── search_lead_projector.go            # Watermill handler — consumes Lead.* events
└── app/
    └── query/
        └── search_leads.go                     # reads from search_leads_view
```

The projector is a **port** (inbound concrete impl per TDL canon) — it consumes from the bus. The write to the read-model table goes through an **adapter** (outbound). The query reads from the same table via the query handler.

### Schema rules for read-model tables

1. **One row per "thing the UI shows in a list cell".** Don't sub-row; pre-flatten.
2. **Searchable text in one or more `lower(...)`-applied columns** with the appropriate trgm/FTS index (per [ADR 0040](0040-search-strategy-pg-trgm-now-fts-later.md)).
3. **Composite indexes matching the query shape.** Sort tuples for cursor pagination, filter columns for facets.
4. **RLS+FORCE if the source data is tenant-scoped.** Same policy shape as the write-model table (`USING (tenant_id = app.current_tenant() OR app.is_platform())`).
5. **`projected_at timestamptz NOT NULL DEFAULT now()`** column. Used by reconciliation to detect stale rows.
6. **Idempotent UPSERT.** PRIMARY KEY = write-model's aggregate ID; subscriber writes `INSERT ... ON CONFLICT (id) DO UPDATE`. Replays don't corrupt.

### Subscriber handler shape

```go
type SearchLeadProjector struct {
    db *pgxpool.Pool
}

func (p *SearchLeadProjector) HandleLeadCreated(ctx context.Context, e LeadCreatedV1) error {
    // UPSERT into search_leads_view; idempotent via PK
    // Compose searchable_text from event payload + joined lookups
    // Set projected_at = now()
}

func (p *SearchLeadProjector) HandleLeadStatusChanged(ctx context.Context, e LeadStatusChangedV1) error {
    // UPDATE search_leads_view SET status = ..., last_activity_at = ..., projected_at = now() WHERE lead_id = ...
}

func (p *SearchLeadProjector) HandleAssignmentChanged(ctx context.Context, e LeadAssignedV1) error {
    // UPDATE search_leads_view SET assigned_to_name = (lookup membership/person), projected_at = now()
}

func (p *SearchLeadProjector) HandleLeadDeleted(ctx context.Context, e LeadDeletedV1) error {
    // DELETE FROM search_leads_view WHERE lead_id = ...
}
```

Each handler is **independently idempotent** — replays during outbox forwarder retries don't corrupt. Watermill's `IdempotentReceiver` middleware adds inbox-side dedup as belt-and-braces.

### Reconciliation job

A river-scheduled job runs every 6 hours per module per read-model:

```go
func (j *ReconcileSearchLeadsView) Work(ctx context.Context, _ *river.Job[ReconcileSearchLeadsViewArgs]) error {
    // 1. Row-count compare: SELECT count(*) FROM leads vs FROM search_leads_view
    // 2. Hash-compare: SELECT id, status, assigned_to_id, last_activity_at FROM leads
    //                  vs corresponding columns in search_leads_view
    // 3. For each drift row: emit a synthetic Lead.Reproject command
    //    (re-runs the projector against current state)
    // 4. Page on drift > threshold (drift = bug; investigate)
}
```

Drift is rare (subscriber handlers should be deterministic) but possible from:

- Subscriber bug that silently dropped an event
- Schema mismatch after a write-model migration without a corresponding projector update
- Outbox forwarder permanently failing on a poison message

Reconciliation catches all three.

### Eventual-consistency guarantees

| Property | Read model |
|---|---|
| Lag from write commit to read-model update | **~10-100ms typical** (outbox poll + Kafka hop + subscriber UPSERT) |
| Lag under outbox backpressure | up to **several seconds** if forwarder lag accumulates |
| Read-your-writes after a mutation | **NOT GUARANTEED.** UI must either refetch from write-model on critical reads OR tolerate stale projection until next subscriber cycle |
| Recovery from full read-model loss | rebuildable via reconciliation job (above) |

The "no read-your-writes" property is fine for search/list UIs; it's not fine for "show me the form for the thing I just created". Each handler decides which model to query: write-model for read-after-write; read-model for search/list/dashboard.

## Consequences

**Positive:**

- **Search-shaped reads are cheap.** A `WHERE searchable_text ILIKE '%foo%' AND tenant_id = $1 ORDER BY last_activity_at DESC LIMIT 50` against a flat denormalized table beats a 4-table JOIN by an order of magnitude at any non-trivial row count.
- **Write-model purity preserved.** The Lead aggregate doesn't acquire denormalized projection columns or search-vector fields. Domain stays clean per ADR 0002.
- **Pluggable read models.** A new search projection (e.g. operator dashboard counts) is a new subscriber + new table — doesn't touch the write model.
- **Reconciliation gives a safety net.** Subscribers are eventually-consistent; reconciliation closes the loop without forcing strong consistency at write time.
- **Standard pattern across modules.** Phase 2 leads, Phase 3 orders, Phase 4 inventory all follow the same shape. Onboarding new modules becomes mechanical.

**Negative:**

- **More tables.** Each module gets one or more `*_view` tables alongside the write model. Disk + backup surface grows.
- **Schema migrations span both models.** Adding a column to the Lead aggregate AND wanting it searchable means: write-model migration + read-model migration + subscriber handler update + reconciliation field update. Disciplined but not free.
- **Read-after-write surprises.** Frontend developers will eventually hit the eventual-consistency edge. Mitigation: document which queries hit which model; add request-side fallback ("if not found in projection, fall back to write-model query").
- **Cross-module event coupling.** If a leads-search projection needs the `Person.first_name` (cross-module — Person lives in Identity), the projector subscribes to BOTH `Lead.*` AND `Person.*` events. This is fine per ADR 0001 (modules communicate via events, not direct DB access) but creates implicit coupling that needs tracking.
- **Subscriber backpressure = stale dashboards.** If a poison message blocks the forwarder, dashboards drift. River + Watermill have backpressure visibility built in; just need to make sure dashboards / alerts surface it.

## Alternatives considered

1. **Materialized views maintained by Postgres.** Considered. Rejected because:
   - `REFRESH MATERIALIZED VIEW CONCURRENTLY` requires a unique index + full rescan; expensive at meaningful scale.
   - No incremental update path; every refresh is a full recompute of the underlying JOIN.
   - Schema migrations on the underlying tables sometimes invalidate the MV definition silently.
   - Watermill subscriber path gives incremental updates with the same locality (same Postgres) at a fraction of the cost.

2. **Application-level caching (Redis HybridCache for list queries).** Considered. Rejected as a primary mechanism because:
   - Cache invalidation is hard; "which list query results need flushing when Lead.{id} changes?" is a tag-cache problem the projector pattern sidesteps.
   - Lists with filters / sorts / cursor pagination don't cache cleanly — each combination is a separate key.
   - Caching is the *right* layer for `/v1/platform/stats` (whole-response cache) but the wrong layer for filtered list endpoints.
   - Caches can complement the projector pattern (cache the projected query results) but not replace it.

3. **Hand-maintain denormalized columns on the write-model table.** Considered. Rejected because:
   - Adds search-shaped state to the aggregate; pollutes the domain per ADR 0002.
   - Write-side transactions get bigger (more columns to update on every state change).
   - Reconciliation surface still exists; just hidden inside the aggregate's persistence path.

4. **Event sourcing the whole thing.** Considered (and revisited at every architecture review). Rejected per ADR 0035 — event sourcing scope is zero modules at v0.1 + the current state-based persistence + outbox model gives us 90% of the audit/replay benefits at 30% of the operational cost. Read-model projectors are the *part* of event sourcing that pays rent without the rest.

5. **Pull-based projection (cron job rebuilds the view from the write model nightly).** Considered. Rejected because:
   - Stale by up to 24h. Operator dashboards need fresher data.
   - Full-rebuild cost grows linearly with write-model size; unsustainable past Phase 2 lead volumes.
   - Reconciliation already handles the "rebuild on drift" path; doing it nightly as the primary mechanism is wasteful.

## Sources

- [ThreeDotsLabs — "Wild Workouts: Going Serverless with Go on Google Cloud" (read models chapter)](https://threedots.tech/post/serverless-cloud-run-firebase-modern-go-application/) — TDL canonical projection pattern.
- [Martin Fowler — CQRS](https://martinfowler.com/bliki/CQRS.html) — pattern foundation.
- [Brandur Leach — "Transactionally Staged Job Drains in Postgres"](https://brandur.org/job-drain) — outbox + subscriber substrate that this ADR builds on.
- [Linear engineering — "Scaling the Linear Sync Engine"](https://linear.app/blog/scaling-the-linear-sync-engine) — concrete production CQRS example at small-medium SaaS scale.
- ADR 0001 — Modular monolith (module-internal CQRS, no cross-module).
- ADR 0003 — State-based persistence + outbox (we rebuild, not replay).
- ADR 0008 — Watermill SQL outbox + forwarder (substrate).
- ADR 0027 — Outbox doubles as audit log (same events feed projections).
- ADR 0035 — Event sourcing scope = zero modules at v0.1 (state-based, not ES).
- ADR 0040 — Search strategy: pg_trgm now, FTS later (read-model indexes follow this).


**Fitness function:** convention-only — not mechanically expressible. Pattern observed in the subscriber files under `ports/subscribers/`; behaviour exercised by integration tests.
