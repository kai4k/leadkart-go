package authn_test

import (
	"bytes"
	"context"
	"github.com/leadkart/leadkart-go/internal/common/actclaim"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/tenancy"
	"github.com/leadkart/leadkart-go/internal/identity/app/jwt"
	"github.com/leadkart/leadkart-go/internal/identity/ports/authn"
)

const unknownTenantHdr = "0190a000-0000-7000-8000-0000000000ee"

// countingTenants is a TenantLookupFunc fake that counts lookups and answers
// from a state table (absent id = unknown).
type countingTenants struct {
	states  map[string]authn.TenantScopeState
	lookups atomic.Int64
}

func (c *countingTenants) Lookup(_ context.Context, id string) (authn.TenantScopeState, error) {
	c.lookups.Add(1)
	return c.states[id], nil
}

func activeTenants() *countingTenants {
	return &countingTenants{states: map[string]authn.TenantScopeState{tenantA: authn.TenantScopeActive, tenantB: authn.TenantScopeActive}}
}

func operatorClaims() *jwt.Claims {
	return &jwt.Claims{TenantID: tenantA, TenantSlug: authn.PlatformTenantSlug, IsPlatform: true, Permissions: []string{"platform.tenants.view"}}
}

// pathTenant returns the {tenantId} path value the real mux would have set.
func pathTenant(path string) string {
	_, rest, ok := strings.Cut(path, "/tenants/")
	if !ok {
		return ""
	}
	id, _, _ := strings.Cut(rest, "/")
	return id
}

// scopeServe runs one request through TenantScope -> (wrap) -> RequireFreshStamp
// -> sentinel and reports the recorder, the sentinel and the captured log.
func scopeServe(t *testing.T, tn *countingTenants, c *jwt.Claims, v authn.StampValidator,
	wrap func(http.Handler) http.Handler, method, path string, headers ...string) (*httptest.ResponseRecorder, *sentinel, string) {
	t.Helper()
	var logBuf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ver := &fakeVerifier{claims: withFreshness(c)}
	s := &sentinel{}
	h := authn.RequireFreshStamp(ver, v)(s.handler())
	if wrap != nil {
		h = wrap(h)
	}
	h = authn.TenantScope(tn.Lookup, log)(h)
	r := httptest.NewRequestWithContext(t.Context(), method, path, nil)
	r.Header.Set("Authorization", "Bearer tok")
	for _, hv := range headers {
		r.Header.Add(authn.TenantScopeHeader, hv)
	}
	r.SetPathValue("tenantId", pathTenant(path))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec, s, logBuf.String()
}

// I2: the header is ignored centrally on auth/*, platform/* (except balance and
// purchase), and POST /tenants — no validation, no lookup, no audit line.
func TestTenantScopePolicy_IgnoredRoutes(t *testing.T) {
	t.Parallel()
	cases := []struct{ method, path string }{
		{"GET", "/api/v1/auth/me/capabilities"},
		{"GET", "/api/v1/auth/sessions"},
		{"POST", "/api/v1/auth/change-password"},
		{"POST", "/api/v1/auth/request-email-change"},
		{"GET", "/api/v1/auth/me/activity"},
		{"GET", "/api/v1/platform/stats"},
		{"GET", "/api/v1/platform/tenants"},
		{"POST", "/api/v1/platform/impersonation/sessions"},
		{"POST", "/api/v1/platform/lead-credits/topup"},
		{"POST", "/api/v1/tenants"},
		{"GET", "/api/v1/tenants"},
		{"GET", "/api/v1/tenants/by-slug/acme"},
		{"GET", "/api/v1/search"},
		{"GET", "/api/v1/notifications"},
		{"GET", "/api/v1/notifications/unread-count"},
		{"POST", "/api/v1/notifications/read-all"},
		{"POST", "/api/v1/notifications/0190a000-0000-7000-8000-0000000000cc/read"},
	}
	for _, tc := range cases {
		for _, hdr := range []string{unknownTenantHdr, "not-a-uuid"} {
			t.Run(tc.method+" "+tc.path+" "+hdr, func(t *testing.T) {
				t.Parallel()
				tn := activeTenants()
				rec, s, logs := scopeServe(t, tn, operatorClaims(), alwaysFresh{}, nil, tc.method, tc.path, hdr)
				if rec.Code != http.StatusOK {
					t.Fatalf("ignored route must answer normally, got %d %s", rec.Code, rec.Body.String())
				}
				if tn.lookups.Load() != 0 {
					t.Fatalf("ignored route must not look the tenant up (%d lookups)", tn.lookups.Load())
				}
				if string(s.tenantID) != tenantA {
					t.Fatalf("effective tenant %q, want claims tenant", s.tenantID)
				}
				if strings.Contains(logs, "operator tenant scope") || strings.Contains(logs, "level=WARN") {
					t.Fatalf("ignored route must not audit: %s", logs)
				}
			})
		}
	}
}

// A route inside an ignored group that declares HonourTenantScope still reads
// the header; without the marker the table ignores it.
func TestTenantScopePolicy_BalanceHonoursHeader(t *testing.T) {
	t.Parallel()
	tn := activeTenants()
	rec, s, _ := scopeServe(t, tn, operatorClaims(), alwaysFresh{}, authn.HonourTenantScope, "GET", "/api/v1/platform/lead-credits/balance", tenantB)
	if rec.Code != http.StatusOK || string(s.tenantID) != tenantB {
		t.Fatalf("balance must honour the header: %d tenant %q", rec.Code, s.tenantID)
	}
	rec, _, _ = scopeServe(t, tn, operatorClaims(), alwaysFresh{}, authn.HonourTenantScope, "GET", "/api/v1/platform/lead-credits/balance", unknownTenantHdr)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("balance with unknown tenant: %d", rec.Code)
	}
}

// The purchase route sits under /platform/ but is scope-aware: it must see the
// header so RejectTenantScope can refuse it.
func TestTenantScopePolicy_PurchaseSeesHeader(t *testing.T) {
	t.Parallel()
	rec, _, _ := scopeServe(t, activeTenants(), operatorClaims(), alwaysFresh{}, authn.RejectTenantScope,
		"POST", "/api/v1/platform/marketplace/leads/"+tenantB+"/purchase", tenantB)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "tenant_scope_not_supported") {
		t.Fatalf("purchase under scope: %d %s", rec.Code, rec.Body.String())
	}
}

// I5: validation, lookup and the audit line happen only for a caller whose
// token AND stamp are valid.
func TestTenantScopePolicy_StaleStampNeverLooksUp(t *testing.T) {
	t.Parallel()
	for _, hdr := range []string{unknownTenantHdr, "not-a-uuid", tenantB} {
		tn := activeTenants()
		stale := &fakeStampValidator{fresh: false}
		rec, s, logs := scopeServe(t, tn, operatorClaims(), stale, nil, "GET", "/api/v1/users", hdr)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("hdr %q: status %d want 401 (%s)", hdr, rec.Code, rec.Body.String())
		}
		if tn.lookups.Load() != 0 || s.called {
			t.Fatalf("hdr %q: stale token must not reach lookup/handler (%d lookups)", hdr, tn.lookups.Load())
		}
		if strings.Contains(logs, "operator tenant scope") {
			t.Fatalf("stale token must not produce an audit line: %s", logs)
		}
	}
}

// I5: rejected scope attempts log at WARN; accepted ones at INFO.
func TestTenantScopePolicy_AuditLevels(t *testing.T) {
	t.Parallel()
	tn := activeTenants()
	_, _, logs := scopeServe(t, tn, operatorClaims(), alwaysFresh{}, nil, "GET", "/api/v1/users", tenantB)
	if !strings.Contains(logs, "level=INFO") || !strings.Contains(logs, "operator tenant scope") ||
		!strings.Contains(logs, "effective_tenant_id="+tenantB) || !strings.Contains(logs, "caller_tenant_id="+tenantA) {
		t.Fatalf("accepted scope must log INFO with ADR 0039 fields: %s", logs)
	}
	for name, hdr := range map[string]string{"malformed": "nope", "unknown": unknownTenantHdr} {
		_, _, logs = scopeServe(t, tn, operatorClaims(), alwaysFresh{}, nil, "GET", "/api/v1/users", hdr)
		if !strings.Contains(logs, "level=WARN") || strings.Contains(logs, "level=INFO") {
			t.Fatalf("%s: rejected scope must log WARN only: %s", name, logs)
		}
	}
	_, _, logs = scopeServe(t, tn, operatorClaims(), alwaysFresh{}, authn.RejectTenantScope, "GET", "/api/v1/permission-requests", tenantB)
	if !strings.Contains(logs, "level=WARN") || strings.Contains(logs, "level=INFO") {
		t.Fatalf("not-supported scope must log WARN only: %s", logs)
	}
}

// M1: more than one X-Tenant-Id value is ambiguous — never pick the first.
func TestTenantScopePolicy_MultipleHeaderValues(t *testing.T) {
	t.Parallel()
	for _, pair := range [][2]string{{tenantA, tenantB}, {tenantB, tenantA}, {tenantB, tenantB}} {
		tn := activeTenants()
		rec, s, _ := scopeServe(t, tn, operatorClaims(), alwaysFresh{}, nil, "GET", "/api/v1/users", pair[0], pair[1])
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid_tenant_scope") {
			t.Fatalf("%v: %d %s", pair, rec.Code, rec.Body.String())
		}
		if s.called || tn.lookups.Load() != 0 {
			t.Fatalf("%v: ambiguous scope must not reach lookup/handler", pair)
		}
	}
	// A tenant user's duplicate headers are still just ignored.
	rec, s, _ := scopeServe(t, activeTenants(), &jwt.Claims{TenantID: tenantA, TenantSlug: "acme"}, alwaysFresh{}, nil, "GET", "/api/v1/users", tenantA, tenantB)
	if rec.Code != http.StatusOK || string(s.tenantID) != tenantA {
		t.Fatalf("member duplicate headers must be ignored: %d", rec.Code)
	}
}

// I3: path vs header conflict on {tenantId} routes outside RequireTenantPermission.
func TestTenantScopePolicy_PathConflict(t *testing.T) {
	t.Parallel()
	lifecycle := authn.TenantScopeLifecycle("tenantId")
	pathOnly := authn.TenantScopePathOnly("tenantId")
	cases := []struct {
		name   string
		wrap   func(http.Handler) http.Handler
		method string
		path   string
		header string
		want   int
		tenant string
	}{
		{"lifecycle conflict", lifecycle, "POST", "/api/v1/tenants/" + tenantB + "/suspend", tenantA, 400, ""},
		// A header equal to the path tenant selects nothing (F4): the home tenant stays bound.
		{"lifecycle same tenant", lifecycle, "POST", "/api/v1/tenants/" + tenantB + "/suspend", tenantB, 200, tenantA},
		{"lifecycle no header", lifecycle, "POST", "/api/v1/tenants/" + tenantB + "/suspend", "", 200, tenantA},
		{"path only conflict", pathOnly, "DELETE", "/api/v1/platform/tenants/" + tenantB, tenantA, 400, ""},
		{"path only same tenant", pathOnly, "DELETE", "/api/v1/platform/tenants/" + tenantB, tenantB, 200, tenantA},
		{"path only no header", pathOnly, "DELETE", "/api/v1/platform/tenants/" + tenantB, "", 200, tenantA},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var hdrs []string
			if tc.header != "" {
				hdrs = []string{tc.header}
			}
			rec, s, _ := scopeServe(t, activeTenants(), operatorClaims(), alwaysFresh{}, tc.wrap, tc.method, tc.path, hdrs...)
			if rec.Code != tc.want {
				t.Fatalf("status %d want %d body %s", rec.Code, tc.want, rec.Body.String())
			}
			if tc.want == 400 && !strings.Contains(rec.Body.String(), "tenant_scope_conflict") {
				t.Fatalf("code: %s", rec.Body.String())
			}
			if tc.tenant != "" && string(s.tenantID) != tc.tenant {
				t.Fatalf("effective tenant %q want %q", s.tenantID, tc.tenant)
			}
		})
	}
}

// M4: scoped requests into a suspended / pending-deletion tenant: reads and
// lifecycle and path-tenant routes pass; other (writable) writes are 409
// tenant_not_active.
func TestTenantScopePolicy_InactiveTenant(t *testing.T) {
	t.Parallel()
	tn := &countingTenants{states: map[string]authn.TenantScopeState{tenantB: authn.TenantScopeInactive}}
	for _, m := range []string{"GET", "HEAD"} {
		if rec, _, _ := scopeServe(t, tn, operatorClaims(), alwaysFresh{}, nil, m, "/api/v1/users", tenantB); rec.Code != http.StatusOK {
			t.Fatalf("%s into inactive tenant: %d", m, rec.Code)
		}
	}
	for _, m := range []string{"POST", "PATCH", "PUT", "DELETE"} {
		rec, s, _ := scopeServe(t, tn, operatorClaims(), alwaysFresh{}, authn.TenantScopeWritable, m, "/api/v1/users", tenantB)
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "tenant_not_active") || s.called {
			t.Fatalf("%s into inactive tenant: %d %s", m, rec.Code, rec.Body.String())
		}
	}
	rec, _, _ := scopeServe(t, tn, operatorClaims(), alwaysFresh{}, authn.TenantScopeLifecycle("tenantId"), "POST", "/api/v1/tenants/"+tenantB+"/activate", tenantB)
	if rec.Code != http.StatusOK {
		t.Fatalf("lifecycle write into inactive tenant: %d %s", rec.Code, rec.Body.String())
	}
	// The path is authoritative on {tenantId} routes: a write on a suspended
	// tenant works with a matching header exactly as it does without one.
	if rec, _, _ := scopeServe(t, tn, operatorClaims(), alwaysFresh{}, authn.TenantScopeMatchesPath("tenantId"), "PATCH", "/api/v1/tenants/"+tenantB+"/profile", tenantB); rec.Code != http.StatusOK {
		t.Fatalf("path-tenant write into inactive tenant with header: %d", rec.Code)
	}
	// A deleted tenant is reported unknown (404), never scoped into.
	if rec, _, _ := scopeServe(t, &countingTenants{}, operatorClaims(), alwaysFresh{}, nil, "GET", "/api/v1/users", tenantB); rec.Code != http.StatusNotFound {
		t.Fatalf("deleted/unknown tenant: %d", rec.Code)
	}
}

// M6: EffectiveTenantID fails closed when no tenant is bound on ctx.
func TestEffectiveTenantID_FailsClosedWithoutBoundTenant(t *testing.T) {
	t.Parallel()
	c := &jwt.Claims{TenantID: tenantA}
	if id, ok := authn.EffectiveTenantID(authn.WithClaims(t.Context(), c)); ok || id != "" {
		t.Fatalf("claims alone must not stand in for the bound tenant: %q %v", id, ok)
	}
	if id, ok := authn.EffectiveTenantID(tenancy.WithID(t.Context(), tenancy.ID(tenantB))); !ok || id != tenantB {
		t.Fatalf("bound tenant is returned: %q %v", id, ok)
	}
}

// I4: an accepted operator scope stamps the act claim on ctx so every
// outbox / audit row written by the request carries act_operator_id; nothing
// is stamped when the request stays in the home tenant or the header is ignored.
func TestTenantScopePolicy_ActClaimOnScopedRequests(t *testing.T) {
	t.Parallel()
	const operatorID = "01999999-aaaa-7000-8000-aaaaaaaaaaaa" // withFreshness default subject
	probe := func(t *testing.T, c *jwt.Claims, headers ...string) (actclaim.Claim, bool) {
		t.Helper()
		var got actclaim.Claim
		var ok bool
		ver := &fakeVerifier{claims: withFreshness(c)}
		h := authn.TenantScope(activeTenants().Lookup, slog.New(slog.NewTextHandler(io.Discard, nil)))(
			authn.RequireFreshStamp(ver, alwaysFresh{})(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				got, ok = actclaim.FromContext(r.Context())
			})))
		r := httptest.NewRequestWithContext(t.Context(), "GET", "/api/v1/users", nil)
		r.Header.Set("Authorization", "Bearer tok")
		for _, hv := range headers {
			r.Header.Add(authn.TenantScopeHeader, hv)
		}
		h.ServeHTTP(httptest.NewRecorder(), r)
		return got, ok
	}
	if got, ok := probe(t, operatorClaims(), tenantB); !ok || got.OperatorID != operatorID || got.Reason != authn.ScopeActReason || got.SessionID != "" {
		t.Fatalf("scoped operator must carry the act claim: %+v %v", got, ok)
	}
	if _, ok := probe(t, operatorClaims()); ok {
		t.Fatal("no header: no act claim")
	}
	if _, ok := probe(t, operatorClaims(), tenantA); ok {
		t.Fatal("header naming the home tenant: no act claim")
	}
	if _, ok := probe(t, &jwt.Claims{TenantID: tenantA, TenantSlug: "acme"}, tenantB); ok {
		t.Fatal("tenant member header is ignored: no act claim")
	}
}

// Deny-by-default writes: a non-read request that selects a foreign tenant is
// 403 tenant_scope_read_only unless the route opted in with TenantScopeWritable
// (or is a path-tenant route). Reads, the home-tenant header and non-operators
// are unaffected.
func TestTenantScopePolicy_ForeignWritesReadOnlyByDefault(t *testing.T) {
	t.Parallel()
	for _, m := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		tn := activeTenants()
		rec, s, _ := scopeServe(t, tn, operatorClaims(), alwaysFresh{}, nil, m, "/api/v1/orders/quotations", tenantB)
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "tenant_scope_read_only") || s.called {
			t.Fatalf("%s unmarked route under foreign scope: %d %s", m, rec.Code, rec.Body.String())
		}
		if tn.lookups.Load() != 0 {
			t.Fatalf("%s: the read-only decision must not look the tenant up", m)
		}
		if rec, s, _ := scopeServe(t, activeTenants(), operatorClaims(), alwaysFresh{}, authn.TenantScopeWritable, m, "/api/v1/roles", tenantB); rec.Code != http.StatusOK || string(s.tenantID) != tenantB {
			t.Fatalf("%s writable route: %d tenant %q", m, rec.Code, s.tenantID)
		}
		if rec, _, _ := scopeServe(t, activeTenants(), operatorClaims(), alwaysFresh{}, authn.TenantScopeMatchesPath("tenantId"), m, "/api/v1/tenants/"+tenantB+"/profile", tenantB); rec.Code != http.StatusOK {
			t.Fatalf("%s path-tenant route: %d", m, rec.Code)
		}
		if rec, _, _ := scopeServe(t, activeTenants(), operatorClaims(), alwaysFresh{}, nil, m, "/api/v1/orders/quotations", tenantA); rec.Code != http.StatusOK {
			t.Fatalf("%s with the home tenant in the header: %d", m, rec.Code)
		}
		member := &jwt.Claims{TenantID: tenantA, TenantSlug: "acme"}
		if rec, s, _ := scopeServe(t, activeTenants(), member, alwaysFresh{}, nil, m, "/api/v1/orders/quotations", tenantB); rec.Code != http.StatusOK || string(s.tenantID) != tenantA {
			t.Fatalf("%s tenant member: header must stay ignored: %d", m, rec.Code)
		}
	}
	if rec, _, _ := scopeServe(t, activeTenants(), operatorClaims(), alwaysFresh{}, nil, "GET", "/api/v1/orders/quotations", tenantB); rec.Code != http.StatusOK {
		t.Fatalf("reads stay available under foreign scope: %d", rec.Code)
	}
}

// A registration marker beats the central table: a marked route under an
// ignored group is NOT silently run with the header ignored. This fails if the
// table ever shadows a marker.
func TestTenantScopePolicy_MarkerWinsOverTable(t *testing.T) {
	t.Parallel()
	const marked = "/api/v1/platform/anything/x"
	if rec, s, _ := scopeServe(t, activeTenants(), operatorClaims(), alwaysFresh{}, nil, "GET", marked, unknownTenantHdr); rec.Code != http.StatusOK || !s.called {
		t.Fatalf("control: an unmarked route under /platform/ ignores the header: %d", rec.Code)
	}
	for name, tc := range map[string]struct {
		wrap func(http.Handler) http.Handler
		want int
	}{
		"reject":   {authn.RejectTenantScope, http.StatusForbidden}, // refused before any lookup
		"honour":   {authn.HonourTenantScope, http.StatusNotFound},  // header validated: unknown tenant
		"writable": {authn.TenantScopeWritable, http.StatusNotFound},
	} {
		rec, s, _ := scopeServe(t, activeTenants(), operatorClaims(), alwaysFresh{}, tc.wrap, "GET", marked, unknownTenantHdr)
		if rec.Code != tc.want || s.called {
			t.Fatalf("%s: marker must beat the table: %d %s, want %d", name, rec.Code, rec.Body.String(), tc.want)
		}
	}
	rec, _, _ := scopeServe(t, activeTenants(), operatorClaims(), alwaysFresh{}, authn.RejectTenantScope, "POST", marked, tenantB)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "tenant_scope_not_supported") {
		t.Fatalf("reject marker under /platform/: %d %s", rec.Code, rec.Body.String())
	}
}

// Nested auth gates (platform anchor + permission gate each run the freshness
// gate) apply the scope ONCE: one tenant lookup, one INFO line, and the inner
// handler still sees the scoped tenant.
func TestTenantScopePolicy_NestedGatesApplyScopeOnce(t *testing.T) {
	t.Parallel()
	tn := activeTenants()
	var logBuf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ver := &fakeVerifier{claims: withFreshness(operatorClaims())}
	s := &sentinel{}
	gate := authn.RequireFreshStamp(ver, alwaysFresh{})
	h := authn.TenantScope(tn.Lookup, log)(gate(gate(gate(s.handler()))))
	r := httptest.NewRequestWithContext(t.Context(), "GET", "/api/v1/users", nil)
	r.Header.Set("Authorization", "Bearer tok")
	r.Header.Set(authn.TenantScopeHeader, tenantB)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK || string(s.tenantID) != tenantB {
		t.Fatalf("nested gates: %d tenant %q", rec.Code, s.tenantID)
	}
	if got := tn.lookups.Load(); got != 1 {
		t.Fatalf("lookups = %d, want 1", got)
	}
	if n := strings.Count(logBuf.String(), "operator tenant scope"); n != 1 {
		t.Fatalf("INFO lines = %d, want 1: %s", n, logBuf.String())
	}
}

// An anchored operator acting in a foreign tenant through the PATH alone
// (RequireTenantPermission, no header) carries the same act claim, with the
// path reason; an operator on its own tenant's path does not.
func TestRequireTenantPermission_PathReBindCarriesActClaim(t *testing.T) {
	t.Parallel()
	probe := func(c *jwt.Claims, pathTenant string) (actclaim.Claim, bool, int) {
		var got actclaim.Claim
		var ok bool
		ver := &fakeVerifier{claims: withFreshness(c)}
		h := authn.RequireTenantPermission(ver, alwaysFresh{}, "tenantId", "identity.tenants.update")(
			http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				got, ok = actclaim.FromContext(r.Context())
			}))
		r := httptest.NewRequestWithContext(t.Context(), "PATCH", "/api/v1/tenants/"+pathTenant+"/profile", nil)
		r.SetPathValue("tenantId", pathTenant)
		r.Header.Set("Authorization", "Bearer tok")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return got, ok, rec.Code
	}
	op := operatorClaims()
	op.Permissions = []string{"platform.tenants.manage"}
	got, ok, code := probe(op, tenantB)
	if code != http.StatusOK || !ok || got.OperatorID == "" || got.Reason != authn.ScopeActReasonPath {
		t.Fatalf("path-only foreign write: %d %+v %v", code, got, ok)
	}
	if _, ok, code := probe(op, tenantA); code != http.StatusOK || ok {
		t.Fatalf("own tenant path: %d act=%v", code, ok)
	}
	admin := &jwt.Claims{TenantID: tenantA, TenantSlug: "acme", Permissions: []string{"tenant.admin"}}
	if _, ok, code := probe(admin, tenantA); code != http.StatusOK || ok {
		t.Fatalf("tenant admin on own path: %d act=%v", code, ok)
	}
}

// F4: on a path-tenant route a header equal to the path tenant is not a scope
// request at all: no scope authority, no lookup. The route's own permission gate
// decides, so an operator holding only the route's permission is not refused
// with tenant_scope_forbidden. A different header is still a 400 conflict.
func TestTenantScopePolicy_PathTenantHeaderEqualToPathNeedsNoScopeAuthority(t *testing.T) {
	t.Parallel()
	noScopeAuthority := func() *jwt.Claims {
		return &jwt.Claims{TenantID: tenantA, TenantSlug: authn.PlatformTenantSlug, IsPlatform: true, Permissions: []string{"platform.tenants.manage", "identity.tenants.suspend"}}
	}
	zeroPermission := func() *jwt.Claims {
		return &jwt.Claims{TenantID: tenantA, TenantSlug: authn.PlatformTenantSlug, IsPlatform: true}
	}
	routes := []struct {
		name   string
		wrap   func(http.Handler) http.Handler
		method string
		path   string
	}{
		{"match", authn.TenantScopeMatchesPath("tenantId"), "PATCH", "/api/v1/tenants/" + tenantB + "/profile"},
		{"lifecycle", authn.TenantScopeLifecycle("tenantId"), "POST", "/api/v1/tenants/" + tenantB + "/suspend"},
		{"path only", authn.TenantScopePathOnly("tenantId"), "DELETE", "/api/v1/platform/tenants/" + tenantB},
	}
	for _, rt := range routes {
		for _, who := range []struct {
			name   string
			claims func() *jwt.Claims
		}{{"holder of the route permission only", noScopeAuthority}, {"zero-permission platform member", zeroPermission}} {
			t.Run(rt.name+" header==path, "+who.name, func(t *testing.T) {
				t.Parallel()
				tn := activeTenants()
				rec, s, logs := scopeServe(t, tn, who.claims(), alwaysFresh{}, rt.wrap, rt.method, rt.path, tenantB)
				if rec.Code != http.StatusOK || !s.called {
					t.Fatalf("header equal to the path tenant: %d %s, want the request to reach the route gate", rec.Code, rec.Body.String())
				}
				if tn.lookups.Load() != 0 {
					t.Fatalf("no tenant lookup on a path-tenant route (%d)", tn.lookups.Load())
				}
				if strings.Contains(logs, "level=WARN") {
					t.Fatalf("not a scope rejection: %s", logs)
				}
			})
			t.Run(rt.name+" header!=path, "+who.name, func(t *testing.T) {
				t.Parallel()
				rec, s, _ := scopeServe(t, activeTenants(), who.claims(), alwaysFresh{}, rt.wrap, rt.method, rt.path, tenantA)
				if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "tenant_scope_conflict") || s.called {
					t.Fatalf("conflicting header: %d %s, want 400 tenant_scope_conflict", rec.Code, rec.Body.String())
				}
			})
		}
	}
}
