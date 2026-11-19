package authn_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/tenancy"
	"github.com/leadkart/leadkart-go/internal/identity/app/jwt"
	"github.com/leadkart/leadkart-go/internal/identity/domain/authority"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/ports/authn"
)

func rankPtr(n int) *int { return &n }

// holds proves what the actor's effective set contains through the public
// policy: creating a level-50 role carrying exactly perm is allowed only when
// the actor holds perm (the grant ceiling).
func holds(t *testing.T, a authority.Actor, perm string) bool {
	t.Helper()
	err := a.MayCreateRole(a.TenantID(), authority.RankUser.Int(), []string{perm}, nil)
	switch {
	case err == nil:
		return true
	case errors.Is(err, authority.ErrGrantExceedsAuthority):
		return false
	default:
		t.Fatalf("MayCreateRole(%s) = %v", perm, err)
		return false
	}
}

func TestAuthorityFrom_NoClaims_ZeroActor(t *testing.T) {
	t.Parallel()
	a := authn.AuthorityFrom(t.Context())
	if a.Valid() || a.Rank() != authority.RankNone {
		t.Fatalf("no claims -> zero Actor, got valid=%v rank=%d", a.Valid(), a.Rank())
	}
	if nilClaims := authn.AuthorityFrom(authn.WithClaims(t.Context(), nil)); nilClaims.Valid() {
		t.Fatal("nil claims -> zero Actor")
	}
}

func TestAuthorityFrom_TenantMember_MapsClaims(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	c := &jwt.Claims{TenantID: strings.ToUpper(tenantA), MembershipID: strings.ToUpper(selfMem), Rank: rankPtr(20), Permissions: []string{p.Roles.View}}
	// A header/path-scoped request re-binds the ctx tenant; the actor stays the caller's own.
	ctx := tenancy.WithID(authn.WithClaims(t.Context(), c), tenancy.ID(tenantB))
	a := authn.AuthorityFrom(ctx)
	if !a.Valid() || a.Rank() != authority.RankManager {
		t.Fatalf("member actor valid=%v rank=%d", a.Valid(), a.Rank())
	}
	if string(a.TenantID()) != tenantA || string(a.MembershipID()) != selfMem {
		t.Fatalf("ids = %s / %s, want canonical claims ids", a.TenantID(), a.MembershipID())
	}
	if a.IsSuperUser() || a.IsPlatformOperator() || a.IsImpersonation() {
		t.Fatal("plain member must carry no elevated flag")
	}
	if !holds(t, a, p.Roles.View) || holds(t, a, p.Users.View) {
		t.Fatal("effective permissions must be exactly the claims (the gates' source)")
	}
}

func TestAuthorityFrom_AbsentRankClaim_IsNoneNeverOwner(t *testing.T) {
	t.Parallel()
	a := authn.AuthorityFrom(authn.WithClaims(t.Context(), &jwt.Claims{TenantID: tenantA, MembershipID: selfMem}))
	if !a.Valid() || a.Rank() != authority.RankNone {
		t.Fatalf("absent rank -> valid actor with RankNone, got valid=%v rank=%d", a.Valid(), a.Rank())
	}
	owner := authn.AuthorityFrom(authn.WithClaims(t.Context(), &jwt.Claims{TenantID: tenantA, MembershipID: selfMem, Rank: rankPtr(0)}))
	if owner.Rank() != authority.RankOwner {
		t.Fatalf("explicit 0 is owner, got %d", owner.Rank())
	}
}

func TestAuthorityFrom_Operator_SuperUser_Impersonation(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	op := authn.AuthorityFrom(authn.WithClaims(t.Context(), &jwt.Claims{
		TenantID: tenantA, TenantSlug: authn.PlatformTenantSlug, IsPlatform: true, MembershipID: selfMem, Permissions: []string{p.Platform.UsersManage}}))
	if !op.IsPlatformOperator() || op.IsImpersonation() || op.IsSuperUser() {
		t.Fatal("slug-anchored operator flag only")
	}
	// is_platform without the platform slug is not an operator (slug anchor).
	notOp := authn.AuthorityFrom(authn.WithClaims(t.Context(), &jwt.Claims{TenantID: tenantA, TenantSlug: "acme", IsPlatform: true, MembershipID: selfMem}))
	if !notOp.Valid() || notOp.IsPlatformOperator() {
		t.Fatal("is_platform without the platform slug must not be an operator")
	}
	su := authn.AuthorityFrom(authn.WithClaims(t.Context(), &jwt.Claims{TenantID: tenantA, MembershipID: selfMem, IsSuperUser: true}))
	if !su.IsSuperUser() {
		t.Fatal("super user flag")
	}
	imp := authn.AuthorityFrom(authn.WithClaims(t.Context(), &jwt.Claims{
		TenantID: tenantA, MembershipID: selfMem, Rank: rankPtr(0), Permissions: []string{p.Meta.TenantAdmin},
		Act: &jwt.ActClaim{Sub: "op"}}))
	if !imp.IsImpersonation() || imp.Rank() != authority.RankAdmin || imp.IsPlatformOperator() {
		t.Fatalf("impersonation actor: imp=%v rank=%d (want Admin-equivalent whatever the claim)", imp.IsImpersonation(), imp.Rank())
	}
	// An impersonation token carries no rank claim (as minted today): Admin too.
	imp2 := authn.AuthorityFrom(authn.WithClaims(t.Context(), &jwt.Claims{TenantID: tenantA, MembershipID: selfMem, Act: &jwt.ActClaim{Sub: "op"}}))
	if imp2.Rank() != authority.RankAdmin {
		t.Fatalf("impersonation without rank claim = %d, want Admin", imp2.Rank())
	}
}

func TestAuthorityFrom_MalformedIDs_ZeroActor(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]*jwt.Claims{
		"bad tenant":      {TenantID: "tenant-1", MembershipID: selfMem, Rank: rankPtr(0)},
		"bad membership":  {TenantID: tenantA, MembershipID: "nope", Rank: rankPtr(0)},
		"empty ids":       {Rank: rankPtr(0)},
		"nil uuid tenant": {TenantID: "00000000-0000-0000-0000-000000000000", MembershipID: selfMem, Rank: rankPtr(0)},
		"padded tenant":   {TenantID: " " + tenantA, MembershipID: selfMem, Rank: rankPtr(0)},
	} {
		a := authn.AuthorityFrom(authn.WithClaims(t.Context(), c))
		if a.Valid() || a.Rank() != authority.RankNone {
			t.Errorf("%s: want the zero Actor, got valid=%v rank=%d", name, a.Valid(), a.Rank())
		}
	}
}

func TestSatisfiesInTenant(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	member := authn.WithClaims(t.Context(), &jwt.Claims{TenantID: tenantA, Permissions: []string{p.Tenants.View}})
	if !authn.SatisfiesInTenant(member, tenancy.ID(tenantA), p.Tenants.View) {
		t.Error("own tenant with the permission -> true")
	}
	if !authn.SatisfiesInTenant(member, tenancy.ID(strings.ToUpper(tenantA)), p.Tenants.View) {
		t.Error("uppercase spelling of own tenant -> true")
	}
	if authn.SatisfiesInTenant(member, tenancy.ID(tenantB), p.Tenants.View) {
		t.Error("another tenant -> false (D2)")
	}
	if authn.SatisfiesInTenant(member, tenancy.ID("not-a-uuid"), p.Tenants.View) {
		t.Error("unparseable tenant -> false")
	}
	op := authn.WithClaims(t.Context(), &jwt.Claims{TenantID: tenantA, TenantSlug: authn.PlatformTenantSlug, IsPlatform: true, Permissions: []string{p.Platform.TenantsView}})
	if !authn.SatisfiesInTenant(op, tenancy.ID(tenantB), p.Tenants.View) {
		t.Error("operator with platform.tenants.view -> any tenant")
	}
	if authn.SatisfiesInTenant(t.Context(), tenantA, p.Tenants.View) {
		t.Error("no claims -> false")
	}
}

func TestDenySelfPerson(t *testing.T) {
	t.Parallel()
	const other = "0190a000-0000-7000-8000-0000000000bb"
	const self = "0190a000-0000-7000-8000-0000000000ff"
	mk := func(super bool) *jwt.Claims {
		c := &jwt.Claims{TenantID: tenantA, MembershipID: selfMem, IsSuperUser: super, TenantSlug: authn.PlatformTenantSlug, IsPlatform: true}
		c.Subject = self
		return c
	}
	for _, tc := range []struct {
		name   string
		claims *jwt.Claims
		path   string
		want   int
	}{
		{"self upper-case denied", mk(false), strings.ToUpper(self), 403},
		{"self denied", mk(false), self, 403},
		{"other allowed", mk(false), other, 200},
		{"super user self allowed", mk(true), self, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := &sentinel{}
			h := authn.RequireFreshStamp(&fakeVerifier{claims: withFreshness(tc.claims)}, alwaysFresh{})(authn.DenySelfPerson("personId")(s.handler()))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, userReqWithVar(t, "personId", tc.path))
			if rec.Code != tc.want {
				t.Fatalf("status %d want %d (%s)", rec.Code, tc.want, rec.Body.String())
			}
			if tc.want == http.StatusForbidden && !strings.Contains(rec.Body.String(), `"self_modification_forbidden"`) {
				t.Fatalf("body %s", rec.Body.String())
			}
			if (tc.want == 200) != s.called {
				t.Fatalf("handler called = %v", s.called)
			}
		})
	}
}

func TestDenySelfPerson_NoClaims_401(t *testing.T) {
	t.Parallel()
	s := &sentinel{}
	rec := httptest.NewRecorder()
	authn.DenySelfPerson("personId")(s.handler()).ServeHTTP(rec, userReqWithVar(t, "personId", tenantA))
	if rec.Code != http.StatusUnauthorized || s.called {
		t.Fatalf("status %d called=%v", rec.Code, s.called)
	}
}
