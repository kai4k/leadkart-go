# ADR 0027 — Audit log: outbox table doubles as audit

**Status:** Accepted
**Date:** 2026-05-05

## Context

LeadKart .NET maintains a separate audit-log Marten document store (`buildingblocks.audit_log_entry`) for forensic queries (who/when/why). The Go ecosystem has no Marten equivalent (ADR 0003). Building a parallel audit-log table on top of the outbox table would duplicate effort.

Brandur Leach's [Crunchy Bridge architecture](https://brandur.org/fragments/events) explicitly uses **one events table** as both audit log and webhook source: *"the outbox table is also a free audit log; you can replay it, grep it, and show it to support."*

## Decision

**Each module's `outbox` table doubles as that module's audit log.** No separate audit table.

Outbox row schema (per module schema):

```sql
CREATE TABLE identity.outbox (
    id          UUID PRIMARY KEY,
    tenant_id   UUID NOT NULL,                   -- denormalised for tenant-scoped audit queries
    topic       TEXT NOT NULL,                   -- e.g. "identity.tenant_registered"
    payload     JSONB NOT NULL,                  -- event data
    occurred_at TIMESTAMPTZ NOT NULL,            -- when it happened (domain time)
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),  -- when row was written
    forwarded_at TIMESTAMPTZ,                    -- when watermill-sql forwarder picked it up
    forwarded   BOOLEAN NOT NULL DEFAULT false   -- index target for forwarder polling
);

CREATE INDEX idx_identity_outbox_unforwarded ON identity.outbox (created_at) WHERE NOT forwarded;
CREATE INDEX idx_identity_outbox_tenant_topic_occurred ON identity.outbox (tenant_id, topic, occurred_at DESC);
ALTER TABLE identity.outbox ENABLE ROW LEVEL SECURITY;
ALTER TABLE identity.outbox FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON identity.outbox
    USING (tenant_id = current_setting('app.tenant_id')::uuid);
```

**Audit queries** (e.g. "show all actions on tenant X in the last 30 days"):

```sql
SELECT topic, payload, occurred_at
FROM identity.outbox
WHERE tenant_id = $1
  AND occurred_at >= now() - interval '30 days'
ORDER BY occurred_at DESC;
```

RLS ensures cross-tenant audit queries return only the requesting tenant's history.

## Retention

**7-year retention per DPDP Act 2023 + SOC2 CC4.1**. Implementation:

1. Daily river job (ADR 0010) scans for outbox rows older than 7 years.
2. Exports to cold storage (S3 Glacier / Azure Archive Blob) before delete.
3. Hard DELETE post-export.
4. Significant-Data-Fiduciary status under DPDP MAY require 10 years — config-driven `Audit:RetentionYears` env var.

## Consequences

**Positive:**
- Single table for messaging + audit + DPDP/GDPR right-to-erasure visibility.
- Audit queries are plain SQL on a partitioned, indexed table — fast.
- No Marten equivalent needed.
- Cross-context audit grep is straightforward (UNION ALL across module outboxes).

**Negative:**
- Outbox size grows fast at scale — partitioning by `occurred_at` (monthly) at Year-2+.
- `forwarded` flag pollutes a "pure audit" query — minor: `WHERE NOT forwarded` on hot path; full audit reads all rows regardless.
- Cold-storage export job is a critical path — failure means data retention non-compliance. Mitigation: river retry policy + alerting on job failure.

## Audit hooks beyond outbox

The outbox captures **domain events** (state-change facts). For non-state-change auditable events (failed login attempts, denied authorisation checks, admin reads), a parallel `audit_event` mechanism may be needed — punt to a future ADR if requirements demand it.

## Alternatives considered

1. **Separate Marten-equivalent doc store.** Rejected: no canonical Go library; duplicates outbox work.
2. **Append-only `audit_log` table written by every command handler.** Rejected: every handler must remember to write the audit row; outbox is structurally inseparable from the aggregate update (per ADR 0004 UpdateFn pattern), so audit-via-outbox is automatic.
3. **External SIEM (Datadog Logs, Splunk).** Rejected as primary: source-of-truth must live in Postgres for DPDP retention compliance; SIEM can ingest *from* the outbox via OTel logs export, but the audit record itself stays in Postgres.

## Sources

- [Brandur Leach — There's always an events table](https://brandur.org/fragments/events).
- [TDL — Distributed Transactions in Go (Oct 2024)](https://threedots.tech/post/distributed-transactions-in-go/) — outbox + audit unification.
- [DPDP Act 2023 §12 + SOC2 CC4.1](https://www.meity.gov.in/digital-personal-data-protection-act-2023) — 7-year retention requirements.
- LeadKart .NET — `data-retention.md` doctrine (mirror reference for Go port).


## Fitness function

`TestArch_OutboxTableSchema` (in `internal/architecture/`).

Outbox table column set is fixed — the audit reader assumes id / occurred_at / topic / payload / forwarded_at + the act_* columns per ADR 0056.
