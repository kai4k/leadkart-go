// rate_limit_arch_test.go — Principle O: Rate limiting.
//
// Public endpoints WITHOUT rate limiting are credential-stuffing
// catnip. The httpmw chain wires an IP-level limiter; the tests
// here assert it's present, configured from real config (not
// defaults baked in literals), and the 429 path carries
// `Retry-After`.
//
// Cited canon:
//   - OWASP API4:2023 — Unrestricted Resource Consumption
//   - RFC 6585 §4 (429 Too Many Requests) — Retry-After REQUIRED-ish
//   - Cloudflare / Stripe public-API rate-limit shape

package architecture_test

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// ----------------------------------------------------------------------------
// O1: TestArch_IPRateLimitMiddlewarePresent
// ----------------------------------------------------------------------------
//
// The composition root's middleware chain MUST include the IP-level
// rate-limiter (`httpmw.IPRateLimit` or equivalent). Drift here
// would re-open the credential-stuffing path.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_IPRateLimitMiddlewarePresent(t *testing.T) {
	t.Parallel()

	mainPath := filepath.Join(repoRoot(t), "cmd", "api", "main.go")
	src, err := readFileBytes(mainPath)
	if err != nil {
		t.Fatalf("read cmd/api/main.go: %v", err)
	}
	body := string(src)
	if !strings.Contains(body, "IPRateLimit") &&
		!strings.Contains(body, "RatePerSecond") {
		t.Fatal("cmd/api/main.go: middleware chain has no IPRateLimit / RatePerSecond ref — credential-stuffing risk")
	}
}

// ----------------------------------------------------------------------------
// O2: TestArch_429ResponseHasRetryAfter
// ----------------------------------------------------------------------------
//
// Every site that writes HTTP 429 MUST set the `Retry-After` header
// (RFC 6585 §4 + Stripe/Cloudflare best practice). Without it
// callers can't back off intelligently.
//
// Predicate: every file that contains `StatusTooManyRequests` (or
// the integer 429 inside an http response) must also contain
// `Retry-After`.
//
// Scope: production — Retry-After header is a wire-contract concern;
// test code that constructs 429 responses inline (e.g. fakes) is
// allowed to skip the header for brevity.
func TestArch_429ResponseHasRetryAfter(t *testing.T) {
	t.Parallel()

	root := internalDir(t)
	var bad []string

	walkGoFiles(t, root, false, func(path string, src []byte) {
		body := string(src)
		if !strings.Contains(body, "StatusTooManyRequests") {
			return
		}
		if !strings.Contains(body, "Retry-After") {
			bad = append(bad, pathToSlash(path))
		}
	})

	if len(bad) > 0 {
		t.Fatalf("returns 429 without Retry-After header (RFC 6585 §4 + Stripe/Cloudflare canon):\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// ----------------------------------------------------------------------------
// O3: TestArch_RateLimitConfigExplicit
// ----------------------------------------------------------------------------
//
// Rate-limit config (RPS + burst) MUST be sourced from config, not
// hard-coded literals. Hard-coded defaults survive "lower the limit"
// PRs silently (the new config field is wired but the literal
// shadows it).
//
// Predicate: every file referencing `RatePerSecond` or `Burst` AND
// `httpmw.LimiterConfig` must reference `cfg.` or `config.` — i.e.
// the value comes from config.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_RateLimitConfigExplicit(t *testing.T) {
	t.Parallel()

	root := internalDir(t)
	var bad []string

	walkGoFiles(t, root, false, func(path string, src []byte) {
		body := string(src)
		if !strings.Contains(body, "LimiterConfig") && !strings.Contains(body, "IPRateLimit") {
			return
		}
		// Allow the substrate (where LimiterConfig is DEFINED).
		if strings.Contains(pathToSlash(path), "/common/httpmw/") {
			return
		}
		if !strings.Contains(body, "cfg.") && !strings.Contains(body, "config.") {
			bad = append(bad, pathToSlash(path))
		}
	})

	if len(bad) > 0 {
		t.Fatalf("LimiterConfig populated without cfg.* — hard-coded defaults shadow config:\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// (compile-anchor for regexp import — used by sibling files)
var _ = regexp.MustCompile
