// Package httpmw is the canonical HTTP middleware chain for the
// LeadKart API host.
//
// The chain order (outer → inner) is:
//
//	otelhttp     — outermost; OTel sees the raw request/response.
//	correlation  — extract or mint X-Correlation-ID; stash on ctx;
//	               echo in response header.
//	requestlog   — slog start + end with method, path, status, latency,
//	               correlation_id, tenant_id (if bound).
//	recover      — catch handler panics → 500 + structured log.
//	idempotency  — Stripe-style X-Command-Id replay protection on
//	               POST/PUT/DELETE; transparent on GET.
//	ratelimit    — IP-keyed token bucket on anonymous endpoints;
//	               tenant-keyed on authenticated endpoints (route
//	               applies the tenant variant after auth has bound
//	               the tenant ctx).
//	mux          — http.ServeMux; per-route auth (RequireFreshStamp)
//	               + handlers.
//
// Doctrine:
//
//   - audit-checklist.md §12 "Observability": every request log line
//     carries correlation_id + tenant_id (when bound) so downstream
//     log aggregation can stitch a customer-facing trace from a
//     single ID.
//   - security.md "Rate limiting on every mutating endpoint": IP and
//     tenant rate limits are required; the .NET reference uses the
//     same names. v0.2 ships in-memory token buckets — single-replica
//     correct. v0.3 swaps to a Redis-backed limiter via the same
//     [Limiter] interface.
//   - messaging.md "X-Command-Id accepted on mutating HTTP commands":
//     idempotency middleware is mandatory for write paths.
//   - Mat Ryer 2024 "How I write HTTP services in Go after 13 years":
//     middleware = `func(http.Handler) http.Handler`; composed
//     functionally; no struct-based pipelines.
//
// All middleware here is safe for concurrent use. None mutate the
// request — they read headers + write to ctx + observe responses.
package httpmw
