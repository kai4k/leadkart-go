package authority

import (
	"errors"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// The private ceiling and has-permission helpers are reached only through
// decisions that already refuse a zero actor; these pin their own fail-closed
// behaviour so a future caller cannot weaken it.

const (
	tenantOne tenant.ID = "0190a000-0000-7000-8000-00000000000a"
	tenantTwo tenant.ID = "0190a000-0000-7000-8000-00000000000b"
)

func newActorT(t *testing.T, in ActorInput) Actor {
	t.Helper()
	in.MayManageRoles = true
	a, err := NewActor(in)
	if err != nil {
		t.Fatalf("NewActor: %v", err)
	}
	return a
}

func TestHolds_FailsClosed(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	var zero Actor
	super := newActorT(t, ActorInput{MembershipID: "0190a000-0000-7000-8000-000000000001", TenantID: tenantOne, IsSuperUser: true})
	op := newActorT(t, ActorInput{MembershipID: "0190a000-0000-7000-8000-000000000002", TenantID: tenantOne, IsPlatformOperator: true,
		EffectivePermissions: []string{p.Platform.UsersManage}})
	plain := newActorT(t, ActorInput{MembershipID: "0190a000-0000-7000-8000-000000000003", TenantID: tenantOne, EffectivePermissions: []string{p.Crm.Leads.Read}})

	for name, c := range map[string]struct {
		got, want bool
	}{
		"zero actor holds nothing":                                  {zero.holds(p.Crm.Leads.Read, tenantOne), false},
		"super holds a known name":                                  {super.holds(p.Platform.TenantsManage, tenantOne), true},
		"super holds no unknown name":                               {super.holds("not.a.permission", tenantOne), false},
		"super holds no empty name":                                 {super.holds("", tenantOne), false},
		"exempt operator holds what tenant.admin implies elsewhere": {op.holds(p.Crm.Leads.ReadAll, tenantTwo), true},
		"exempt operator holds no reserved name":                    {op.holds(p.Platform.TenantsManage, tenantTwo), false},
		"operator in its own tenant holds only its set":             {op.holds(p.Crm.Leads.ReadAll, tenantOne), false},
		"plain actor holds its own":                                 {plain.holds(p.Crm.Leads.Read, tenantOne), true},
		"plain actor holds no more":                                 {plain.holds(p.Crm.Leads.ReadAll, tenantOne), false},
	} {
		if c.got != c.want {
			t.Errorf("%s: holds = %v, want %v", name, c.got, c.want)
		}
	}
}

func TestMayChangePermissions_ZeroActorDenies(t *testing.T) {
	t.Parallel()
	var zero Actor
	for _, names := range [][]string{nil, {}, {permission.IdentityPermissions.Crm.Leads.Read}} {
		if err := zero.mayChangePermissions(tenantOne, names); !errors.Is(err, ErrNoAuthority) {
			t.Errorf("zero actor, names %v: %v, want no authority", names, err)
		}
	}
}

// A tenant id that does not canonicalise is never "another tenant": no
// exemption, no holding, and no ceiling decision for it, whoever asks.
func TestTenantHelpers_RefuseNonCanonicalTenants(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	super := newActorT(t, ActorInput{MembershipID: "0190a000-0000-7000-8000-000000000001", TenantID: tenantOne, IsSuperUser: true})
	op := newActorT(t, ActorInput{MembershipID: "0190a000-0000-7000-8000-000000000002", TenantID: tenantOne, IsPlatformOperator: true,
		EffectivePermissions: []string{p.Platform.UsersManage}})
	for _, bad := range []tenant.ID{"", "garbage", "00000000-0000-0000-0000-000000000000", " " + tenantTwo, tenantTwo + "\n"} {
		if op.exemptOperatorIn(bad) || op.exemptIn(bad) || super.exemptIn(bad) {
			t.Errorf("tenant %q: exempt, want never", bad)
		}
		if op.holds(p.Crm.Leads.ReadAll, bad) {
			t.Errorf("tenant %q: exempt operator holds a name, want false", bad)
		}
		if err := super.mayChangePermissions(bad, nil); !errors.Is(err, ErrNoAuthority) {
			t.Errorf("tenant %q: super mayChangePermissions = %v, want no authority", bad, err)
		}
	}
	if !op.exemptOperatorIn(tenantTwo) || !super.exemptIn(tenantTwo) {
		t.Error("a canonical other tenant must still be exempt")
	}
}
