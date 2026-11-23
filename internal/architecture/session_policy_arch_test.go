// session_policy_arch_test.go — plan D8 (forced password change) and D9
// (impersonation reach): the per-route session policies are declared once, in
// authn's session policy, and these gates prove them against EVERY registered
// route, not against a hand-kept list of routes.
//
// The route list comes from the shared scanner (route_scan_test.go); the
// behaviour comes from serving each scanned route through the real route
// registrations of every module (their AddRoutes, zero-value applications: a
// request that passes its gate may fail inside the handler, which only means the
// gate let it through).
package architecture_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/httpmw"
	companysettingsapp "github.com/leadkart/leadkart-go/internal/companysettings/app"
	companysettingsports "github.com/leadkart/leadkart-go/internal/companysettings/ports"
	crmapp "github.com/leadkart/leadkart-go/internal/crm/app"
	crmports "github.com/leadkart/leadkart-go/internal/crm/ports"
	dispatchapp "github.com/leadkart/leadkart-go/internal/dispatch/app"
	dispatchports "github.com/leadkart/leadkart-go/internal/dispatch/ports"
	identityapp "github.com/leadkart/leadkart-go/internal/identity/app"
	"github.com/leadkart/leadkart-go/internal/identity/app/jwt"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	identityports "github.com/leadkart/leadkart-go/internal/identity/ports"
	"github.com/leadkart/leadkart-go/internal/identity/ports/authn"
	inventoryapp "github.com/leadkart/leadkart-go/internal/inventory/app"
	inventoryports "github.com/leadkart/leadkart-go/internal/inventory/ports"
	notificationsapp "github.com/leadkart/leadkart-go/internal/notifications/app"
	notificationsports "github.com/leadkart/leadkart-go/internal/notifications/ports"
	ordersapp "github.com/leadkart/leadkart-go/internal/orders/app"
	ordersports "github.com/leadkart/leadkart-go/internal/orders/ports"
	platformapp "github.com/leadkart/leadkart-go/internal/platform/app"
	platformports "github.com/leadkart/leadkart-go/internal/platform/ports"
	tasksapp "github.com/leadkart/leadkart-go/internal/tasks/app"
	tasksports "github.com/leadkart/leadkart-go/internal/tasks/ports"
)

// anonymousAPIRoutes is the PINNED set of /api/v1 routes that never pass through
// the stamp check (no bearer token): credential exchange and the emailed-token
// flows. They are the only routes a must_change_password token is not judged on,
// because they do not look at a token at all.
var anonymousAPIRoutes = []string{
	"POST /api/v1/auth/confirm-email-change",
	"POST /api/v1/auth/login",
	"POST /api/v1/auth/logout",
	"POST /api/v1/auth/refresh",
	"POST /api/v1/auth/request-password-reset",
	"POST /api/v1/auth/reset-password",
}

// tokenFake serves one claims set for the bearer "tok".
type tokenFake struct{ claims *jwt.Claims }

func (v tokenFake) Verify(tok string) (*jwt.Claims, error) {
	if tok != "tok" {
		return nil, errors.New("unknown token")
	}
	return v.claims, nil
}

// liveValidator vouches for every stamp and every impersonation session.
type liveValidator struct{}

func (liveValidator) IsFresh(context.Context, string, string) (bool, error) { return true, nil }

// IsSessionLive makes liveValidator a authn.SessionValidator: every session is live.
func (liveValidator) IsSessionLive(context.Context, string, string, tenant.ID) (bool, error) {
	return true, nil
}

// allRoutesMux registers every module's routes exactly as cmd/api newServer
// does (same AddRoutes), against zero-value applications.
func allRoutesMux(verifier authn.Verifier, validator authn.StampValidator) http.Handler {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mux := http.NewServeMux()
	identityports.AddRoutes(mux, log, identityapp.Application{}, verifier, validator)
	platformports.AddRoutes(mux, log, platformapp.Application{}, verifier, validator)
	inventoryports.AddRoutes(mux, log, inventoryapp.Application{}, verifier, validator)
	crmports.AddRoutes(mux, log, crmapp.Application{}, verifier, validator)
	tasksports.AddRoutes(mux, log, tasksapp.Application{}, verifier, validator)
	dispatchports.AddRoutes(mux, log, dispatchapp.Application{}, verifier, validator)
	ordersports.AddRoutes(mux, log, ordersapp.Application{}, verifier, validator)
	notificationsports.AddRoutes(mux, log, notificationsapp.Application{}, verifier, validator)
	companysettingsports.AddRoutes(mux, log, companysettingsapp.Application{}, verifier, validator)
	return httpmw.Recover(log)(mux)
}

// serveRoute sends one request for a scanned route (wildcards filled with a
// UUID) with the bearer "tok" and returns the response.
func serveRoute(t *testing.T, h http.Handler, reg routeReg) *httptest.ResponseRecorder {
	t.Helper()
	path := scopeWildcardRE.ReplaceAllString(reg.Path, "0190a000-0000-7000-8000-000000000001")
	req := httptest.NewRequestWithContext(t.Context(), reg.Method, path, strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// apiRoutes returns every scanned /api/v1 route, failing the test on a scanner
// offence or an implausibly small scan.
func apiRoutes(t *testing.T) []routeReg {
	t.Helper()
	routes, offences := scanAllRoutes(t)
	failOnOffences(t, offences)
	var out []routeReg
	for _, reg := range routes {
		if strings.HasPrefix(reg.Path, "/api/v1/") {
			out = append(out, reg)
		}
	}
	if len(out) < 50 {
		t.Fatalf("scanner found only %d /api/v1 routes: the scan is broken", len(out))
	}
	return out
}

func tenantClaims() *jwt.Claims {
	c := &jwt.Claims{
		TenantID: "0190a000-0000-7000-8000-000000000001", TenantSlug: "acme",
		MembershipID: "0190a000-0000-7000-8000-0000000000aa", SecurityStamp: "stamp",
	}
	c.Subject = "0190a000-0000-7000-8000-0000000000ff"
	return c
}

func isCode(rec *httptest.ResponseRecorder, status int, code string) bool {
	return rec.Code == status && strings.Contains(rec.Body.String(), `"`+code+`"`)
}

// TestArch_MustChangePassword_BlocksEveryRouteButThePinnedAllowlist (plan D8):
// for EVERY registered /api/v1 route, a token carrying must_change_password
//   - is answered 403 password_change_required when the route is authenticated
//     and not on authn.PendingPasswordChangeAllowlist;
//   - reaches the handler on the allowlist (change-password, own capabilities);
//   - is never blocked on the pinned anonymous routes (they do not read a token).
//
// It also pins the anonymous set, asserts every allowlist entry is a registered
// authenticated route, and forbids a route registered with authn.RequireAuth
// alone (it would skip the stamp check, and with it the forced-change check).
//
// Scope: production — every route registered by every module's ports and cmd/api.
//
// arch-test:no-negative-fixture (RED proof: register any route with authn.RequireAuth, or add an entry to the allowlist → this test fails)
func TestArch_MustChangePassword_BlocksEveryRouteButThePinnedAllowlist(t *testing.T) {
	t.Parallel()
	flagged := tenantClaims()
	flagged.MustChangePassword = true
	h := allRoutesMux(tokenFake{flagged}, liveValidator{})

	allow := authn.PendingPasswordChangeAllowlist()
	var anonymous []string
	registered := map[string]bool{}
	for _, reg := range apiRoutes(t) {
		registered[reg.Pattern] = true
		if reg.has(tagBareAuth) {
			t.Errorf("%s:%d %s is registered with authn.RequireAuth alone: it skips the stamp check and so the forced-password-change check; use RequireFreshStamp or a permission gate", reg.File, reg.Line, reg.Pattern)
		}
		rec := serveRoute(t, h, reg)
		blocked := isCode(rec, http.StatusForbidden, "password_change_required")
		switch {
		case reg.firstGate() < 0:
			anonymous = append(anonymous, reg.Pattern)
			if blocked {
				t.Errorf("%s is anonymous but answered password_change_required", reg.Pattern)
			}
		case slices.Contains(allow, reg.Pattern):
			if blocked {
				t.Errorf("%s is on the allowlist but was blocked: %d %s", reg.Pattern, rec.Code, rec.Body.String())
			}
		case !blocked:
			t.Errorf("%s:%d %s: a must_change_password token got %d %s; want 403 password_change_required (every authenticated route not on the allowlist is blocked)", reg.File, reg.Line, reg.Pattern, rec.Code, rec.Body.String())
		}
	}
	slices.Sort(anonymous)
	if !slices.Equal(anonymous, anonymousAPIRoutes) {
		t.Errorf("anonymous /api/v1 routes changed.\n got:  %v\n want: %v\nA route that does not read a token escapes the forced change; adding one is a reviewed decision (anonymousAPIRoutes).", anonymous, anonymousAPIRoutes)
	}
	for _, entry := range allow {
		if !registered[entry] {
			t.Errorf("allowlist entry %q is not a registered route: remove it", entry)
		}
		if slices.Contains(anonymousAPIRoutes, entry) {
			t.Errorf("allowlist entry %q is anonymous: it needs no entry", entry)
		}
	}
}

// impersonationRefusedOutsideThePlatformPlane is the exact list of routes, outside
// /api/v1/platform/, an impersonation token is refused on (plan D9). It restates
// authn.ImpersonationRefusals on purpose: changing the policy is a deliberate edit
// of both, reviewed together.
var impersonationRefusedOutsideThePlatformPlane = []string{
	"DELETE /api/v1/auth/sessions",
	"DELETE /api/v1/auth/sessions/{familyId}",
	"GET /api/v1/auth/me/activity",
	"GET /api/v1/auth/sessions",
	"GET /api/v1/search",
	"POST /api/v1/auth/change-password",
	"POST /api/v1/auth/request-email-change",
	"POST /api/v1/permission-requests",
	"POST /api/v1/permission-requests/{requestId}/approve",
	"POST /api/v1/permission-requests/{requestId}/cancel",
	"POST /api/v1/permission-requests/{requestId}/deny",
	"POST /api/v1/tenants",
	"POST /api/v1/tenants/{tenantId}/activate",
	"POST /api/v1/tenants/{tenantId}/suspend",
}

// impersonationOpenSelfService are the routes with no permission gate that an
// impersonation token may still reach: the session's own capabilities (the
// front-end renders the session from them) and the two permission-request reads
// (read-only, empty for the synthetic member). Every OTHER route that acts on "the
// caller" without a permission gate must be on the refusal list.
var impersonationOpenSelfService = []string{
	"GET /api/v1/auth/me/capabilities",
	"GET /api/v1/permission-requests",
	"GET /api/v1/permission-requests/{requestId}",
}

// impersonationOpenPlatformReads restates authn.ImpersonationOpenPlatformReads on
// purpose: the two tenant-plane reads that live under /api/v1/platform/ but are
// gated by tenant permissions, and so stay open to a session. Exactly these.
var impersonationOpenPlatformReads = []string{
	"GET /api/v1/platform/marketplace/leads",
	"GET /api/v1/platform/lead-credits/balance",
}

// impersonationOpenAuthRoutes are the authenticated /api/v1/auth/ routes an
// impersonation token may reach. Every other authenticated route under that prefix
// is on the refusal list. A route is in exactly one of the two sets: refused (the
// list) or open (here); a new one in neither fails the test, whatever its gate.
var impersonationOpenAuthRoutes = []string{
	"GET /api/v1/auth/me/capabilities",
}

// undecidedAuthRoutes returns the authenticated /api/v1/auth/ patterns that are in
// neither or in both of the two sets. Anonymous routes (login, refresh, ...) read no
// token and are pinned by anonymousAPIRoutes instead.
func undecidedAuthRoutes(patterns []string, refused func(string) bool, open []string) (neither, both []string) {
	for _, pattern := range patterns {
		if _, path, _ := strings.Cut(pattern, " "); !strings.HasPrefix(path, "/api/v1/auth/") {
			continue
		}
		r, o := refused(pattern), slices.Contains(open, pattern)
		switch {
		case !r && !o:
			neither = append(neither, pattern)
		case r && o:
			both = append(both, pattern)
		}
	}
	return neither, both
}

// A future POST /auth/mfa/enroll or /auth/api-keys gated by a permission that
// tenant.admin implies is not a "self-service route with no gate"; it still has to
// be decided. The check is over the pattern list, so a synthetic offender proves it.
func TestUndecidedAuthRoutes_FlagsEveryRouteInNeitherOrBothSets(t *testing.T) {
	t.Parallel()
	patterns := []string{
		"GET /api/v1/auth/me/capabilities",  // open
		"GET /api/v1/auth/sessions",         // refused
		"POST /api/v1/auth/mfa/enroll",      // neither: a new gated route
		"POST /api/v1/auth/api-keys",        // neither
		"GET /api/v1/users",                 // not under /auth/: out of scope
		"POST /api/v1/auth/change-password", // refused AND (wrongly) open below
	}
	open := append(slices.Clone(impersonationOpenAuthRoutes), "POST /api/v1/auth/change-password")
	neither, both := undecidedAuthRoutes(patterns, authn.ImpersonationRefused, open)
	if want := []string{"POST /api/v1/auth/mfa/enroll", "POST /api/v1/auth/api-keys"}; !slices.Equal(neither, want) {
		t.Errorf("neither = %v; want %v", neither, want)
	}
	if want := []string{"POST /api/v1/auth/change-password"}; !slices.Equal(both, want) {
		t.Errorf("both = %v; want %v", both, want)
	}
	if n, b := undecidedAuthRoutes(patterns[:2], authn.ImpersonationRefused, impersonationOpenAuthRoutes); len(n)+len(b) != 0 {
		t.Errorf("decided routes flagged: %v %v", n, b)
	}
}

// TestArch_ImpersonationToken_RefusedExactlyWhereTheListSays (plan D9): for EVERY
// registered /api/v1 route, an impersonation token (a live session carrying
// tenant.admin, i.e. Admin-equivalent authority that would satisfy the permission
// gates)
//   - is answered 403 impersonation_not_allowed on the platform plane (less the
//     two pinned tenant-plane reads) and on the pinned list, and only there;
//   - for every authenticated /api/v1/auth/ route, whatever its gate, finds it in
//     exactly one of two pinned sets (refused / open), so a new one must be decided;
//   - is refused on every platform-anchored route, wherever it lives;
//   - is refused on every self-service route (no permission gate) except the
//     pinned open ones, so a new self-service route has to be decided.
//
// The list is also pinned exactly and every entry must be a registered route.
//
// Scope: production — every route registered by every module's ports and cmd/api.
//
// arch-test:no-negative-fixture (RED proof: add a route under authn.RequirePlatform outside /api/v1/platform/, or a self-service route without a decision → this test fails)
func TestArch_ImpersonationToken_RefusedExactlyWhereTheListSays(t *testing.T) {
	t.Parallel()
	imp := tenantClaims()
	imp.Permissions = []string{"tenant.admin"}
	imp.Act = &jwt.ActClaim{Sub: imp.Subject, SessionID: "0190a000-0000-7000-8000-0000000000e1", Reason: "architecture test session"}
	h := allRoutesMux(tokenFake{imp}, liveValidator{})

	if got := authn.ImpersonationRefusals(); !slices.Equal(got, impersonationRefusedOutsideThePlatformPlane) {
		t.Errorf("authn.ImpersonationRefusals changed.\n got:  %v\n want: %v\nChanging what an impersonation session may reach is a reviewed decision: update both lists and the ADR.", got, impersonationRefusedOutsideThePlatformPlane)
	}
	if got := authn.ImpersonationOpenPlatformReads(); !slices.Equal(got, impersonationOpenPlatformReads) {
		t.Errorf("authn.ImpersonationOpenPlatformReads changed.\n got:  %v\n want: %v\nThe exemptions from the platform-plane refusal are a reviewed decision: update both lists and the ADR.", got, impersonationOpenPlatformReads)
	}
	registered := map[string]bool{}
	var authenticatedAuth []string
	for _, reg := range apiRoutes(t) {
		registered[reg.Pattern] = true
		if reg.firstGate() >= 0 {
			authenticatedAuth = append(authenticatedAuth, reg.Pattern)
		}
		want := authn.ImpersonationRefused(reg.Pattern)
		rec := serveRoute(t, h, reg)
		got := isCode(rec, http.StatusForbidden, "impersonation_not_allowed")
		anonymous := reg.firstGate() < 0
		switch {
		case anonymous:
			if got {
				t.Errorf("%s is anonymous but answered impersonation_not_allowed", reg.Pattern)
			}
			continue
		case got != want:
			t.Errorf("%s:%d %s: impersonation token got %d %s; refused = %v, want %v", reg.File, reg.Line, reg.Pattern, rec.Code, rec.Body.String(), got, want)
		}
		if slices.Contains(impersonationOpenPlatformReads, reg.Pattern) && (reg.has(tagAnchor) || !reg.has(tagAuthz)) {
			t.Errorf("%s:%d %s is exempt from the platform-plane refusal but is not a plain tenant-permission route", reg.File, reg.Line, reg.Pattern)
		}
		if reg.has(tagAnchor) && !want {
			t.Errorf("%s:%d %s is platform-anchored but an impersonation token is not refused on it", reg.File, reg.Line, reg.Pattern)
		}
		if !reg.has(tagAuthz) && !reg.has(tagAnchor) && !want && !slices.Contains(impersonationOpenSelfService, reg.Pattern) {
			t.Errorf("%s:%d %s is a self-service route (no permission gate) an impersonation token may reach: refuse it (authn.impersonationRefusedRoutes) or pin it in impersonationOpenSelfService with a reason", reg.File, reg.Line, reg.Pattern)
		}
	}
	for _, entry := range impersonationRefusedOutsideThePlatformPlane {
		if !registered[entry] {
			t.Errorf("refusal-list entry %q is not a registered route: remove it", entry)
		}
		if _, path, _ := strings.Cut(entry, " "); strings.HasPrefix(path, "/api/v1/platform/") {
			t.Errorf("refusal-list entry %q is under the platform prefix, which is refused as a whole: remove it", entry)
		}
	}
	neither, both := undecidedAuthRoutes(authenticatedAuth, authn.ImpersonationRefused, impersonationOpenAuthRoutes)
	for _, p := range neither {
		t.Errorf("%s is an authenticated /api/v1/auth/ route in neither list: refuse it for a session (authn.impersonationRefusedRoutes) or pin it in impersonationOpenAuthRoutes with a reason", p)
	}
	for _, p := range both {
		t.Errorf("%s is both refused and pinned open: decide once", p)
	}
	for _, entry := range impersonationOpenPlatformReads {
		if !registered[entry] {
			t.Errorf("impersonationOpenPlatformReads entry %q is not a registered route: remove it", entry)
		}
	}
	for _, entry := range impersonationOpenAuthRoutes {
		if !registered[entry] {
			t.Errorf("impersonationOpenAuthRoutes entry %q is not a registered route: remove it", entry)
		}
	}
	for _, entry := range impersonationOpenSelfService {
		if !registered[entry] {
			t.Errorf("impersonationOpenSelfService entry %q is not a registered route: remove it", entry)
		}
	}
}

// TestArch_NoProductionCodeImportsATestHelperPackage: the *test helper packages
// (identitytest and its siblings) hold fixtures that reach into the database and
// clear security state (identitytest.ClearMustChangePassword); production code
// must never link them. A non-_test.go file imports one only from inside another
// *test helper package.
//
// Scope: production — every non-test file under internal/ and cmd/.
//
// arch-test:no-negative-fixture (RED proof: import internal/identity/identitytest from any production file → this test fails)
func TestArch_NoProductionCodeImportsATestHelperPackage(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	var violations []string
	for _, dir := range []string{"internal", "cmd"} {
		walkGoFiles(t, filepath.Join(root, dir), false, func(path string, src []byte) {
			slashed := filepath.ToSlash(path)
			if strings.Contains(slashed, "/testdata/") || isTestHelperDir(filepath.Dir(slashed)) {
				return
			}
			for _, imp := range parseImports(t, path, src) {
				if isTestHelperDir(imp) {
					violations = append(violations, slashed+" imports "+imp)
				}
			}
		})
	}
	for _, v := range violations {
		t.Errorf("production file imports a test helper package: %s", v)
	}
}

// isTestHelperDir reports whether a directory or import path names a *test helper
// package: the last element ends in "test" (identitytest, persontest, pgtest, ...).
func isTestHelperDir(p string) bool {
	return strings.HasSuffix(path.Base(p), "test")
}
