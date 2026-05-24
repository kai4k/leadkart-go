package httpmw

import "net/http"

// SecurityHeaders sets the canonical OWASP-recommended response
// headers on every API response. Per the OWASP Secure Headers Project
// (https://owasp.org/www-project-secure-headers/) the JSON-API floor
// is four headers:
//
//   - X-Content-Type-Options: nosniff
//     Disables MIME-type sniffing on the client. Prevents an attacker
//     from coaxing a JSON response into being interpreted as HTML/JS.
//
//   - X-Frame-Options: DENY
//     Forbids the response from being framed. Defence-in-depth against
//     UI-redress / clickjacking on any HTML surface accidentally served
//     via the API host (e.g. /docs Scalar UI page).
//
//   - Strict-Transport-Security: max-age=31536000; includeSubDomains
//     HSTS one-year posture. Mozilla Observatory A+ floor. Cloud
//     deployments terminate TLS at the front door; the header upgrades
//     the browser's posture on first contact + sticks for a year.
//
//   - Referrer-Policy: no-referrer
//     The API host does not link out. no-referrer is the strict default;
//     downstream BFF (SvelteKit per ADR 0043) may override on its own
//     surface.
//
// Wired in cmd/api/main.go inside the PublicChain — must sit OUTSIDE
// the recover layer so panic-derived 500s also carry the headers.
func SecurityHeaders() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("X-Frame-Options", "DENY")
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
			h.Set("Referrer-Policy", "no-referrer")
			next.ServeHTTP(w, r)
		})
	}
}
