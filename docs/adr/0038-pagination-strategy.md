# ADR 0038 — Pagination strategy: cursor (keyset) over offset

**Status:** Accepted
**Date:** 2026-05-18

## Context

Phase 1 shipped every `List*` endpoint returning the full result set. The deferred-pagination decision was documented inline at [internal/identity/app/query/platform.go:140](../../internal/identity/app/query/platform.go) ("No filtering knobs at v0.2 — the result set is small enough … for client-side pagination. Add server-side filtering when a real customer asks.") — explicitly YAGNI for v0.2.

Phase 1.5's unified-surface spec (per the frontend backend wishlist + the operator/tenant-admin path consolidation) raises the realistic ceiling: per-tenant user lists (`GET /v1/users` and `GET /v1/users` operator view) can return hundreds of rows once a pharma tenant reaches 100+ employees, and Phase 2 will introduce `crm.leads` which is intrinsically unbounded per tenant.

Constraints inherited from preceding ADRs:

- ADR 0004 — sqlc + pgx/v5 + squirrel + goose. Pagination must work with sqlc-generated query types; no ORM-level abstractions.
- ADR 0006 — Postgres RLS + SET LOCAL. Every paginated query runs under either `TxScopeTenant` (filtered by `app.tenant_id` GUC) or `TxScopePlatform` (operator bypass). Cursor predicate must compose with RLS without forcing a seq-scan.
- ADR 0007 — stdlib `net/http` ServeMux. Pagination is wired through query params + response body; no framework-level pagination middleware.
- CLAUDE.md "no premature abstraction" — only paginate endpoints where the result set realistically grows.

Non-goals for v0.2:

- Total-count exposure (`X-Total-Count` header, `total: N` in response). Computing total requires a separate `COUNT(*)` query per page — O(n) seq-scan that doesn't pay rent. Stripe drops totals entirely; GitHub provides them via a separate Link header but on-demand only.
- Bidirectional pagination (previous-page navigation). Forward-only matches Google AIP-158 + simplifies cursor encoding. Add reverse direction later only if a UX demands it (Stripe added it years after launch).
- Server-driven sort customisation. v0.2 sort tuples are baked into each endpoint's query; `?sort=` knob is a Phase 2+ ask.

## Decision

**Keyset (cursor) pagination on a strictly-monotonic tuple, with the cursor encoded as an opaque base64 token. No total counts. Forward-only.**

### The query shape

Every paginated query follows this skeleton:

```sql
SELECT <columns>
FROM   <table>
WHERE  <RLS-compatible filter (e.g. tenant_id, status='active')>
AND    (sort_col, id) < ($cursor_sort_val::<type>, $cursor_id::uuid)
ORDER  BY sort_col DESC, id DESC
LIMIT  $page_size + 1
```

Notes:

- **Tuple comparison `(sort_col, id) < (a, b)`** is the canonical keyset predicate (Markus Winand "Use the Index, Luke" / Brandur Leach river-queue canon). The tiebreaker `id` is necessary because `sort_col` alone isn't unique — two rows at the same `joined_at` would cause page-boundary skips or duplicates.
- **`LIMIT $page_size + 1`** is the standard "peek one extra row" trick: if you got `page_size + 1` rows back, there's more — emit `has_more: true` + set `next_cursor` from the `page_size`-th row (drop the extra). If you got `≤ page_size` rows, you're on the last page — `has_more: false`.
- **`ORDER BY ... DESC` matches the cursor direction.** Reverse-chronological ("newest first") is the universal dashboard default; if a specific endpoint wants chronological, flip both the predicate sign and the ORDER direction.

### The cursor

```go
type Cursor struct {
    SortValue any    // typed per-endpoint; usually time.Time
    ID        string // tiebreaker; always UUIDv7 string
}
```

Encoded as **base64(JSON)** for the wire:

```
?cursor=eyJzIjoiMjAyNi0wNS0xMFQxMjozNDo1NiIsImkiOiIwMTkyMzRhYi0uLi4ifQ
```

- **Base64 wrapping** keeps the wire-format opaque — clients treat it as a blob and must not introspect.
- **JSON inside** keeps it debuggable — we can decode in logs / dev tools when triaging.
- **Per-endpoint cursor shape may extend** with extra fields (e.g. a `direction` byte for bidirectional, a `filter_hash` for "cursor only valid against the same filter set"). The opaque wrapper means clients won't break.

### Page size

```
?page_size=50    # caller-supplied, optional
```

| Constant | Value | Source |
|---|---|---|
| `DefaultPageSize` | **50** | Matches dashboard-grid UX; Stripe = 10, GitHub = 30, Google AIP = 50 |
| `MaxPageSize`     | **200** | Stripe = 100, GitHub = 100, AIP = unspecified. Higher cap pays for fewer round-trips on bulk-export-style usage |
| `MinPageSize`     | **1** | Defensive — a `?page_size=0` request gets coerced to default, not zero |

The clamp helper rejects negative values, coerces zero to `DefaultPageSize`, and caps at `MaxPageSize`.

### Index discipline

**Every paginated query has a covering composite index matching the predicate.** Example for `tenant_memberships`:

```sql
CREATE INDEX idx_memberships_tenant_active_joined
    ON identity.tenant_memberships (tenant_id, joined_at DESC, id DESC)
    WHERE status = 'active';
```

Partial index (`WHERE status = 'active'`) keeps the btree small. Composite ordering matches the query's `WHERE tenant_id = $1 AND status = 'active' AND (joined_at, id) < (...)` predicate so the planner emits an Index Scan, not a Seq Scan + Filter.

**Mandatory integration test** for every paginated endpoint: load realistic row counts under RLS, run `EXPLAIN (ANALYZE, BUFFERS)`, assert the plan starts with `Index Scan using <expected_index>`. Add to CI. Without this, the planner can silently fall back to seq-scan and nobody notices until production p95 latency rises.

### Response shape

```json
{
  "items": [ {...}, {...} ],
  "has_more": true,
  "next_cursor": "eyJzIjoi..."
}
```

- **`has_more: true` + `next_cursor: "..."`** when there's another page
- **`has_more: false` + `next_cursor: ""` (or omitted via `omitempty`)** when this is the last page
- **NO `total`, `total_count`, `total_pages`, `page_number`** — see non-goals

### Per-resource pagination tuple

Each endpoint declares its sort tuple. Initial tuples for the realistic paginated surfaces:

| Endpoint | Sort tuple | Index |
|---|---|---|
| `GET /v1/users` | `(joined_at DESC, id DESC)` | `idx_memberships_tenant_active_joined` (partial: `status='active'`) |
| `GET /v1/tenants` (Platform list) | `(created_at DESC, id DESC)` | New partial index on `(created_at DESC, id) WHERE status != 'hard_deleted'` |
| `GET /v1/audit/events` | `(occurred_at_utc DESC, id DESC)` | `idx_audit_*_occurred` composites per filter shape |
| `GET /v1/leads` (Phase 2) | `(last_activity_at DESC, id DESC)` | Built into `crm.search_leads_view` projection (ADR 0041) |

## Consequences

**Positive:**

- **O(log n) keyset scan vs O(n) offset scan.** At 1M rows, offset-pagination at page 1000 takes ~3-5s; keyset stays at ~20ms. The break-even is around 10K rows; past that, offset is increasingly painful. Indian PCD pharma tenants with 10K+ leads will hit this within the year.
- **Stable under concurrent inserts.** Each cursor points at a specific row's sort tuple; new rows arriving between requests appear on page 1 of the next request (not as duplicates or skips in mid-walk). Offset-pagination is famously unstable here.
- **Cache-friendly cursor.** The `next_cursor` is a deterministic function of (sort_value, id) — same cursor returns same page (modulo new inserts). CDN/HTTP cache by URL + cursor query param works correctly.
- **Forward-compatible.** Cursor opacity means we can add fields (direction, filter_hash, snapshot timestamp) without breaking clients.

**Negative:**

- **No jump-to-page-N.** Cursor walks are sequential; users can't bookmark "page 17 of the user list". Acceptable trade-off — most modern dashboards don't expose page numbers, they expose "load more" / infinite scroll. Stripe Dashboard / Linear / Notion all use cursor; nobody complains.
- **Cursor encodes assumptions.** Changing a query's `ORDER BY` invalidates all in-flight cursors held by clients. Mitigation: cursor format version byte; reject incompatible cursors with 400 + a hint to refresh.
- **Index discipline cost.** Every paginated query needs a matching composite index; reviewers need to verify EXPLAIN under RLS. Mandatory integration test (above) makes this a CI gate, not a memory test.
- **Cursor exposes sort key in opaque blob.** A motivated client could base64-decode and see the encoded `joined_at` + `id`. Not a security issue (the data is already returned to that client in the row payload) but worth noting that the cursor is debuggable both for us and for clients.

## Alternatives considered

1. **Offset pagination (`?offset=N&limit=50`).** Rejected. O(n) skip cost; unstable under concurrent inserts; recognized antipattern in `pg_stat_statements` audits. Works fine at < 10K rows but doesn't scale.

2. **Cursor over a single `id` column (no sort-tuple).** Rejected. UUIDv7 is time-ordered so `id` alone is monotonic in insert order — but most dashboards want a domain sort (joined_at, last_activity_at, severity, etc.), not insert order. Single-id cursor would force "always sort by id" which is wrong UX. Tuple cursor handles both cases.

3. **Bidirectional cursors with prev/next.** Rejected for v0.2 per non-goals. Adds a direction byte to the cursor + reverses ORDER BY; doable later without breaking the wire format thanks to opaque encoding.

4. **Page-token cursors per Google AIP-158 vs cursor-style per Stripe.** Cosmetic difference; both encode opaque tokens. AIP calls it `next_page_token`, Stripe calls it `starting_after`. Picked the Stripe shape because LeadKart's `.NET` parent's frontend already understands it.

5. **Total-count exposure via separate `?include_total=true`.** Rejected for v0.2. Even on-demand, `COUNT(*)` under RLS is O(n) and the planner can't push the policy predicate cleanly. Frontend teams who want totals get told "use the `has_more` flag; if you absolutely need a number, run a separate `/stats` query — those are cached." Stripe's stance for ~7 years; nobody dies.

6. **GraphQL-style Relay connection (`{ edges, pageInfo, totalCount }`).** Rejected. Heavier wire shape, GraphQL-coupled vocabulary in a REST API, and the `totalCount` field still requires the O(n) count. We're not building a Relay client.

## Sources

- [Markus Winand — Use the Index, Luke: Paging Through Results](https://use-the-index-luke.com/sql/partial-results/fetch-next-page) — canonical exposition of keyset vs offset.
- [Brandur Leach — Pagination, with Postgres pages, and Hailtail-y](https://brandur.org/postgres-cursors) — per CLAUDE.md doctrine, Tier 1 source for Postgres-at-scale.
- [Stripe API — Pagination](https://stripe.com/docs/api/pagination) — `starting_after` / `ending_before` cursor shape; `has_more` flag; no total counts.
- [Google AIP-158 — Pagination](https://google.aip.dev/158) — `next_page_token` opaque cursor canon.
- [GitHub REST API — Using pagination](https://docs.github.com/en/rest/guides/using-pagination-in-the-rest-api) — Link header style; deferred-total approach.
- [PostgreSQL planner notes on RLS predicate pushdown](https://www.postgresql.org/docs/current/sql-createpolicy.html) — required reading for the "EXPLAIN under RLS" discipline.


**Fitness function:** convention-only — not mechanically expressible. Cursor pagination shape (Page[T] + opaque base64 Cursor) is covered by the EXPLAIN-under-RLS integration test (`keyset_explain_integration_test.go`).
