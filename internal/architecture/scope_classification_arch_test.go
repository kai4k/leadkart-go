// scope_classification_arch_test.go — ADR 0068 D7: every registered route has
// ONE declared X-Tenant-Id scope policy, and a route that honours the header
// is authorised by a permission, never by authentication or platform-tenant
// membership alone.
package architecture_test

import (
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/ports/authn"
)

// scopeWritableRoutes is the EXPLICIT set of routes that may write under a
// foreign X-Tenant-Id (authn.TenantScopeWritable). Adding one is a deliberate,
// reviewed decision: every other honoured mutation is 403
// tenant_scope_read_only under a foreign header, which is what keeps an
// operator's own membership id out of another tenant's rows.
var scopeWritableRoutes = []string{
	"DELETE /api/v1/roles/{roleId}",
	"DELETE /api/v1/users/{userId}/manager",
	"DELETE /api/v1/users/{userId}/roles/{roleId}",
	"PATCH /api/v1/roles/{roleId}",
	"PATCH /api/v1/roles/{roleId}/parent",
	"PATCH /api/v1/users/{userId}/permission-overrides",
	"PATCH /api/v1/users/{userId}/profile",
	"POST /api/v1/roles",
	"POST /api/v1/roles/{roleId}/permissions/grant",
	"POST /api/v1/roles/{roleId}/permissions/revoke",
	"POST /api/v1/users",
	"POST /api/v1/users/{userId}/anonymise",
	"POST /api/v1/users/{userId}/deactivate",
	"POST /api/v1/users/{userId}/reactivate",
	"POST /api/v1/users/{userId}/roles",
	"PUT /api/v1/roles/{roleId}/permissions",
	"PUT /api/v1/users/{userId}/manager",
}

const (
	scopeClassIgnore = "IGNORE"
	scopeClassReject = "REJECT"
	scopeClassPath   = "path-tenant"
	scopeClassHonour = "honour"
)

type classifiedRoute struct {
	line     int
	key      string
	class    string
	writable bool
	problems []string
}

var (
	scopeRejectRE     = regexp.MustCompile(`authn\.RejectTenantScope\(`)
	scopePathMarkerRE = regexp.MustCompile(`authn\.TenantScope(MatchesPath|Lifecycle|PathOnly)\(`)
	scopeWritableRE   = regexp.MustCompile(`authn\.TenantScopeWritable\b`)
	scopeHonourRE     = regexp.MustCompile(`authn\.HonourTenantScope\(`)
	scopePlatformRE   = regexp.MustCompile(`authn\.RequirePlatform\(`)
	scopeWildcardRE   = regexp.MustCompile(`\{[^}]+\}`)
)

func exprUses(expr string, aliases map[string]bool) bool {
	for name := range aliases {
		if regexp.MustCompile(`\b` + regexp.QuoteMeta(name) + `\b`).MatchString(expr) {
			return true
		}
	}
	return false
}

// classifyScopeRoutes classifies every mux.Handle in one ports file by
// textual scan (the same technique as the other route arch tests).
func classifyScopeRoutes(text string) []classifiedRoute {
	aliases := authzAliases(text)
	pathAliases := aliasesOf(text, `RequireTenantPermission|TenantScopeMatchesPath|TenantScopeLifecycle|TenantScopePathOnly`)
	writableAliases := aliasesOf(text, `TenantScopeWritable`)
	platformAliases := aliasesOf(text, `RequirePlatform`)

	var out []classifiedRoute
	for _, m := range muxHandleRE.FindAllStringSubmatchIndex(text, -1) {
		key := text[m[2]:m[3]]
		if publicRoutesAllowList[key] {
			continue
		}
		expr := extractMuxHandlerExpr(text, m[1])
		method, path, _ := strings.Cut(key, " ")
		concrete := scopeWildcardRE.ReplaceAllString(path, "x")

		reject := scopeRejectRE.MatchString(expr)
		pathTenant := scopePathMarkerRE.MatchString(expr) || authzPathGateRE.MatchString(expr) || exprUses(expr, pathAliases)
		writable := scopeWritableRE.MatchString(expr) || exprUses(expr, writableAliases)
		marked := reject || pathTenant || writable || scopeHonourRE.MatchString(expr)
		authorised := exprHasAuthz(expr, aliases)
		usesPlatformAnchor := scopePlatformRE.MatchString(expr) || exprUses(expr, platformAliases)

		r := classifiedRoute{line: 1 + strings.Count(text[:m[0]], "\n"), key: key, writable: writable}
		switch {
		case reject:
			r.class = scopeClassReject
		case pathTenant:
			r.class = scopeClassPath
		case authn.ScopeIgnoredByTable(method, concrete) && !marked:
			r.class = scopeClassIgnore
		default:
			r.class = scopeClassHonour
		}
		if r.class == scopeClassHonour && !authorised {
			r.problems = append(r.problems, "honours X-Tenant-Id but is gated by authentication or platform membership only: add a permission gate evaluated in the scoped tenant, or declare the route IGNORE (authn scope_policy table) / REJECT (authn.RejectTenantScope)")
		}
		if usesPlatformAnchor && !authorised {
			r.problems = append(r.problems, "platform-tenant membership (authn.RequirePlatform) is not a permission: use authn.RequirePlatformGate with the permission that authorises the route")
		}
		if writable && r.class != scopeClassHonour {
			r.problems = append(r.problems, "authn.TenantScopeWritable on a route that is not an honoured tenant-plane route ("+r.class+")")
		}
		out = append(out, r)
	}
	return out
}

var authzPathGateRE = regexp.MustCompile(`authn\.RequireTenantPermission\(`)

// TestArch_EveryRouteIsScopeClassified fails when a registered route is not
// exactly one of: IGNORE group, REJECT, path-tenant, or honour + permission
// gate evaluated in the scoped tenant; when a route is gated by
// authentication or platform-tenant membership alone outside the IGNORE and
// REJECT groups (GETs included); or when the set of routes that may write under
// a foreign X-Tenant-Id differs from scopeWritableRoutes.
//
// Scope: production — route registrations live in ports/http.go; test files register none.
//
// Negative coverage: TestScopeClassifier_FlagsUnclassifiedAndUnauthorisedRoutes
// runs the classifier over synthetic offenders (a plain test, so it also runs
// in the unit suite).
//
// arch-test:no-negative-fixture (RED proof: wrap any honoured identity route in `auth(` only, or bind a route to authn.RequirePlatform alone → this test fails; see the synthetic offenders in TestScopeClassifier_FlagsUnclassifiedAndUnauthorisedRoutes)
func TestArch_EveryRouteIsScopeClassified(t *testing.T) {
	t.Parallel()
	var writable []string
	total := 0
	counts := map[string]int{}
	for _, mod := range modulesUnderInternal(t) {
		portsDir := filepath.Join(internalDir(t), mod, "ports")
		walkGoFiles(t, portsDir, false, func(path string, src []byte) {
			slash := pathToSlash(path)
			if !strings.HasSuffix(slash, "/ports/http.go") && !strings.HasSuffix(slash, "_http.go") {
				return
			}
			for _, r := range classifyScopeRoutes(string(src)) {
				total++
				counts[r.class]++
				if r.writable {
					writable = append(writable, r.key)
				}
				for _, p := range r.problems {
					t.Errorf("%s:%d %s [%s] — %s", slash, r.line, r.key, r.class, p)
				}
			}
		})
	}
	if total == 0 || counts[scopeClassHonour] == 0 || counts[scopeClassIgnore] == 0 || counts[scopeClassReject] == 0 || counts[scopeClassPath] == 0 {
		t.Fatalf("classifier found no routes in some class (total=%d %v): the scan is broken", total, counts)
	}
	slices.Sort(writable)
	want := slices.Clone(scopeWritableRoutes)
	slices.Sort(want)
	if !slices.Equal(writable, want) {
		t.Errorf("routes writable under a foreign X-Tenant-Id changed.\n got:  %v\n want: %v\nAdding one is a deliberate decision: update scopeWritableRoutes with the reason in the commit.", writable, want)
	}
}

// TestScopeClassifier_FlagsUnclassifiedAndUnauthorisedRoutes is the negative
// proof for TestArch_EveryRouteIsScopeClassified: each offender below must be
// reported, each compliant route must not.
func TestScopeClassifier_FlagsUnclassifiedAndUnauthorisedRoutes(t *testing.T) {
	t.Parallel()
	src := `package ports
func AddRoutes() {
	auth := authn.RequireFreshStamp(v, s)
	anchor := authn.RequirePlatform(v, s)
	read := authn.RequirePermission(v, s, p.Orders.Read)
	manage := authn.RequirePermission(v, s, p.Orders.Manage)
	gate := authn.RequirePlatformGate(v, s)
	mux.Handle("GET /api/v1/orders/authonly", auth(h))
	mux.Handle("GET /api/v1/orders/anchoronly", anchor(h))
	mux.Handle("GET /api/v1/platform/stats-anchor", anchor(h))
	mux.Handle("POST /api/v1/orders/ok", manage(h))
	mux.Handle("GET /api/v1/orders/ok", read(h))
	mux.Handle("GET /api/v1/platform/stats-ok", gate(p.Platform.TenantsView)(h))
	mux.Handle("GET /api/v1/auth/sessions", auth(h))
	mux.Handle("POST /api/v1/permission-requests", authn.RejectTenantScope(auth(h)))
	mux.Handle("POST /api/v1/orders/writable-ignored", authn.TenantScopeWritable(authn.RejectTenantScope(manage(h))))
}`
	got := map[string][]string{}
	for _, r := range classifyScopeRoutes(src) {
		got[r.key] = r.problems
	}
	for _, key := range []string{
		"GET /api/v1/orders/authonly",          // honoured, authentication only
		"GET /api/v1/orders/anchoronly",        // honoured, platform membership only
		"GET /api/v1/platform/stats-anchor",    // IGNORE group but membership-only authorisation
		"POST /api/v1/orders/writable-ignored", // writable on a REJECT route
	} {
		if len(got[key]) == 0 {
			t.Errorf("%s must be flagged", key)
		}
	}
	for _, key := range []string{
		"POST /api/v1/orders/ok", "GET /api/v1/orders/ok", "GET /api/v1/platform/stats-ok",
		"GET /api/v1/auth/sessions", "POST /api/v1/permission-requests",
	} {
		if p, ok := got[key]; !ok || len(p) != 0 {
			t.Errorf("%s must be classified without problems, got %v (present=%v)", key, p, ok)
		}
	}
}
