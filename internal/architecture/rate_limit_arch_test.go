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
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"strconv"
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

// clientAddressReaders are the only files allowed to read the connection's
// address or a client-address forwarding header: everything else asks
// httpmw.ClientIP / ClientKey / ClientIPFromContext, so the trusted-proxy rules
// (and the /64 keying) cannot be bypassed by one more direct read.
var clientAddressReaders = map[string]bool{
	"internal/common/httpmw/clientip.go": true,
}

// forwardingHeaderNames are the headers that carry a client address in front of a
// proxy. Compared exactly and case-insensitively against string literals.
var forwardingHeaderNames = map[string]bool{
	"x-forwarded-for":          true,
	"x-real-ip":                true,
	"forwarded":                true,
	"true-client-ip":           true,
	"cf-connecting-ip":         true,
	"x-client-ip":              true,
	"x-cluster-client-ip":      true,
	"fastly-client-ip":         true,
	"x-original-forwarded-for": true,
}

// clientAddressReads lists the offending reads in one source file: selectors
// named RemoteAddr and string literals naming a forwarding header.
func clientAddressReads(src []byte) []string {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "x.go", src, 0)
	if err != nil {
		return []string{"unparseable: " + err.Error()}
	}
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.SelectorExpr:
			if v.Sel.Name == "RemoteAddr" {
				out = append(out, fmt.Sprintf("line %d: .RemoteAddr", fset.Position(v.Pos()).Line))
			}
		case *ast.BasicLit:
			if v.Kind == token.STRING {
				if s, err := strconv.Unquote(v.Value); err == nil && forwardingHeaderNames[strings.ToLower(strings.TrimSpace(s))] {
					out = append(out, fmt.Sprintf("line %d: header literal %s", fset.Position(v.Pos()).Line, v.Value))
				}
			}
		}
		return true
	})
	return out
}

// TestArch_ClientAddressIsReadOnlyInClientIP pins ADR 0068 D11: r.RemoteAddr and
// the forwarding header names are read in internal/common/httpmw/clientip.go and
// nowhere else in production code.
// Scope: production — non-test files under internal/ and cmd/.
// arch-test:no-negative-fixture (the scanner itself is proven by TestClientAddressScan_FlagsSyntheticOffenders, which feeds it in-memory offenders)
func TestArch_ClientAddressIsReadOnlyInClientIP(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	var bad []string
	for _, dir := range []string{"internal", "cmd"} {
		walkGoFiles(t, filepath.Join(root, dir), false, func(path string, src []byte) {
			rel, err := filepath.Rel(root, path)
			if err != nil || clientAddressReaders[filepath.ToSlash(rel)] {
				return
			}
			for _, r := range clientAddressReads(src) {
				bad = append(bad, filepath.ToSlash(rel)+": "+r)
			}
		})
	}
	if len(bad) > 0 {
		t.Fatalf("the connection address / forwarding headers are read outside httpmw/clientip.go; use httpmw.ClientIP, ClientKey or ClientIPFromContext:\n  %s", strings.Join(bad, "\n  "))
	}
}

func TestClientAddressScan_FlagsSyntheticOffenders(t *testing.T) {
	t.Parallel()
	for name, src := range map[string]string{
		"RemoteAddr":      `package x; import "net/http"; func f(r *http.Request) string { return r.RemoteAddr }`,
		"forwarded for":   `package x; import "net/http"; func f(r *http.Request) string { return r.Header.Get("x-forwarded-for") }`,
		"real ip":         `package x; import "net/http"; func f(r *http.Request) string { return r.Header.Get("X-Real-IP") }`,
		"forwarded":       `package x; import "net/http"; func f(r *http.Request) string { return r.Header.Get("Forwarded") }`,
		"cloudflare":      `package x; import "net/http"; func f(r *http.Request) string { return r.Header.Get("CF-Connecting-IP") }`,
		"through a field": `package x; type s struct{ a struct{ RemoteAddr string } }; func f(v s) string { return v.a.RemoteAddr }`,
	} {
		if len(clientAddressReads([]byte(src))) == 0 {
			t.Errorf("%s: the scan missed an offender", name)
		}
	}
	for name, src := range map[string]string{
		"long message": `package x; var m = "X-Forwarded-For is ignored here"`,
		"clean":        `package x; import "net/http"; func f(r *http.Request) string { return r.Host }`,
	} {
		if got := clientAddressReads([]byte(src)); len(got) != 0 {
			t.Errorf("%s: false positive %v", name, got)
		}
	}
}
