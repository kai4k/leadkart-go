package httpmw

import (
	"net/http"
	"time"
)

// RouteRateLimits maps exact "METHOD /path" keys to tighter buckets.
type RouteRateLimits map[string]LimiterConfig

// RouteRateLimit applies a per-route, per-client token bucket on top of the
// global limiter for credential and abuse-prone anonymous endpoints (login,
// refresh, password reset, email-change confirm, tenant registration).
//
// Matching is exact on r.Method and r.URL.Path (the decoded path, which is what
// ServeMux routes on): these routes have no path wildcards, and a near-miss path
// is a 404 from the mux, not a way around the limit. Each route has its own
// buckets, keyed by keyer (the forwarded client, see [ClientIPKeyer]).
func RouteRateLimit(routes RouteRateLimits, keyer LimiterKeyer) Middleware {
	return routeRateLimit(routes, keyer, time.Now)
}

func routeRateLimit(routes RouteRateLimits, keyer LimiterKeyer, now func() time.Time) Middleware {
	if keyer == nil {
		panic("httpmw: RouteRateLimit keyer required")
	}
	limiters := make(map[string]*RateLimiter, len(routes))
	for route, cfg := range routes {
		limiters[route] = newRateLimiter(cfg, keyer, now)
	}
	return func(next http.Handler) http.Handler {
		wrapped := make(map[string]http.Handler, len(limiters))
		for route, l := range limiters {
			wrapped[route] = l.Middleware()(next)
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if h, ok := wrapped[r.Method+" "+r.URL.Path]; ok {
				h.ServeHTTP(w, r)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
