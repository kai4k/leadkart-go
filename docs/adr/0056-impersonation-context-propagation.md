# ADR 0056 — Impersonation context propagation (outbox → Watermill subscriber)

**Status:** Accepted
**Date:** 2026-05-23

## Context

Wave 4 (ADR 0045) shipped scoped JWT impersonation: the `Claims.Act` field carries the RFC 8693 §4.1 actor claim (operator + session_id + reason) on every scoped token, and migration 20260524000001 added the matching `act_operator_id` / `act_session_id` / `act_reason` columns to `buildingblocks.audit_log_entry` for forensic queries. The deliberate Wave 4.1 punt:

> Audit-log enrichment (writing the new act_* columns) deferred to Wave 4.1 — requires propagating impersonation context through outbox → Watermill subscriber boundary. Schema shipped; population NULL until 4.1.

The gap is structural. The audit row for an integration-event-driven action is written by `AuditMiddleware` inside a **Watermill subscriber** — a goroutine that consumes a `*message.Message`, not an HTTP request. There is no JWT in scope at that point; the original operator's identity was lost at the outbox boundary (the HTTP handler wrote the outbox row + returned 200; the subscriber-side audit row, written seconds later by the worker process, has no idea who started the session).

Without propagation, every Watermill-driven audit row from an impersonation session is indistinguishable from a regular user action. SOC2 CC4.1 + DPDP §12 actor-chain audit requirements force the gap closed.

### What we want to propagate

Three fields, all already minted by the impersonation issuer (per ADR 0045 §"Token lifecycle"):

| Field | Source | Audit column |
|---|---|---|
| `act_operator_id` | `claims.Act.Sub` (operator's PersonID) | `act_operator_id` (uuid) |
| `act_session_id`  | `claims.Act.SessionID` (impersonation session ID) | `act_session_id` (uuid) |
| `act_reason`      | `claims.Act.Reason` (operator-supplied free text) | `act_reason` (text) |

NULL on the non-impersonation hot path (the overwhelming majority of requests). The columns already exist on `audit_log_entry`; this ADR wires the value.

### Choice space

| Option | Cost | Trade-off |
|---|---|---|
| A. JSONB `metadata` column on outbox + JSON parse in forwarder | Schema-flexible | Hides the actor chain inside an opaque blob; loses partial-index ability; JSON parse on the hot forwarder path. |
| B. Named `act_*` columns on outbox + 1:1 forwarder mapping | Three columns, predictable shape | Forwarder code knows the names; new headers require a column add (acceptable — these are slow-changing). |
| C. Bake the actor chain into the event PAYLOAD | Zero infra change | Pollutes the V1 event schemas (every event would now carry act_*); breaks the "events are user-domain data, NOT cross-cutting metadata" boundary; forces every consumer to know about impersonation. |
| D. River queue side-channel | Decoupled | River is for jobs, not events; we already have the outbox; adding river for cross-cutting context is parallel infra. |

We pick **B** — named columns. Microsoft "Outbox pattern with .NET" canon + Brandur "events table" canon both use well-named columns for cross-cutting context over opaque metadata bags. The forwarder maps column → Watermill metadata 1:1; the subscriber-side `AuditMiddleware` maps metadata → audit row 1:1. No JSON parse on the hot path.

Watermill metadata (the message's key/value envelope, NOT the JSON payload) is the canonical place to carry cross-cutting context per Watermill canon — exactly the same shape we use for `tenant_id`, `event_type`, `correlation_id`, and the W3C Trace Context (ADR 0014). The analogous design in distributed tracing is **OpenTelemetry Baggage**: named cross-cutting context propagated alongside the trace, NOT inside the span payload.

## Decision

### Propagation path

```
HTTP request → authn middleware → claims.Act → ctx via actclaim.WithContext
   ↓
Command handler → repository → adapter (outbox writer)
   ↓
outbox row.act_* columns ← actclaim.FromContext(ctx)
   ↓
OutboxForwarder → Watermill message.Metadata[act_*]
   ↓
Subscriber → AuditMiddleware → audit_log_entry.act_*
```

### 1. New ctx-accessor package — `internal/identity/app/actclaim/`

```go
type Claim struct {
    OperatorID string
    SessionID  string
    Reason     string
}

func WithContext(ctx context.Context, c Claim) context.Context { ... }
func FromContext(ctx context.Context) (Claim, bool)            { ... }
```

Lives at `app/` (not `ports/authn/` or `adapters/`) because:

- `authn` (the source) lives at `ports/` — `adapters/` MUST NOT import `ports/` per ADR 0047.
- `adapters/outbox_writer.go` (the sink) MAY import `app/*` per ADR 0047 §"app/ may depend on".
- `app/` is the boundary-clean pivot: ports populates, adapters reads.

The `Claim` shape is a structural copy of `jwt.ActClaim` (three strings) so the adapter doesn't need to import `app/jwt` — keeps the dependency arrow shallow.

Zero `Claim` is a no-op inside `WithContext` (saves a ctx allocation on the non-impersonation hot path).

### 2. authn middleware change — `RequireAuth`

After successful JWT verification:

```go
if claims.Act != nil {
    ctx = actclaim.WithContext(ctx, actclaim.Claim{
        OperatorID: claims.Act.Sub,
        SessionID:  claims.Act.SessionID,
        Reason:     claims.Act.Reason,
    })
}
```

Identical short-circuit shape to the existing tenancy + claims wiring above it. No change to `RequirePermission` / `RequirePlatform` / `RequireTenantContext` — they wrap `RequireFreshStamp(verifier, validator)` which itself wraps `RequireAuth`, so the ctx propagation is transitive.

### 3. Outbox schema change — migration 20260525000001

```sql
ALTER TABLE identity.outbox
    ADD COLUMN act_operator_id uuid NULL,
    ADD COLUMN act_session_id  uuid NULL,
    ADD COLUMN act_reason      text NULL;
```

Nullable; no data migration; non-impersonation rows leave columns NULL. Same shape migration 20260524000001 used for the audit table.

### 4. Outbox writer change — `writeOutboxEvents`

Per-batch helper reads the ctx claim once + applies to every row in the batch (commands that emit N events all share the same actor):

```go
actOperatorID, actSessionID, actReason := outboxActParams(ctx)
for _, e := range events {
    q.InsertOutboxEvent(ctx, db.InsertOutboxEventParams{
        ..., ActOperatorID: actOperatorID, ActSessionID: actSessionID, ActReason: actReason,
    })
}
```

Defensive: malformed UUID strings (claim sub / session_id that isn't a UUID) drop to NULL instead of failing the write. Audit-log outage MUST NOT cascade per `audit-checklist.md §12`.

### 5. OutboxForwarder change

Per row drained:

```go
if row.ActOperatorID.Valid { msg.Metadata.Set("act_operator_id", uuidFromPg(row.ActOperatorID).String()) }
if row.ActSessionID.Valid  { msg.Metadata.Set("act_session_id",  uuidFromPg(row.ActSessionID).String())  }
if row.ActReason != nil && *row.ActReason != "" { msg.Metadata.Set("act_reason", *row.ActReason) }
```

Empty metadata for non-impersonation rows. Same shape as the existing `tenant_id` + `event_type` + `occurred_at` stamping.

### 6. AuditMiddleware change

Subscriber-side projection into `audit.Entry`:

```go
entry := audit.Entry{
    ..., 
    ActOperatorID: parseUUIDHeader(msg.Metadata.Get(HeaderActOperatorID)),
    ActSessionID:  parseUUIDHeader(msg.Metadata.Get(HeaderActSessionID)),
    ActReason:     msg.Metadata.Get(HeaderActReason),
}
```

`parseUUIDHeader` already drops malformed values to `uuid.Nil` — which `audit.Writer.Write` translates to SQL NULL via its existing NULL-or-value branches.

### 7. Header name constants — `internal/common/messaging/`

```go
HeaderActOperatorID = "act_operator_id"
HeaderActSessionID  = "act_session_id"
HeaderActReason     = "act_reason"
```

Same naming canon as the existing `HeaderTenantID` / `HeaderEventType`.

## Consequences

### Positive

- **The act_* columns are populated.** Wave 4.1 closed.
- **Boundary-clean.** Adapter doesn't import `ports`; `app/actclaim` is a 30-line VO + ctx accessor.
- **NULL-safe hot path.** Non-impersonation requests (the >99% case) carry empty Watermill metadata + write SQL NULL. No JSON parse, no UUID parse, no allocation overhead.
- **Forensic queryability preserved.** Partial indexes (`idx_audit_act_operator_occurred`, `idx_audit_act_session_occurred` from migration 20260524000001) match the populated rows.
- **Failure mode = audit-only.** Malformed metadata drops to NULL → log at WARN → no cascade. Stripe / Auth0 / Twilio outbox canon: cross-cutting context is best-effort propagation, never the cause of a 500.

### Negative

- **Schema column add.** Three new nullable columns on `identity.outbox`. Trade-off accepted (option B over A); the migration is additive + reversible.
- **Forwarder code path widens.** Three new `if row.X.Valid { ... }` blocks. Trivial to read; no perf concern (existing per-row UPDATE already dominates the forwarder loop).
- **No HTTP-side audit middleware enrichment.** The HTTP `idempotency.Middleware` does NOT currently call `audit.Writer` directly (the design was speculative in the original Wave 9.2c spec). When it does (deferred), it will use the same `authn.ClaimsFromContext(ctx).Act` shape — no API change.

### Out of scope (deferred follow-ups)

- HTTP request-path audit enrichment when the idempotency middleware grows audit writes. The shape will mirror this ADR: read `claims.Act` from ctx, populate `audit.Entry.Act*`.
- River background-job propagation. River jobs are minted by the API process via `riverClient.Insert(ctx, jobArgs, opts)`; if a job carries impersonation context (e.g. "scheduled action under operator X"), we'll stamp on `Metadata` (River 0.10+ has named per-job metadata).
- Cross-module event propagation when Phase 2 (Platform) module subscribers consume Identity events. The metadata headers travel through Watermill verbatim across module boundaries.

## Sources

- RFC 8693 §4.1 — Actor claim (`act`) shape (nested-actor recommendation).
- RFC 7515 — JWS design (named claims over `ext` blob).
- OpenTelemetry Baggage spec — analogous cross-cutting context propagation pattern.
- Microsoft Learn — "Outbox pattern with .NET" (named columns for cross-cutting context).
- Brandur Leach — "Building robust systems with transactional outbox" (events table canon).
- ADR 0045 — Scoped JWT impersonation (the parent ADR this completes).
- ADR 0047 — Layer-boundary discipline (drove the `app/actclaim` placement decision).
- LeadKart `.NET .claude/rules/audit-checklist.md §12` — audit-log outage MUST NOT cascade.
- ThreeDotsLabs Watermill — message metadata vs payload conventions.
