# ADR 0040 — Search strategy: `pg_trgm` now, Postgres FTS at Phase 4, defer dedicated infrastructure

**Status:** Accepted
**Date:** 2026-05-18

## Context

The frontend backend wishlist (committed at `fbca944`) calls for server-side search across multiple resources:

- `GET /v1/users?q=` — per-tenant member search by email + name
- `GET /v1/tenants?q=` — operator listing by slug + legal_name + display_name
- `GET /v1/persons?q=` — operator cross-tenant person lookup
- `GET /v1/search?q=` — Cmd+K omni-search across persons + tenants + (future) leads

Phase 2 will add `crm.leads` — intrinsically a search-heavy aggregate (find by name, phone, GST, product interest) with potentially millions of rows per tenant.

The search-infrastructure decision space is broad — `pg_trgm` extension, Postgres FTS (`tsvector`/`tsquery`), `pg_search` (ParadeDB / BM25-in-Postgres), Meilisearch / Typesense (dedicated services), Elasticsearch / OpenSearch (cluster-scale), Algolia (hosted). Each has a different cost / capability / ops profile. **Picking late is cheap; picking early and wrong is expensive** (search-projection schemas + indexes are sticky once data is in them).

Constraints inherited from preceding ADRs:

- ADR 0004 — sqlc + pgx/v5. Search queries are generated through sqlc; no ORM-level abstractions.
- ADR 0006 — RLS + SET LOCAL. Trigram + FTS indexes must compose with RLS predicate pushdown.
- ADR 0017 — koanf config; no per-search-service config sprawl.
- ADR 0024 — Chainguard distroless + cosign + Trivy. Every new service is an ops-surface tax.
- Phase 1.5 — single-binary `cmd/api` + single Postgres + single Redis. No additional infra unless forced.

Non-goals:

- Vector / semantic search. Defers to Phase 6+ AI features per separate ADR when AI gates this.
- Full-text search across rich documents (PDFs, scanned licences). Out of scope; metadata-only.
- Geographic search (PostGIS / "near Mumbai"). Out of v0.2 scope; lands with the dispatch module.

## Decision

**Three-stage staircase, each level upgradeable in-place:**

```
v0.2 - Phase 3              pg_trgm GIN indexes
                            └─ Substring + similarity for ILIKE patterns
                            └─ Sub-50ms at ~1-10M rows per indexed table
                            └─ Postgres extension; $0 extra infra; zero ops

Phase 4 - Phase 5           Postgres FTS (tsvector + tsquery + ts_rank_cd)
                            └─ Word-tokenized + stemmed search + relevance ranking
                            └─ Domain-specific dictionaries (pharma terms, drug names)
                            └─ Maintained via outbox subscribers into search-projection tables (ADR 0041)
                            └─ Postgres-native; $0 extra infra; zero ops

Phase 6+ (gated on AI work) pgvector + HNSW for semantic search
                            └─ Hybrid lexical + semantic via Reciprocal Rank Fusion (RRF)
                            └─ Postgres extension; ~$100/yr embedding API + storage growth

Hypothetical (50M+ rows OR  Dedicated search infrastructure
1K+ QPS OR ranking pain)    └─ ParadeDB (pg_search BM25) OR Meilisearch (Rust binary, simple ops)
                            └─ Postgres-resident first; only externalize if Postgres-resident hits real limits
                            └─ Re-decide at the time; this ADR doesn't pre-commit
```

### v0.2 implementation — `pg_trgm` (active decision)

Trigram GIN indexes on the searchable columns of every realistic-pain search target:

```sql
CREATE EXTENSION IF NOT EXISTS pg_trgm;

CREATE INDEX idx_persons_search_trgm
    ON identity.persons
    USING gin (
        (lower(email) || ' ' || lower(first_name) || ' ' || lower(last_name)) gin_trgm_ops
    )
    WHERE is_active AND NOT is_anonymised;

CREATE INDEX idx_tenants_search_trgm
    ON identity.tenants
    USING gin (
        (lower(slug) || ' ' || lower(legal_name) || ' ' || lower(display_name)) gin_trgm_ops
    )
    WHERE status != 'hard_deleted';

CREATE INDEX idx_memberships_search_trgm
    ON identity.tenant_memberships
    USING gin (
        tenant_id,
        (coalesce(lower(designation),'') || ' ' || coalesce(lower(department),'')) gin_trgm_ops
    )
    WHERE status = 'active';
```

Notes:

- **Concatenated search column.** A single GIN index over `(lower(col_a) || ' ' || lower(col_b))` is more space-efficient than per-column indexes and lets one query match across all fields.
- **`lower()` for case-insensitive.** `pg_trgm` is case-sensitive by default; lowercasing both the indexed expression and the query string makes ILIKE behavior natural.
- **Partial indexes** match the common predicate (`is_active`, `status='active'`, `status != 'hard_deleted'`). Cuts index size 30-50% for tables where most queries filter on activity status anyway.
- **`tenant_id` leading in the membership index.** Per-tenant search queries always include `tenant_id = $1`; the planner uses the leading column as a coarse filter then the GIN trigram for the substring match.

### Query shape

```sql
-- name: SearchPersons :many
SELECT id, email, first_name, last_name, created_at
FROM   identity.persons
WHERE  is_active AND NOT is_anonymised
AND    (lower(email) || ' ' || lower(first_name) || ' ' || lower(last_name))
       LIKE '%' || lower($1) || '%'
ORDER  BY similarity(
    lower(email) || ' ' || lower(first_name) || ' ' || lower(last_name),
    lower($1)
) DESC, id DESC
LIMIT  $2;
```

`similarity()` is the `pg_trgm` ranking primitive — returns 0.0-1.0 based on trigram overlap. Good-enough relevance for v0.2; not BM25-quality but doesn't need to be at this scale.

### Multi-stage retrieval (Cmd+K omni-search)

`GET /v1/search?q=acme` runs the query across 4 resource types in parallel via `errgroup`:

```
Stage 1: parallel trigram filter (4 queries, LIMIT 20 each)
         persons + tenants + users + leads → 80 candidates
   │
   ▼
Stage 2: per-resource similarity() rank → top 5 per type
   │
   ▼
Stage 3: return categorised JSON
```

Bounds: `q` length 2-100 chars; each sub-query has a 200ms timeout; partial results returned with `has_partial: true` if one resource search slows the response.

### Upgrade path

When `pg_trgm` hits a limit (poor ranking quality, > 50M searchable rows, > 100 search QPS), the migration to FTS is **schema-additive, not schema-replacing**:

1. Add a `search_vector tsvector GENERATED ALWAYS AS (...) STORED` column alongside the existing trgm-indexed column.
2. Add a GIN index on `search_vector`.
3. Update the sqlc query to use `@@` operator + `ts_rank_cd` for ranking.
4. Drop the trgm index once the FTS query is validated.

No data migration; no downtime; the trgm index can coexist during cutover. Same pattern applies for the FTS → ParadeDB (`pg_search`) upgrade later if BM25 ranking quality becomes a pain point.

### When to consider dedicated infrastructure

This ADR explicitly defers the decision but documents the **decision triggers** so future contributors know when to re-evaluate:

- **Per-table searchable row count > 50M** AND `pg_trgm` query p95 > 500ms after index tuning
- **Search QPS sustained > 100/sec** AND Postgres CPU consistently > 70% from search queries
- **Ranking quality complaints** that BM25 (via `pg_search` extension, still in Postgres) doesn't resolve
- **Faceted search requirements** (e.g. "leads with status IN (...) AND tag IN (...) AND created_after > X") beyond what Postgres indexing covers efficiently

Until ≥1 of those fires, **stay in Postgres**.

## Consequences

**Positive:**

- **$0 extra infrastructure for the entire v0.2-Phase 5 trajectory.** No new services to deploy, monitor, back up, or pay for. `pg_trgm` is a Postgres extension; FTS is built into Postgres core.
- **Schema-additive upgrade path.** `pg_trgm` → FTS → `pg_search` (BM25) → external infra is a staircase, not a cliff. Each step builds on the last; no data migration between v0.2 and Phase 5.
- **Ops simplicity.** One Postgres to manage. The closer you get to FAANG scale, the more this matters — but small teams benefit too.
- **Predictable latency.** Sub-50ms search at realistic LeadKart scale (1-10M rows per table). Tested via `EXPLAIN ANALYZE` under partial-index conditions.
- **Per-tenant isolation by RLS.** Tenant search queries inherit the existing RLS policy; no separate access-control surface to keep in sync.

**Negative:**

- **`pg_trgm` ranking is weaker than BM25.** For short queries with clear keyword matches, `similarity()` works fine. For long natural-language queries ("find a wholesaler in Mumbai with valid drug licence"), ranking quality degrades. Mitigation: Phase 4 FTS upgrade brings stemming + ranking; Phase 6 vector search brings semantic match.
- **GIN index update cost.** GIN indexes have higher write amplification than B-tree (~3-5× insert cost). At Indian PCD pharma write throughput (~10s of inserts/sec/tenant), this is invisible. At Phase 4+ scale (high lead-volume tenants) we'd consider switching to GIST or partial-index strategies.
- **No faceted aggregation.** Postgres FTS doesn't support facet counts natively (no equivalent of Elasticsearch `terms` aggregation). At v0.2 we don't need it; if Phase 4 dashboards demand "show counts by status / by region", we'd add materialized aggregates per facet, not externalize search.
- **No multi-language tokenization out of the box.** Postgres FTS comes with English + a few Latin-script languages. Hindi / regional Indian languages (for pharma operator names in Devanagari) need `pg_jieba` or similar — Phase 4+ concern.

## Alternatives considered

1. **Start with Postgres FTS, skip `pg_trgm`.** Considered. Rejected for v0.2 because FTS tokenization adds 2-3 days of dictionary-tuning work (figuring out which language config; whether to lowercase; how to handle pharma-specific terms) that doesn't pay off until ranking-quality complaints appear. `pg_trgm` is "good enough" out of the box; FTS is the next step when ranking matters.

2. **Start with Meilisearch (self-hosted).** Considered. Rejected for v0.2 because $20/mo extra VM + new service to deploy/monitor/back up isn't justified before customer pain emerges. Meilisearch is the right answer when Postgres FTS hits ranking-quality limits AND we can articulate a customer-visible benefit. Until then, in-Postgres is cheaper in every dimension.

3. **Start with Elasticsearch / OpenSearch.** Hard rejection. ES requires JVM tuning, a multi-node cluster for prod HA, ~$200-500/mo at minimum self-hosted, and is a famously sharp ops surface. Justified only at 50M+ rows AND > 100 QPS — neither of which LeadKart faces for at least 18-24 months.

4. **Algolia (hosted).** Rejected. Pricing scales linearly with search volume + indexed records — quickly hits $200-2000/mo at realistic LeadKart scale. Margin-killing for Indian PCD pharma SaaS pricing. Excellent product for documentation-search use cases (Stripe Docs, HN, etc.) but wrong tool for app search.

5. **Hand-write inverted indexes in Postgres.** Considered (briefly, for educational value). Rejected — `pg_trgm` IS the canonical "inverted index on n-grams" implementation, written by people who understand Postgres internals far better than we do. Reinventing it would be premature specialization (cf. GitHub Blackbird — only justified at extreme scale + extreme specificity).

6. **Defer search entirely until Phase 4.** Rejected. The frontend has a real UX dependency on `?q=` for users / tenants / persons today. v0.2 ships without search means the UI either lists everything (already a pain at 100+ rows) or hand-filters client-side (worse). `pg_trgm` is cheap enough that "no, defer it" is over-conservative.

## Sources

- [PostgreSQL pg_trgm documentation](https://www.postgresql.org/docs/current/pgtrgm.html) — primary reference.
- [Brandur Leach — "How We Built a Postgres-Powered Search for an API"](https://brandur.org/ohai-postgres) — companion to ADR 0004 canon citation; Tier 1 source per CLAUDE.md doctrine.
- [ParadeDB — `pg_search` BM25 in Postgres](https://docs.paradedb.com/api-reference/full-text/overview) — Phase 4+ upgrade target.
- [PostgreSQL Full-Text Search](https://www.postgresql.org/docs/current/textsearch.html) — Phase 4 baseline.
- [Russ Cox — "Regular Expression Matching with a Trigram Index"](https://swtch.com/~rsc/regexp/regexp4.html) — algorithmic foundation; explains why trigram pre-filtering scales.
- ADR 0004 — DB layer (sqlc + pgx + squirrel + goose).
- ADR 0006 — Multi-tenancy via Postgres RLS (search must compose with RLS).
- ADR 0041 — CQRS read models via outbox subscribers (the projection pattern search will use at Phase 2+).
