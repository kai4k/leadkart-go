package authn_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/tenancy"
	"github.com/leadkart/leadkart-go/internal/identity/app/jwt"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/ports/authn"
)

const (
	tenantA = "0190a000-0000-7000-8000-00000000000a"
	tenantB = "0190a000-0000-7000-8000-00000000000b"
	selfMem = "0190a000-0000-7000-8000-0000000000aa"
)

func TestSatisfies_DecisionTable(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	cases := []struct {
		name      string
		claims    *jwt.Claims
		effective string
		perm      string
		want      bool
	}{
		{"explicit grant", &jwt.Claims{TenantID: tenantA, Permissions: []string{p.Users.View}}, tenantA, p.Users.View, true},
		{"missing", &jwt.Claims{TenantID: tenantA}, tenantA, p.Users.View, false},
		{"super user", &jwt.Claims{TenantID: tenantA, IsSuperUser: true}, tenantB, p.Users.Anonymise, true},
		{"tenant.admin own tenant", &jwt.Claims{TenantID: tenantA, Permissions: []string{p.Meta.TenantAdmin}}, tenantA, p.Roles.Assign, true},
		{"tenant.admin other module", &jwt.Claims{TenantID: tenantA, Permissions: []string{p.Meta.TenantAdmin}}, tenantA, p.Crm.Leads.ReadAll, true},
		{"tenant.admin other tenant", &jwt.Claims{TenantID: tenantA, Permissions: []string{p.Meta.TenantAdmin}}, tenantB, p.Roles.Assign, false},
		{"tenant.admin reserved", &jwt.Claims{TenantID: tenantA, Permissions: []string{p.Meta.TenantAdmin}}, tenantA, p.Tenants.Suspend, false},
		{"platform equivalent", &jwt.Claims{TenantID: tenantA, TenantSlug: authn.PlatformTenantSlug, IsPlatform: true, Permissions: []string{p.Platform.UsersView}}, tenantB, p.Users.View, true},
		{"platform equivalent needs slug", &jwt.Claims{TenantID: tenantA, TenantSlug: "acme", IsPlatform: true, Permissions: []string{p.Platform.UsersView}}, tenantB, p.Users.View, false},
		{"platform equivalent not for impersonation", &jwt.Claims{TenantID: tenantA, TenantSlug: authn.PlatformTenantSlug, IsPlatform: true, Permissions: []string{p.Platform.UsersView}, Act: &jwt.ActClaim{Sub: "op"}}, tenantB, p.Users.View, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := authn.WithClaims(t.Context(), tc.claims)
			ctx = tenancy.WithID(ctx, tenancy.ID(tc.effective))
			if got := authn.Satisfies(ctx, tc.perm); got != tc.want {
				t.Fatalf("Satisfies = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSatisfies_NoClaims_False(t *testing.T) {
	t.Parallel()
	if authn.Satisfies(t.Context(), permission.IdentityPermissions.Users.View) {
		t.Fatal("Satisfies without claims must be false")
	}
}

func TestEffectivePermissions_TenantAdminExpandsAndSorts(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	got := authn.EffectivePermissions(&jwt.Claims{TenantID: tenantA, Permissions: []string{p.Meta.TenantAdmin}})
	if !slices.IsSorted(got) {
		t.Fatal("not sorted")
	}
	for _, want := range []string{p.Meta.TenantAdmin, p.Users.Create, p.Crm.Leads.Read} {
		if !slices.Contains(got, want) {
			t.Errorf("missing %s", want)
		}
	}
	if slices.Contains(got, p.Platform.TenantsManage) {
		t.Error("tenant.admin must not expand to platform.tenants.manage")
	}
	if slices.Contains(got, p.Platform.ImpersonationManage) {
		t.Error("tenant.admin must not expand to platform.impersonation.manage")
	}
}

func TestRequirePermission_TenantAdminPasses(t *testing.T) {
	t.Parallel()
	v := &fakeVerifier{claims: withFreshness(&jwt.Claims{TenantID: tenantA, Permissions: []string{permission.IdentityPermissions.Meta.TenantAdmin}})}
	s := &sentinel{}
	h := authn.RequirePermission(v, alwaysFresh{}, permission.IdentityPermissions.Users.View)(s.handler())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newRequest(t, "Bearer tok"))
	if rec.Code != http.StatusOK || !s.called {
		t.Fatalf("status %d called %v, want 200 true", rec.Code, s.called)
	}
}

func tenantReq(t *testing.T, tenantID string) *http.Request {
	t.Helper()
	mux := http.NewServeMux()
	var captured *http.Request
	mux.HandleFunc("GET /t/{tenantId}", func(_ http.ResponseWriter, r *http.Request) { captured = r })
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/t/"+tenantID, nil)
	mux.ServeHTTP(httptest.NewRecorder(), req)
	captured.Header.Set("Authorization", "Bearer tok")
	return captured
}

func TestRequireTenantPermission(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	cases := []struct {
		name   string
		claims *jwt.Claims
		path   string
		want   int
	}{
		{"owner own tenant", &jwt.Claims{TenantID: tenantA, Permissions: []string{p.Meta.TenantAdmin}}, tenantA, 200},
		{"member own tenant no perm", &jwt.Claims{TenantID: tenantA}, tenantA, 403},
		{"owner other tenant", &jwt.Claims{TenantID: tenantA, Permissions: []string{p.Meta.TenantAdmin}}, tenantB, 403},
		{"platform view equivalent", &jwt.Claims{TenantID: tenantA, TenantSlug: authn.PlatformTenantSlug, IsPlatform: true, Permissions: []string{p.Platform.TenantsView}}, tenantB, 200},
		{"platform without perm", &jwt.Claims{TenantID: tenantA, TenantSlug: authn.PlatformTenantSlug, IsPlatform: true}, tenantB, 403},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v := &fakeVerifier{claims: withFreshness(tc.claims)}
			s := &sentinel{}
			h := authn.RequireTenantPermission(v, alwaysFresh{}, "tenantId", p.Tenants.View)(s.handler())
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, tenantReq(t, tc.path))
			if rec.Code != tc.want {
				t.Fatalf("status %d, want %d (body %s)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

func TestTenantScope_PathTenantRoutes(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	op := func() *jwt.Claims {
		return &jwt.Claims{TenantID: tenantA, TenantSlug: authn.PlatformTenantSlug, IsPlatform: true, Permissions: []string{p.Platform.TenantsView}}
	}
	gates := map[string]func(v authn.Verifier, s *sentinel) http.Handler{
		"RequireTenantPermission": func(v authn.Verifier, s *sentinel) http.Handler {
			return authn.RequireTenantPermission(v, alwaysFresh{}, "tenantId", p.Tenants.View)(s.handler())
		},
	}
	cases := []struct {
		name   string
		header string
		path   string
		want   int
		body   string
	}{
		{"header differs from path", tenantA, tenantB, 400, "tenant_scope_conflict"},
		{"header equals path", tenantB, tenantB, 200, ""},
		{"no header path wins", "", tenantB, 200, ""},
	}
	for gate, build := range gates {
		for _, tc := range cases {
			t.Run(gate+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				v := &fakeVerifier{claims: withFreshness(op())}
				s := &sentinel{}
				log := slog.New(slog.NewTextHandler(io.Discard, nil))
				h := authn.TenantScope(fakeTenants{tenantA: true, tenantB: true}.Lookup, log)(build(v, s))
				r := tenantReq(t, tc.path)
				if tc.header != "" {
					r.Header.Set(authn.TenantScopeHeader, tc.header)
				}
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, r)
				if rec.Code != tc.want || !strings.Contains(rec.Body.String(), tc.body) {
					t.Fatalf("status %d want %d body %s", rec.Code, tc.want, rec.Body.String())
				}
				if tc.want == 200 && string(s.tenantID) != tc.path {
					t.Fatalf("tenant %q want path tenant %q", s.tenantID, tc.path)
				}
			})
		}
	}
}

func TestTenantScope_SelfTargetUsesCallerMembership(t *testing.T) {
	t.Parallel()
	// Operator scoped to tenant B: DenySelfTarget still compares against the
	// caller's OWN membership id, not anything tenant-derived.
	const self = "0190a000-0000-7000-8000-0000000000aa"
	c := &jwt.Claims{TenantID: tenantA, MembershipID: self, TenantSlug: authn.PlatformTenantSlug, IsPlatform: true, Permissions: []string{"platform.tenants.view"}}
	v := &fakeVerifier{claims: withFreshness(c)}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	for target, want := range map[string]int{self: 403, "0190a000-0000-7000-8000-0000000000bb": 200} {
		s := &sentinel{}
		h := authn.TenantScope(fakeTenants{tenantB: true}.Lookup, log)(authn.RequireFreshStamp(v, alwaysFresh{})(authn.DenySelfTarget("id")(s.handler())))
		r := userReqWithVar(t, "id", target)
		r.Header.Set(authn.TenantScopeHeader, tenantB)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != want {
			t.Fatalf("target %s: status %d want %d", target, rec.Code, want)
		}
	}
}

func userReqWithVar(t *testing.T, name, value string) *http.Request {
	t.Helper()
	mux := http.NewServeMux()
	var captured *http.Request
	mux.HandleFunc("GET /u/{"+name+"}", func(_ http.ResponseWriter, r *http.Request) { captured = r })
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/u/"+value, nil)
	mux.ServeHTTP(httptest.NewRecorder(), req)
	captured.Header.Set("Authorization", "Bearer tok")
	return captured
}

func userReq(t *testing.T, userID string) *http.Request {
	t.Helper()
	mux := http.NewServeMux()
	var captured *http.Request
	mux.HandleFunc("GET /u/{userId}", func(_ http.ResponseWriter, r *http.Request) { captured = r })
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/u/"+userID, nil)
	mux.ServeHTTP(httptest.NewRecorder(), req)
	captured.Header.Set("Authorization", "Bearer tok")
	return captured
}

func TestRequirePermissionOrSelf(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	member := func() *jwt.Claims { return withFreshness(&jwt.Claims{TenantID: tenantA, MembershipID: selfMem}) }
	for _, tc := range []struct {
		name   string
		claims *jwt.Claims
		path   string
		want   int
	}{
		{"self", member(), selfMem, 200},
		{"other without perm", member(), "0190a000-0000-7000-8000-0000000000bb", 403},
		{"other with perm", withFreshness(&jwt.Claims{TenantID: tenantA, MembershipID: selfMem, Permissions: []string{p.Users.View}}), "0190a000-0000-7000-8000-0000000000bb", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := &sentinel{}
			h := authn.RequirePermissionOrSelf(&fakeVerifier{claims: tc.claims}, alwaysFresh{}, "userId", p.Users.View)(s.handler())
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, userReq(t, tc.path))
			if rec.Code != tc.want {
				t.Fatalf("status %d want %d", rec.Code, tc.want)
			}
		})
	}
}

// selfSpellings are every spelling uuid.Parse accepts for the same value as
// selfMem — the handler/repository treat them all as the caller's own id.
var selfSpellings = []struct{ name, path string }{
	{"canonical", selfMem},
	{"uppercase", strings.ToUpper(selfMem)},
	{"urn prefix", "urn:uuid:" + selfMem},
	{"encoded braces", "%7B" + selfMem + "%7D"},
	{"hyphenless", strings.ReplaceAll(selfMem, "-", "")},
}

func TestDenySelfTarget(t *testing.T) {
	t.Parallel()
	const other = "0190a000-0000-7000-8000-0000000000bb"
	type row struct {
		name   string
		claims *jwt.Claims
		path   string
		want   int
		code   string
	}
	rows := []row{
		{"self denied", &jwt.Claims{TenantID: tenantA, MembershipID: selfMem}, selfMem, 403, "self_modification_forbidden"},
		{"other allowed", &jwt.Claims{TenantID: tenantA, MembershipID: selfMem}, other, 200, ""},
		{"super user self allowed", &jwt.Claims{TenantID: tenantA, MembershipID: selfMem, IsSuperUser: true}, selfMem, 200, ""},
		{"super user self uppercase allowed", &jwt.Claims{TenantID: tenantA, MembershipID: selfMem, IsSuperUser: true}, strings.ToUpper(selfMem), 200, ""},
		{"platform operator self denied", &jwt.Claims{TenantID: tenantA, MembershipID: selfMem, TenantSlug: authn.PlatformTenantSlug, IsPlatform: true}, selfMem, 403, "self_modification_forbidden"},
		{"empty membership claim denied", &jwt.Claims{TenantID: tenantA}, other, 403, "forbidden"},
		{"unparseable membership claim denied", &jwt.Claims{TenantID: tenantA, MembershipID: "not-a-uuid"}, other, 403, "forbidden"},
		{"unparseable path 400", &jwt.Claims{TenantID: tenantA, MembershipID: selfMem}, "nope", 400, "invalid_user_id"},
		{"uppercase membership claim vs canonical path denied", &jwt.Claims{TenantID: tenantA, MembershipID: strings.ToUpper(selfMem)}, selfMem, 403, "self_modification_forbidden"},
	}
	for _, sp := range selfSpellings {
		rows = append(rows, row{"self as " + sp.name, &jwt.Claims{TenantID: tenantA, MembershipID: selfMem}, sp.path, 403, "self_modification_forbidden"})
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := &sentinel{}
			h := authn.RequireFreshStamp(&fakeVerifier{claims: withFreshness(tc.claims)}, alwaysFresh{})(authn.DenySelfTarget("userId")(s.handler()))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, userReq(t, tc.path))
			if rec.Code != tc.want {
				t.Fatalf("status %d want %d (body %s)", rec.Code, tc.want, rec.Body.String())
			}
			if tc.code != "" && !strings.Contains(rec.Body.String(), `"`+tc.code+`"`) {
				t.Fatalf("body %s, want code %s", rec.Body.String(), tc.code)
			}
			if (tc.want == 200) != s.called {
				t.Fatalf("handler called = %v with status %d", s.called, tc.want)
			}
		})
	}
}

func TestDenySelfTarget_NoClaims_401(t *testing.T) {
	t.Parallel()
	s := &sentinel{}
	rec := httptest.NewRecorder()
	authn.DenySelfTarget("userId")(s.handler()).ServeHTTP(rec, userReq(t, selfMem))
	if rec.Code != http.StatusUnauthorized || s.called {
		t.Fatalf("status %d called %v, want 401 false", rec.Code, s.called)
	}
}

func TestDenySelfTarget_MissingPathVar_400(t *testing.T) {
	t.Parallel()
	s := &sentinel{}
	h := authn.RequireFreshStamp(&fakeVerifier{claims: withFreshness(&jwt.Claims{TenantID: tenantA, MembershipID: selfMem})}, alwaysFresh{})(authn.DenySelfTarget("userId")(s.handler()))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newRequest(t, "Bearer tok")) // no mux → PathValue("userId") == ""
	if rec.Code != http.StatusBadRequest || s.called {
		t.Fatalf("status %d called %v, want 400 false", rec.Code, s.called)
	}
}

func TestRequirePermissionOrSelf_SpellingsOfSelf(t *testing.T) {
	t.Parallel()
	for _, sp := range selfSpellings {
		t.Run(sp.name, func(t *testing.T) {
			t.Parallel()
			s := &sentinel{}
			claims := withFreshness(&jwt.Claims{TenantID: tenantA, MembershipID: selfMem})
			h := authn.RequirePermissionOrSelf(&fakeVerifier{claims: claims}, alwaysFresh{}, "userId", permission.IdentityPermissions.Users.View)(s.handler())
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, userReq(t, sp.path))
			if rec.Code != 200 {
				t.Fatalf("status %d want 200", rec.Code)
			}
		})
	}
	t.Run("empty membership claim is not self", func(t *testing.T) {
		t.Parallel()
		s := &sentinel{}
		h := authn.RequirePermissionOrSelf(&fakeVerifier{claims: withFreshness(&jwt.Claims{TenantID: tenantA})}, alwaysFresh{}, "userId", permission.IdentityPermissions.Users.View)(s.handler())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, userReq(t, selfMem))
		if rec.Code != 403 {
			t.Fatalf("status %d want 403", rec.Code)
		}
	})
}

func TestRequireTenantPermission_PathTenantCompared_AsUUIDValue(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	for _, path := range []string{strings.ToUpper(tenantA), "urn:uuid:" + tenantA, strings.ReplaceAll(tenantA, "-", "")} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			s := &sentinel{}
			claims := withFreshness(&jwt.Claims{TenantID: tenantA, Permissions: []string{p.Meta.TenantAdmin}})
			h := authn.RequireTenantPermission(&fakeVerifier{claims: claims}, alwaysFresh{}, "tenantId", p.Tenants.View)(s.handler())
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, tenantReq(t, path))
			if rec.Code != 200 {
				t.Fatalf("status %d want 200 (own tenant, other spelling)", rec.Code)
			}
		})
	}
}

func TestRequireTenantPermission_BadPathValue_400InvalidTenantID(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	for _, path := range []string{"not-a-uuid"} {
		s := &sentinel{}
		claims := withFreshness(&jwt.Claims{TenantID: tenantA, Permissions: []string{p.Meta.TenantAdmin}})
		h := authn.RequireTenantPermission(&fakeVerifier{claims: claims}, alwaysFresh{}, "tenantId", p.Tenants.View)(s.handler())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, tenantReq(t, path))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"invalid_tenant_id"`) || s.called {
			t.Fatalf("status %d body %s called %v, want 400 invalid_tenant_id", rec.Code, rec.Body.String(), s.called)
		}
	}
	// missing path variable entirely (no mux) → 400, not 403 "forbidden"
	s := &sentinel{}
	claims := withFreshness(&jwt.Claims{TenantID: tenantA, Permissions: []string{p.Meta.TenantAdmin}})
	h := authn.RequireTenantPermission(&fakeVerifier{claims: claims}, alwaysFresh{}, "tenantId", p.Tenants.View)(s.handler())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newRequest(t, "Bearer tok"))
	if rec.Code != http.StatusBadRequest || s.called {
		t.Fatalf("missing var: status %d called %v, want 400", rec.Code, s.called)
	}
}
