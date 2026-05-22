# ADR 0031 — HTTP idempotency via `X-Command-Id`

**Status:** Accepted
**Date:** 2026-05-08

## Context

Mutating HTTP commands need replay protection: a client that retries POST/PUT/DELETE after a transient error (network reset, 502 from a load balancer, browser-tab refresh on a payment screen) must not double-execute. Without server-side dedup, the second submission performs the side effect again.

Two layers of dedup are already shipped in the codebase:

- `messaging.IdempotentReceiver` deduplicates Watermill envelopes by `messageID` against `identity.processed_messages`. That's the *event-bus* layer.
- The DDD aggregate factories (`NewTenant`, `NewPerson`, …) reject duplicates via Postgres unique indexes. That's the *aggregate-invariant* layer.

Neither addresses the HTTP layer. A client that POSTs `/api/v1/tenants` twice with identical bodies sees two `201`s with two distinct tenant IDs — the aggregate invariant doesn't fire because each POST creates a *different* aggregate.

`messaging.md` "X-Command-Id accepted on mutating HTTP commands" specifies the contract; the implementation existed in `internal/common/idempotency` but was never wired into `cmd/api`.

## Decision

**`X-Command-Id` HTTP header + Stripe-style replay protection** wired as a middleware in the canonical chain (ADR 0030).

Header name: `X-Command-Id` (LeadKart-canonical, mirrors Stripe's `Idempotency-Key`). Value: client-supplied UUID.

Behaviour (per `internal/common/idempotency.Middleware`):

- **Header absent** → middleware passes through. Idempotency is opt-in per request; clients without retry semantics don't pay the lookup cost.
- **Header malformed** (not a UUID) → 400 `idempotency.invalid_command_id`.
- **First execution + 2xx response** → cache verbatim (status, body, content-type) keyed by `(X-Command-Id, sha256(body))`. TTL 24h (Stripe canon — long enough to survive partial-outage retries, short enough to bound storage).
- **Replay with same key + same body hash** → return cached response with `X-Idempotent-Replay: true` so clients can distinguish fresh from cached.
- **Replay with same key + different body hash** → 422 `idempotency.key_reuse`. Detects key reuse across distinct payloads (programming bug or attempted abuse).
- **First execution + non-2xx** → DO NOT cache. Per Stripe canon: caching failures lets transient infrastructure errors (timeouts, connection resets) be retried legitimately rather than masquerading as a cached client error.

Storage: `idempotency.Store` interface; `InMemoryStore` for v0.2 single-replica + tests; v0.3 swaps to a Postgres-backed store (table + TTL + background purge via the river pool — composes naturally with ADR 0010 / the AuditLogPurgeJob shape).

Auditing: every replay attempt is recorded via `audit.Writer.Write` so operators can see retry patterns.

## Consequences

**Positive:**

- Clients implementing safe retries (browser SDKs, mobile apps, CLI tools) get exactly-once-execution semantics for free.
- Body-hash cross-check catches key reuse with different payloads — a class of bug that pure-key caching silently miscaches.
- Failure-not-cached policy preserves the "transient errors are safe to retry" property.
- Storage is pluggable; v0.2 in-memory ships now, v0.3 Postgres swap is one constructor swap.

**Negative:**

- Per-request store lookup on every mutating endpoint. Mitigated: header-absent path is a no-op; header-present is a single hash + map lookup (in-memory) or one indexed Postgres roundtrip (v0.3).
- In-memory store is single-replica-correct only. Multi-replica = a client that hits replica A then retries against replica B sees two executions. Documented; v0.3 Postgres swap closes the gap.
- 24h TTL means storage grows with traffic. At v0.2 traffic shape (~thousands of mutating commands/day) bounded; v0.3 Postgres + periodic purge handles it.

## Alternatives considered

1. **HTTP-method idempotency only (rely on PUT/DELETE being naturally idempotent at the aggregate layer).** Rejected: doesn't help POST. POST creates new aggregates; "natural idempotency" requires unique constraints that aren't always available (e.g. anonymous flows like password-reset request can't unique-constraint on email without leaking enumeration).
2. **`Idempotency-Key` header (literal Stripe name).** Considered. Rejected: project-internal canon already standardised on `X-Command-Id` (matches the messaging layer's `messageID` shape and the .NET reference).
3. **ETags / `If-Match`.** Optimistic concurrency, not idempotency — different problem. Solves "don't overwrite a stale read" not "don't double-execute a retry."
4. **Cache failures too (return cached 500 on retry).** Rejected: a transient 500 cached for 24h punishes the client for our infrastructure blip. Stripe canon explicitly recommends against caching failures.

## Sources

- [Stripe — "Designing robust and predictable APIs with idempotency"](https://stripe.com/blog/idempotency) (2018) — `Idempotency-Key` semantics, body-hash cross-check, failure-not-cached policy.
- [Stripe API reference — Idempotent Requests](https://docs.stripe.com/api/idempotent_requests).
- LeadKart `.NET .claude/rules/messaging.md` "X-Command-Id accepted on mutating HTTP commands."
- ADR 0030 (canonical middleware chain) — composition placement.
- ADR 0008 (Watermill) + ADR 0027 (audit log outbox) — sibling dedup layers; this ADR defines the HTTP layer above them.
