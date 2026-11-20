package authn

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/leadkart/leadkart-go/internal/common/idempotency"
)

// IdempotencyCallerKeyer scopes X-Command-Id records (ADR 0068 D11).
//
// The idempotency middleware runs in the host chain, before any route's own
// authentication, so the request context carries no tenant yet. Scoping by that
// context (the old default) put every caller behind the BFF into one
// "anon:<bff address>" namespace: tenant A and tenant B choosing the same key on
// the same body were served each other's stored response.
//
// This keyer verifies the bearer token itself (signature, expiry and algorithm
// only; stamp freshness, session liveness and permissions remain the route's job,
// and a refused request is never stored because only 2xx answers are) and returns:
//
//	tenant:<tenant_id>:<hash(person, membership, impersonation session, METHOD path)>
//
// so one tenant, one person (membership) and one route can never replay or collide
// with another's. An unverifiable or absent token falls back to
//
//	anon:<client ip>:<hash(METHOD path)>
//
// where clientIP is the trusted-proxy client address (httpmw.ClientIP). A token the
// verifier rejects never borrows the tenant its unverified claims name. The hash
// keeps the scope at a fixed width under the store's 200-byte limit. An empty
// result (no token, no client address) makes the middleware answer 400.
func IdempotencyCallerKeyer(verifier Verifier, clientIP func(*http.Request) string) idempotency.CallerKeyer {
	if verifier == nil {
		panic("authn: IdempotencyCallerKeyer verifier required")
	}
	if clientIP == nil {
		panic("authn: IdempotencyCallerKeyer clientIP required")
	}
	return func(r *http.Request) string {
		route := r.Method + " " + r.URL.Path
		if token := extractBearer(r.Header); token != "" {
			if c, err := verifier.Verify(token); err == nil && c != nil && strings.TrimSpace(c.TenantID) != "" {
				session := ""
				if c.Act != nil {
					session = c.Act.SessionID
				}
				return "tenant:" + strings.TrimSpace(c.TenantID) + ":" +
					scopeHash(c.Subject, c.MembershipID, session, route)
			}
		}
		ip := clientIP(r)
		if ip == "" {
			return ""
		}
		return "anon:" + ip + ":" + scopeHash(route)
	}
}

// scopeHash is a collision-safe 128-bit digest of the parts (NUL-separated, so
// ("ab","c") and ("a","bc") differ), hex-encoded.
func scopeHash(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		_, _ = h.Write([]byte(p))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}
