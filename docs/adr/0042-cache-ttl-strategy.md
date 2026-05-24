# ADR 0042 — Cache TTL strategy: per-use-case tiered TTLs with documented rationale

**Status:** Accepted
**Date:** 2026-05-18

## Context

[ADR 0015](0015-caching-ristretto-redis-singleflight.md) locked the cache stack — ristretto (L1 in-process) + redis/go-redis/v9 (L2 distributed) + singleflight coalescing, wrapped behind a typed [`cache.Facade[K,V]`](../../internal/common/cache/facade.go). It did NOT specify TTL policies — those were left to per-facade decisions with a `DefaultTTL` fallback of `L1 = 1min / L2 = 5min`.

The Wave 1 pagination + capabilities work introduced multiple cache callers (platform stats, /me/capabilities, future search results, future ETag responses) that each have different freshness vs cost trade-offs. Picking TTLs ad-hoc per caller leads to:

- **Inconsistency** — operator dashboards stale for 30min while operator user lookups invalidate every 30s; users notice the gap.
- **Stampede risk** — multiple facades with identical TTLs all expire at the same wall-clock minute → thundering herd against Postgres.
- **No room to reason about it** — "why is the cache 5 minutes?" gets an "it just is" answer that's hard to revisit.

This ADR establishes the canon TTL groups, the research-grounded rationale for each, and the jitter discipline that prevents synchronized invalidation across replicas.

Research grounding (May 2026):

- **Microsoft HybridCache canon** ([learn.microsoft.com](https://learn.microsoft.com/en-us/aspnet/core/performance/caching/hybrid?view=aspnetcore-10.0)) — `DefaultEntryOptions` examples land on `Expiration = 10min` + `LocalCacheExpiration = 1min` for "typical" entries.
- **Multi-tier (L1 + L2) pattern guidance** — L1 should be seconds-to-minute scale; L2 5min-hour scale depending on freshness need. L1 hit-rate target is 80%+ for hot paths.
- **Search results caching** — 300s (5min) TTL is the canonical "stale-tolerant dashboard" value (Stripe Dashboard, Datadog, Loki Results Cache all converge here). Jitter (~10%) prevents thundering-herd.
- **Auth0 /userinfo** — 5min cache is documented as "not a problem" against a 24h token lifetime.
- **Better Auth** — ~5min cookie/session cache is the modern session-validation cadence.
- **Stampede mitigation** — randomised TTL jitter is mandatory at >1 replica; without it, expiry waves hammer the source.

Non-goals:

- Cross-region TTL strategy (multi-region Redis isn't on the v0.2 roadmap).
- Negative caching ("cache the not-found-result"). Sometimes useful; out of v0.2 scope. Add per-facade when needed.
- Stale-while-revalidate semantics (Microsoft HybridCache has a `LocalCacheExpiration` concept that's effectively SWR-lite — we use the same mechanism but don't expose explicit SWR knobs in our facade).

## Decision

**Five TTL profiles, each pre-named in [`internal/common/cache/hybrid.go`](../../internal/common/cache/hybrid.go), chosen per use-case rather than per-caller-ad-hoc.**

### The TTL groups

| Profile | L1 | L2 | Jitter | Use case |
|---|---|---|---|---|
| `DefaultTTL()` | **1 min** | **5 min** | ±10% | Generic reference data, lookups, configuration values |
| `SecurityStampTTL()` | 30 sec | 30 sec | none | Security-bearing freshness gate — sub-30s invalidation on rotation |
| `CapabilitiesTTL()` *(new)* | **2 min** | **15 min** | none | Membership-bound resolved capabilities (permissions + roles + profile). Stamp rotation invalidates implicitly. |
| `SearchResultsTTL()` *(new)* | **30 sec** | **5 min** | **±10%** | Search result lists keyed by query-hash. Jitter prevents stampede. |
| `DashboardTTL()` *(new)* | **1 min** | **5 min** | **±10%** | Operator dashboard counts, stats, deltas. Slightly stale ok; jitter mandatory. |

### Rationale per profile

#### `DefaultTTL` — 1 min / 5 min

Microsoft HybridCache documented canon. Already in code from Wave 0; this ADR confirms it as the baseline. Used by anything that doesn't have a more specific profile.

#### `SecurityStampTTL` — 30 sec / 30 sec

Auth0 / Okta session-validation refresh window. Sub-30s lets revocation propagate before the next access-token check. Already in code; ADR captures the rationale.

Single-tier (`WithOmitL1` per the SecurityStampCache caller) — the L1-race-with-invalidation window matters more than the sub-µs L1 hit latency for security-sensitive freshness.

#### `CapabilitiesTTL` — 2 min L1 / 15 min L2 (NEW)

`GET /v1/auth/me/capabilities` returns the resolved permission/role/profile bundle for a (membership, security_stamp) tuple. The security_stamp IS the invalidation mechanism — when a role or permission changes, the stamp rotates and the cache key changes (so the new value is naturally fetched). TTL is just a memory bound, not a correctness boundary.

Longer L2 (15min) is fine because:
- Cache key includes the security_stamp → stamp rotation invalidates implicitly
- 24h JWT TTL means the same (membership, stamp) tuple is queried up to 24h before refresh forces a new stamp anyway
- Memory-bound only — eviction is cost-driven, not correctness-driven

No jitter — the cache key is per-membership (low collision rate across replicas); invalidation is event-driven (stamp rotation), not time-driven.

#### `SearchResultsTTL` — 30 sec L1 / 5 min L2 (NEW)

Search result lists are the canonical "stale-tolerant, slightly fresh OK" use case. Stripe Dashboard, Linear, Loki all converge on 5min TTL with jitter.

Short L1 (30s) because:
- Operator/user typing burst (`?q=ac`, `?q=acm`, `?q=acme`) generates different cache keys per stroke; L1 doesn't earn much per-key
- Cross-replica stampede is the bigger risk than per-process repeat

Jitter ±10% mandatory — at >1 API replica, identical 5-minute TTLs would synchronize expiries; jitter desynchronizes.

#### `DashboardTTL` — 1 min L1 / 5 min L2 (NEW)

Operator dashboard stats (`/v1/platform/stats?delta_window=`). Operators refresh dashboards manually; cadence is in minutes. 5min L2 with jitter is Datadog/Stripe canon.

Slightly different L1 from `SearchResultsTTL` (1min vs 30s) because dashboard refreshes are more predictable per-operator → L1 hit ratio per process is higher; longer L1 earns its keep.

### Jitter discipline

Profiles with `Jitter > 0` apply a per-Set randomization to the L2 TTL:

```
actualL2 = baseL2 + rand.Int63n(baseL2 * jitterPercent / 100)
```

Default jitter is ±10% (so a 5-minute L2 becomes 5-5.5 minutes for any given key). Enough to desynchronize replica expiries without making cache lifetime unpredictable for debugging.

L1 does NOT use jitter — L1 is per-process; no cross-replica stampede risk.

### When to add a new profile

Only when the cost/freshness profile genuinely differs from existing groups. Don't add `MyHandlerTTL` because "5min feels right" — use `DefaultTTL` or `DashboardTTL`. New profiles need:

1. A documented reason why existing profiles don't fit
2. Research grounding for the chosen values (canonical reference)
3. Updated row in the table above + ADR amendment

## Consequences

**Positive:**

- **Per-use-case TTLs are research-grounded, not ad-hoc.** Future PRs reach for a named profile, not a raw `time.Duration`.
- **Jitter discipline is built in.** No replica-stampede footgun.
- **The table is the canonical reference.** Reviewers can immediately see "is this cache call using the right profile?".
- **Stamp-rotation invalidation pattern explicit.** Capabilities cache is correct-by-construction because the cache key includes the stamp; TTL is a memory bound, not a correctness one.
- **Microsoft HybridCache canon followed.** Anyone coming from .NET land recognizes the shape.

**Negative:**

- **Five profiles is a small mental tax.** Reviewers need to know which profile to pick. Mitigated by the decision table — pick by use-case, not by feel.
- **No SWR (stale-while-revalidate) semantics.** A cache miss blocks the caller until the factory returns. For LeadKart's scale + use cases (sub-100ms factories), this is fine. SWR is a future enhancement if dashboard p95 latency starts mattering.
- **Jitter makes TTL non-deterministic.** Debugging "why did this cache entry expire at 5:01 instead of 5:00" is harder. Mitigation: jitter is bounded at 10%; log the chosen L2 TTL alongside Set ops at DEBUG.

## Alternatives considered

1. **Single global TTL for everything.** Rejected. Search-result freshness needs ≠ session-validation needs ≠ capabilities-staleness needs. One-size-fits-all forces over-cautious TTLs (everything at 30s) → low hit rate, or over-aggressive (everything at 1h) → stale UX.

2. **Per-caller raw `time.Duration` (no named profiles).** The Wave 1 starting state. Rejected because it lets each PR re-invent the TTL math; no consistency across callers, no documented rationale.

3. **Stale-while-revalidate (SWR).** Considered. Net pattern: cache returns stale on miss + kicks off background refresh. Useful for "users never see latency, even on cache miss". Rejected for v0.2 because:
   - Our factories run in 10-50ms (Postgres reads). The cache-miss latency cost is small.
   - SWR adds operational complexity (background goroutine pool, error handling on background factory failures).
   - HybridCache's `LocalCacheExpiration` mechanism gives most of the SWR benefit (L1 returns stale-ish while L2 might still have fresher) without an explicit SWR knob.
   - Microsoft HybridCache itself didn't ship SWR in .NET 9; .NET 10+ may add it. We'll follow when they do.

4. **Single-tier L2-only across the board.** Rejected. L1 hit ratio is 80%+ on hot paths (per `cache.facade` design); skipping L1 would multiply Redis round-trips by ~5x for no benefit. `WithOmitL1` exists for cases where L1 race-with-invalidation is the dominant concern (SecurityStampCache); not a sensible default.

5. **TTL pulled from config (env var per facade).** Considered. Rejected because operators don't tune cache TTLs at deploy time — these are architectural decisions sealed in code. Config-driven TTLs would invite per-environment drift that's hard to reason about.

## Sources

- [Microsoft HybridCache documentation](https://learn.microsoft.com/en-us/aspnet/core/performance/caching/hybrid?view=aspnetcore-10.0) — `DefaultEntryOptions.Expiration` + `LocalCacheExpiration` canon (1min L1 / 10min L2 typical example).
- [Microsoft HybridCacheEntryOptions.LocalCacheExpiration](https://learn.microsoft.com/en-us/dotnet/api/microsoft.extensions.caching.hybrid.hybridcacheentryoptions.localcacheexpiration?view=net-10.0-pp) — formal API reference for the dual-TTL pattern.
- [Multi-Level Caching with Redis — OneUptime](https://oneuptime.com/blog/post/2026-01-21-redis-multi-level-caching/view) — L1 50ns / L2 0.5ms latency budget + 60s L1 / 300s L2 example.
- [Architecting Multi-Tier Caching on AWS](https://medium.com/@rajeshiyer9944/architecting-multi-tier-caching-on-aws-f096578d9dff) — hot tier 5min / warm tier 24h pattern.
- [Better Auth — Session Management](https://deepwiki.com/better-auth/better-auth/3.3-user-accounts-and-management) — ~5min session/cookie cache rationale.
- ADR 0015 — Caching: ristretto + redis/go-redis/v9 + singleflight (the substrate this ADR layers TTL policy onto).
- ADR 0028 — SecurityStampCache + stale-write fence (the canonical caller using SecurityStampTTL).
- ADR 0040 — Search strategy (cite-back: `?delta_window` closed-set rule prevents cache-key explosion; this ADR adds the TTL for the chosen keys).


**Fitness function:** convention-only — not mechanically expressible. Per-profile TTLs live as constants in `internal/common/cache`; runtime behaviour exercised by unit tests.
