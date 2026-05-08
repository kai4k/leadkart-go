# ADR 0028 — SecurityStampCache + stale-write fence

**Status:** Accepted
**Date:** 2026-05-08

## Context

LeadKart access tokens are short-lived JWTs (10-min TTL per ADR 0011) carrying a `security_stamp` claim — a per-Person nonce minted at every credential-rotation event (password change, email change, anonymisation, global suspend). The middleware that validates JWTs MUST consult the source-of-truth stamp on every request: a token whose Person has rotated its stamp must fail closed within seconds, not at the JWT's expiry.

Naively, that's a Postgres roundtrip per authenticated request — at v0.3 read traffic that's prohibitive. The hot path needs caching.

A read-through cache around the stamp lookup runs into three races:

1. **L1 async eviction (ristretto)**: ristretto's `Cache.Del` queues to an internal buffer. After `Invalidate` returns, the next `L1.Get` can still see the stale entry until the buffer drains.
2. **Invalidate during in-flight Get**: a Get whose factory captured the *pre-mutation* source can write that snapshot to L1+L2 milliseconds *after* Invalidate clears them — re-poisoning the cache the cascade subscriber just emptied.
3. **TTL fallback alone is too slow**: a 30s TTL closes the window eventually but lets a revoked token authenticate for up to half a minute.

## Decision

**Typed cache facade per concern** + **per-facade generation counter** as a stale-write fence.

Wiring (per `audit-checklist.md §12b`):

- `cache.HybridCache` = ristretto L1 + Redis L2 + `singleflight` stampede protection. One per process.
- `cache.Facade[K,V]` = generic typed wrapper around HybridCache. Accepts a keyer + read-through factory + per-facade TTL. Domain code injects the typed facade, never raw HybridCache.
- `adapters.SecurityStampCache` = the typed facade for `(person.ID → SecurityStamp)`. 30s TTL (Auth0/Okta session-validation refresh window).
- `adapters.SecurityStampValidator` = thin compare around the cache facade.
- `authn.RequireFreshStamp(verifier, validator)` = HTTP middleware composing JWT signature verification + `IsFresh(personID, claimStamp)` check.

Stale-write fence on `Facade`:

- Per-facade `gen atomic.Uint64`.
- `Invalidate` / `InvalidateMany` / `Set` bump `gen` *before* clearing/writing storage.
- The Get-miss factory closure captures `genBefore` at start, re-reads `gen.Load()` after the source fetch and encoding; if the generation has advanced, the factory result is returned to the caller (it WAS source-of-truth at read time) but the cache write is **skipped**. The next Get re-queries.
- `L1.Wait()` after `L1.Del` drains ristretto's async buffer so the eviction is observable to the next read.

The two work together — gen-counter fences the **writes**; Wait fences the **reads**.

Cascade-side invalidation: `RevokeFamiliesOnSecurityChange` (Watermill subscriber on `PersonPasswordChangedV1` / `PersonAnonymisedV1` / `PersonGloballySuspendedV1` / `PersonEmailChangedV1`) calls `SecurityStampCache.Invalidate` BEFORE revoking refresh-token families. Cache invalidate is fast (~µs); doing it first means the freshness gate trips on the very next request rather than waiting up to 30s for the TTL.

## Consequences

**Positive:**

- Hot-path hits L1 in microseconds; cache miss is a single Postgres roundtrip via singleflight (concurrent misses for the same Person coalesce).
- Subscribers can `Invalidate` fearlessly — no concurrent Get can re-poison the cache.
- Distinct `stale_token` (revoked session) vs. `unauthenticated` (bad signature / expired) error codes let the SPA differentiate the re-login UX.
- Defense-in-depth: even if the cascade subscriber is offline, the 30s TTL still closes the window.

**Negative:**

- ristretto + Redis adds two cache layers to operate. Mitigated by the typed facade hiding both behind a single interface — domain code is unaware.
- `Wait()` in `Invalidate` blocks until ristretto drains; bounded (typically µs) but a slow ristretto under heavy concurrent Sets could spike. Acceptable because Invalidate runs on the rare cascade-subscriber path, not the per-request hot path.
- Per-facade `gen` is a single global counter; an Invalidate on key A skips writes for in-flight Gets on key B too. Suboptimal but correct (re-fetches don't break behaviour, just add extra factory calls under heavy invalidation churn).

## Alternatives considered

1. **TTL-only invalidation (no `Invalidate` API).** Simplest. Rejected: 30s revocation window is too long for security-critical state. Auth0/Okta canon supports the TTL as a *fallback*, not the primary mechanism.
2. **No L1 (Redis-only).** Removes ristretto async semantics. Rejected: every authenticated request would hit Redis; even local Redis is ~1ms vs ristretto's ~µs. At 10k RPS, 1ms = 10 cores of Redis traffic.
3. **Per-key versioning instead of per-facade.** More precise (Invalidate on A doesn't skip writes for B). Rejected: per-key state in a sync.Map adds memory + lock contention; per-facade is cheap and sufficient — under heavy invalidation churn the over-skip is bounded.
4. **Lock-based `Invalidate` (RWMutex over the facade)**. Trivially correct. Rejected: kills concurrency on Get — every Get takes a read lock that conflicts with Invalidate's write lock.

## Sources

- [Microsoft Learn — "Hybrid cache library in ASP.NET Core"](https://learn.microsoft.com/en-us/aspnet/core/performance/caching/hybrid) — the L1+L2 pattern LeadKart Go ports to.
- [dgraph-io/ristretto v2 README](https://github.com/dgraph-io/ristretto) — L1 admission + async write semantics; `Wait()` documentation.
- [`golang.org/x/sync/singleflight` godoc](https://pkg.go.dev/golang.org/x/sync/singleflight) — concurrent-miss coalescing.
- [Auth0 — "Session Management" canon](https://auth0.com/docs/secure/tokens/access-tokens) — ~30s session-validation refresh window.
- LeadKart `.NET .claude/rules/audit-checklist.md §12b` "Cache facade per concern" + "Proof-of-cache test" rules — same canon Go ports.
