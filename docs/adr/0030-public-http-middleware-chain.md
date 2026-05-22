# ADR 0030 — Canonical public-API HTTP middleware chain

**Status:** Accepted
**Date:** 2026-05-08

## Context

`cmd/api` hosts the public HTTP API. Pre-PR the only middleware was `otelhttp.NewHandler` wrapping the mux — no panic recovery, no correlation IDs, no request logs, no rate limiting, and the existing `idempotency.Middleware` package was built but unwired.

`audit-checklist.md §12` requires every request log line to carry correlation_id + tenant_id (when bound). `security.md` requires rate limiting on every mutating endpoint. `messaging.md` requires `X-Command-Id` replay protection. Each of these had a separate "we'll wire it later" note attached.

There's also an order-of-application question: a panic-recovery middleware that runs *outside* the request logger never gets logged; rate-limiting *after* JWT verification means we burn cycles verifying tokens for traffic we'd reject anyway; idempotency *before* tenant context can't write a tenant-scoped audit row.

## Decision

**One canonical middleware chain composed in `internal/common/httpmw.PublicChain`**, applied as a single call in `cmd/api/main.go` inside the existing `otelhttp` wrapper.

Order (outer → inner, after `otelhttp`):

```
otelhttp        ← OTel sees raw request/response (already wired, unchanged)
  Correlation   ← read X-Correlation-ID or mint UUIDv7; stash on ctx; echo
    RequestLog  ← slog start/end with method/path/status/latency/bytes/IDs
      Recover   ← catch panics → 500 + structured log + correlation_id
        IPRateLimit  ← per-IP token bucket (x/time/rate); 10rps/60burst
          Idempotency ← X-Command-Id replay protection (Stripe-style)
            mux        ← ports.AddRoutes; per-route auth lives here
              auth     ← RequireFreshStamp gates per-route (ADR 0028)
                handler
```

**Why this order:**

- **Correlation outermost** so every log line on the request — including panic logs from Recover — carries the same ID. Outermost-correlation also means client SDKs (browser Network tab, curl `-i`) can pin a single ID across the request lifecycle for support escalations.
- **RequestLog second** so it emits the canonical "http request" log line at request end with the *final* status (recovered 500s log as 5xx ERROR; rate-limited 429s log as 4xx WARN; normal 2xx INFO).
- **Recover third** so panics from any inner layer are caught with the correct correlation_id + a structured stack trace; re-panics on `http.ErrAbortHandler` per stdlib canon.
- **IPRateLimit before auth** so unauthenticated brute-force attempts hit the limiter before we burn cycles on JWT verification.
- **Idempotency before mux** so the replay-store consult precedes per-route work; the per-tenant audit row writes from inside the handler with a tenant context already bound by auth.
- **Per-route auth (RequireFreshStamp + RequirePermission/etc.) inside the mux**, not at chain level, because some routes are anonymous (login, refresh, password-reset, public health) and would 401 incorrectly under a global auth middleware.

`httpmw.Chain(outer, ..., inner)` is the composer. `httpmw.PublicChain(cfg)` is the production preset. Tests can construct ad-hoc chains via `Chain(...)` directly.

Severity policy on `RequestLog`: INFO on 2xx-3xx, WARN on 4xx, ERROR on 5xx — matches slog convention where Error means "operator should investigate."

Rate limiter shape: in-memory token bucket via `golang.org/x/time/rate`, keyed by `IPLimiterKeyer` (extracts the IP from `r.RemoteAddr`; does NOT trust `X-Forwarded-For` without an upstream proxy allowlist). v0.2 single-replica acceptable; v0.3 swaps to a Redis-backed limiter via the same `LimiterKeyer` interface. `X-Forwarded-For` trust lands alongside the Redis swap with a configured trusted-proxy CIDR.

## Consequences

**Positive:**

- One place to look for the request lifecycle. Adding a new global concern (e.g. CSRF, or a tenant-keyed rate limit variant) means one edit.
- Every request log carries correlation + tenant + structured fields — matches `audit-checklist.md §12` line-by-line.
- Panics surface as a structured 500 with the same correlation as the rest of the request — operators stitch a single trace from the same ID.
- Idempotency is now enforced (was unwired pre-PR).

**Negative:**

- In-memory rate limiter is single-replica-correct only. Documented; v0.3 follows.
- The chain composition runs five middleware per request. At µs each on the hot path it's negligible compared to the JWT HMAC + DB lookup, but the layers are visible in CPU profiles. Mitigated: each layer is small, allocates nothing on the steady-state path, and exits early when its check is irrelevant.
- The order becomes load-bearing — tests assert behaviour assuming a specific composition. Mitigated: package doc + chain composer keep the order explicit.

## Alternatives considered

1. **Per-route middleware composition.** Rejected: every route would compose its own chain; trivial to drift between handlers. The global pieces (correlation, log, recover, ratelimit, idempotency) belong at the host level, not the route level.
2. **Recover outermost.** Rejected: panics in Recover or RequestLog should still be caught — by stdlib's `http.Server` recovery — but the request log line + correlation log line never fire if Recover's inner setup itself panics. The correlation+log layers don't allocate or call user code, so panicking there is a programming bug worth surfacing as a stdlib-level connection drop, not a clean 500.
3. **Single struct-based middleware (chi-style).** Rejected: stdlib `func(http.Handler) http.Handler` composes fine; introducing chi (or similar) for the chain shape alone isn't worth a new dep.
4. **Per-tenant rate limit at chain level.** Considered. Rejected for v0.2: tenant ID is bound by per-route auth, so chain-level access doesn't have it. A per-route rate-limit variant (composed after auth) lands in v0.3 alongside the Redis swap.

## Sources

- [Mat Ryer 2024 — "How I write HTTP services in Go after 13 years"](https://grafana.com/blog/2024/02/09/how-i-write-http-services-in-go-after-13-years/) — `func(http.Handler) http.Handler` middleware shape; functional composition.
- [Stripe — "Designing robust and predictable APIs with idempotency"](https://stripe.com/blog/idempotency) (2018) — `Idempotency-Key` semantics LeadKart ports as `X-Command-Id`.
- LeadKart `.NET .claude/rules/audit-checklist.md §12` (Observability), `security.md` (Rate limiting + auth ordering), `messaging.md` (X-Command-Id mandatory on writes).
- [`golang.org/x/time/rate`](https://pkg.go.dev/golang.org/x/time/rate) — token-bucket reference.
- [OWASP "Token Theft & Replay"](https://owasp.org/www-project-api-security/) — reason `X-Forwarded-For` is NOT trusted without an upstream proxy allowlist.
