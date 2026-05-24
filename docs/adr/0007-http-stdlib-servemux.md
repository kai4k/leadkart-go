# ADR 0007 — HTTP router: stdlib `net/http` ServeMux 1.22+

**Status:** Accepted
**Date:** 2026-05-05

## Context

The Go HTTP router landscape shifted in 2024–2026:
- Go 1.22 (Feb 2024) added method + path-parameter routing to stdlib `net/http.ServeMux`. Pattern syntax: `mux.Handle("POST /api/v1/tenants/{id}", h)`.
- gorilla/mux was archived in 2022 and unarchived 2023; no longer canonical.
- chi (go-chi/chi) remains the most-used non-stdlib router (~12% in JetBrains Go survey 2025) — well-maintained but no longer required for basic routing needs.
- gin / echo / fiber remain popular but considered non-idiomatic by the Go-team-adjacent community.

Mat Ryer's [2024 Grafana article](https://grafana.com/blog/2024/02/09/how-i-write-http-services-in-go-after-13-years/) explicitly uses stdlib ServeMux 1.22+ and recommends it as the default for new services.

## Decision

**Stdlib `net/http` ServeMux 1.22+** with the Mat Ryer 2024 NewServer pattern.

```go
func newServer() http.Handler {
    mux := http.NewServeMux()
    mux.Handle("GET /health", handleHealth())
    // ... per-module routes registered via identityhttp.AddRoutes(mux, ...)
    return mux
}
```

**Middleware chain:** function wrapping (closures), not interface chains.

```go
var handler http.Handler = mux
handler = withRecover(handler)
handler = withLogging(handler, logger)
handler = withTenancy(handler)
handler = withRateLimit(handler)
```

**Router escape hatch:** if a future requirement exceeds stdlib ServeMux capability (sub-routers with shared middleware groups; advanced pattern matching), reach for `chi`. Don't speculate now.

## Consequences

**Positive:**
- Zero third-party dep for routing.
- Method + path-param routing built in (Go 1.22+).
- Performance: stdlib ServeMux has been heavily optimised since 1.22.
- Mat Ryer 2024 canonical pattern transfers cleanly.
- Middleware is plain function wrapping — testable, composable, no DSL.

**Negative:**
- No sub-routers or middleware groups built-in. If we need them, we wrap manually (acceptable for v0.1).
- Pattern matching less flexible than chi for edge cases (regex paths, wildcards). Not needed for LeadKart's REST shape.

## Alternatives considered

1. **chi** — second-choice. Add later if middleware groups become painful.
2. **gin / echo** — popular but considered non-idiomatic by Go core; opinionated frameworks lock you in. Rejected.
3. **fiber** — built on fasthttp; not stdlib-compatible; ecosystem incompatibility risk. Rejected.
4. **gorilla/mux** — archived 2022. Rejected.

## Sources

- [Mat Ryer — How I write HTTP services in Go after 13 years (Grafana, Feb 2024)](https://grafana.com/blog/2024/02/09/how-i-write-http-services-in-go-after-13-years/).
- [Go 1.22 release notes — routing enhancements](https://go.dev/blog/routing-enhancements).
- [Eli Bendersky — Better HTTP routing in Go 1.22](https://eli.thegreenplace.net/2023/better-http-server-routing-in-go-122/).
- [Alex Edwards — Which Go Router Should I Use?](https://www.alexedwards.net/blog/which-go-router-should-i-use).
- [JetBrains Go Ecosystem 2025 Trends](https://blog.jetbrains.com/go/2025/11/10/go-language-trends-ecosystem-2025/).


**Fitness function:** convention-only — not mechanically expressible. "Use stdlib net/http" is enforced by `TestArch_NoBannedDeps` indirectly (no chi/gin in go.mod), but the affirmative use is observed in code review rather than a fitness function.
