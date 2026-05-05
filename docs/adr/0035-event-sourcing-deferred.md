# ADR 0035 — Event sourcing scope: zero modules at v0.1

**Status:** Accepted
**Date:** 2026-05-05
**Related:** ADR 0003 (state-based + outbox default).

## Context

LeadKart .NET uses Marten event sourcing for **Orders** (financial state machine + invoice/credit-note replay) per Greg Young's rule: ES where audit/replay/compensation matters. Inventory is planned for ES (stock movements ARE events).

Go has **no Marten-equivalent** (ADR 0003). Building ES per-aggregate by hand requires:
- Event store schema (event tables, optimistic concurrency, snapshots).
- Projection layer (rebuild current state from events).
- Upcasters (event schema evolution).
- Snapshot tuning.

Three Dots Labs' newer EDA training does not even cover ES as a core topic. Their library `esja` (Nov 2024, pre-1.0) is the closest Go reference but explicitly "unstable API".

## Decision

**Zero modules use event sourcing at v0.1.** All aggregates use state-based persistence + outbox-in-Postgres (ADR 0003).

**Per-aggregate ES is a future decision** when:
1. **Audit/replay is provably load-bearing** — a regulatory / business requirement that outbox-as-audit (ADR 0027) cannot satisfy.
2. **Temporal queries** ("state of X at date Y") become a core feature, not a nice-to-have.
3. **Compensation logic** (Order cancellation, credit notes) is materially simpler with replay than with explicit reverse transactions.

When ES is justified per-aggregate:
- Build on plain pgx event-stream tables (Wild Workouts pattern adapted) OR adopt `esja` if it reaches 1.0 stability.
- ADD a per-aggregate ADR documenting the evidence + decision.

## Consequences

**Positive:**
- v0.1 ships faster — no event-store layer to build.
- Operations simpler — no projection rebuild, no snapshot tuning.
- Audit covered by outbox (ADR 0027) — not a feature regression.

**Negative:**
- Orders module loses LeadKart .NET's invoice-replay capability. Mitigation: invoice generation is idempotent via the outbox + business-key (`INV/2026-27/00047`); cancellation produces a CancellationNote referencing the original invoice. No replay needed for the v0.1 feature set.
- If business asks "rebuild Orders state from history", we don't have it. Risk acknowledged; revisit if/when this becomes a real requirement.

## Decision log for future review

When considering ES for a specific aggregate in v0.2+:
- [ ] Does outbox-as-audit cover the audit need? (Almost always yes.)
- [ ] Is "as-of-date" temporal querying a core feature, or an ad-hoc support need? (If the latter, dump from outbox.)
- [ ] Is replay-driven compensation materially simpler than explicit reverse transactions? (Usually no for state-rich aggregates.)
- [ ] Does the team have bandwidth to maintain projection + upcaster infrastructure? (Realistic estimate: +30% per ES-aggregate maintenance cost.)

## Alternatives considered

1. **ES for Orders + Inventory matching .NET.** Rejected: cost too high without library support; outbox covers audit/replay; revisit per-aggregate later.
2. **Plain pgx event tables for all modules.** Rejected as default: complexity per aggregate exceeds benefit when most modules don't need temporal queries.
3. **EventStoreDB (Kurrent) as external store.** Rejected: adds operational dependency; no clear payback for v0.1 scale.

## Sources

- [Greg Young — CQRS Documents (2010)](https://cqrs.files.wordpress.com/2010/11/cqrs_documents.pdf) — original ES decision criteria.
- [Brandur Leach — There's always an events table](https://brandur.org/fragments/events) — explicitly rejects ES for typical SaaS.
- [TDL esja library](https://github.com/ThreeDotsLabs/esja) — reference for if/when ES becomes justified per-aggregate.
- LeadKart .NET — `architecture.md` Greg Young rules section (mirror reference).
