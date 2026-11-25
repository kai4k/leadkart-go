package authn_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/app/jwt"
	"github.com/leadkart/leadkart-go/internal/identity/domain/authority"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/identity/ports/authn"
)

const platformTenantID = "0190a000-0000-7000-8000-0000000000bb"

// G2p n1+n3: every actor knows the platform tenant's id, not only platform-slug tokens. The id comes
// from the resolver the composition root installed (the database), never from the token or the request.
func TestAuthorityFrom_EveryActorKnowsThePlatformTenant(t *testing.T) {
	t.Parallel()
	resolver := func(context.Context) tenant.ID { return platformTenantID }
	cases := map[string]*jwt.Claims{
		"company member":        {TenantID: tenantA, TenantSlug: "acme", MembershipID: selfMem},
		"company super user":    {TenantID: tenantA, TenantSlug: "acme", MembershipID: selfMem, IsSuperUser: true},
		"company impersonation": {TenantID: tenantA, TenantSlug: "acme", MembershipID: selfMem, Act: &jwt.ActClaim{Sub: "op"}},
		"platform operator":     {TenantID: platformTenantID, TenantSlug: authn.PlatformTenantSlug, IsPlatform: true, MembershipID: selfMem},
		"platform super user":   {TenantID: platformTenantID, TenantSlug: authn.PlatformTenantSlug, MembershipID: selfMem, IsSuperUser: true},
	}
	for name, c := range cases {
		a := authn.AuthorityFrom(authn.WithPlatformTenant(authn.WithClaims(t.Context(), c), resolver))
		if got := a.PlatformTenantID(); got != platformTenantID {
			t.Errorf("%s: platform tenant id = %q, want %q", name, got, platformTenantID)
		}
	}
}

// Without a resolver (or when it knows nothing) the platform-slug token still names its own tenant and
// a company token names none: the safe default is "every tenant is a company".
func TestAuthorityFrom_PlatformTenantFallback(t *testing.T) {
	t.Parallel()
	unknown := func(context.Context) tenant.ID { return "" }
	company := &jwt.Claims{TenantID: tenantA, TenantSlug: "acme", MembershipID: selfMem}
	platform := &jwt.Claims{TenantID: platformTenantID, TenantSlug: authn.PlatformTenantSlug, IsPlatform: true, MembershipID: selfMem}
	for name, ctx := range map[string]context.Context{
		"no resolver":      t.Context(),
		"resolver unknown": authn.WithPlatformTenant(t.Context(), unknown),
	} {
		if got := authn.AuthorityFrom(authn.WithClaims(ctx, company)).PlatformTenantID(); got != "" {
			t.Errorf("%s: company token platform id = %q, want empty", name, got)
		}
		if got := authn.AuthorityFrom(authn.WithClaims(ctx, platform)).PlatformTenantID(); got != platformTenantID {
			t.Errorf("%s: platform token platform id = %q, want its own tenant", name, got)
		}
	}
}

// G2p n1: a company super user acting in the platform tenant gets the platform rule in production:
// it may not lift a colleague's managed revoke there (that would confer the name). It still can in a
// company, and without the resolver (unknown platform tenant) the platform tenant is treated as a company.
func TestAuthorityFrom_CompanySuperUserInThePlatformTenantGetsThePlatformRule(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	staff := membershipOf(t, platformTenantID, p.Roles.Manage)
	companyStaff := membershipOf(t, tenantA, p.Roles.Manage)
	lift := authority.Overrides{Granted: []authority.Override{}, Revoked: []string{}}
	c := &jwt.Claims{TenantID: tenantA, TenantSlug: "acme", MembershipID: selfMem, IsSuperUser: true}

	withDB := authn.AuthorityFrom(authn.WithPlatformTenant(authn.WithClaims(t.Context(), c), func(context.Context) tenant.ID { return platformTenantID }))
	if err := withDB.MayChangeOverrides(staff, lift); !errors.Is(err, authority.ErrGrantExceedsAuthority) {
		t.Errorf("company super user lifts a managed revoke in the platform tenant: %v, want ErrGrantExceedsAuthority", err)
	}
	if err := withDB.MayChangeOverrides(companyStaff, lift); err != nil {
		t.Errorf("company super user lifts a stale managed revoke in a company: %v, want nil", err)
	}
	unknown := authn.AuthorityFrom(authn.WithClaims(t.Context(), c))
	if err := unknown.MayChangeOverrides(staff, lift); err != nil {
		t.Errorf("platform tenant unknown: %v, want the company rule (nil)", err)
	}
}

func membershipOf(t *testing.T, tid, revoked string) authority.Member {
	t.Helper()
	m := membership.UnmarshalFromDB(membership.Snapshot{ID: membership.ID("0190a000-0000-7000-8000-0000000000cc"), PersonID: person.ID("0190a000-0000-7000-8000-0000000000dd"),
		TenantID: tenant.ID(tid), Status: membership.StatusActive, JoinedAt: time.Unix(1_700_000_000, 0).UTC(),
		RevokedPermissions: []*permission.Permission{permission.FromConstant(revoked)}})
	out, err := authority.MemberOf(m, nil, time.Unix(1_700_000_100, 0).UTC(), authority.NewPersonState(true, false))
	if err != nil {
		t.Fatalf("MemberOf: %v", err)
	}
	return out
}

// The middleware installs the resolver on the request context for the handlers behind it.
func TestPlatformTenantMiddleware_InstallsTheResolver(t *testing.T) {
	t.Parallel()
	var got tenant.ID
	h := authn.PlatformTenant(func(context.Context) tenant.ID { return platformTenantID })(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		c := &jwt.Claims{TenantID: tenantA, TenantSlug: "acme", MembershipID: selfMem}
		got = authn.AuthorityFrom(authn.WithClaims(r.Context(), c)).PlatformTenantID()
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))
	if got != platformTenantID {
		t.Fatalf("platform id behind the middleware = %q, want %q", got, platformTenantID)
	}
}
