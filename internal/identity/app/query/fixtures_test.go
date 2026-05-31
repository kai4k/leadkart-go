// fixtures_test.go — shared domain-aggregate builders for query handler
// unit tests. Each helper constructs a fresh aggregate; no shared state,
// so t.Parallel-safe without sync primitives (ADR 0062, TDL canon §6).

package query_test

import (
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/druglicence"
	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/common/gst"
	"github.com/leadkart/leadkart-go/internal/common/pan"
	"github.com/leadkart/leadkart-go/internal/common/phone"
	"github.com/leadkart/leadkart-go/internal/common/postaladdress"
	"github.com/leadkart/leadkart-go/internal/common/slug"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permissionrequest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/rolehierarchy"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// testNow is a fixed clock shared across the query-test corpus.
var testNow = time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)

// Canonical IDs shared across the suite; constants keep cmp.Diff stable.
const (
	testTenantID      = tenant.ID("11111111-1111-1111-1111-111111111111")
	testTenantSlugStr = "acme-pharma"
	testPersonID      = person.ID("22222222-2222-2222-2222-222222222222")
	testMembershipID  = membership.ID("33333333-3333-3333-3333-333333333333")
	testRoleID        = role.ID("44444444-4444-4444-4444-444444444444")
	testEdgeID        = rolehierarchy.ID("55555555-5555-5555-5555-555555555555")
	testRequestID     = permissionrequest.ID("66666666-6666-6666-6666-666666666666")
	testParentRoleID  = role.ID("77777777-7777-7777-7777-777777777777")
	testApproverID    = membership.ID("88888888-8888-8888-8888-888888888888")
	testEmail         = "alice@example.test"
)

// mustEmail constructs an email.Address or fails the test.
func mustEmail(t *testing.T, raw string) email.Address {
	t.Helper()
	a, err := email.New(raw)
	if err != nil {
		t.Fatalf("email.New(%q): %v", raw, err)
	}
	return a
}

// mustSlug constructs a slug.Slug or fails the test.
func mustSlug(t *testing.T, raw string) slug.Slug {
	t.Helper()
	s, err := slug.New(raw)
	if err != nil {
		t.Fatalf("slug.New(%q): %v", raw, err)
	}
	return s
}

// passwordHash returns a valid-shaped PasswordHash for projection tests.
func passwordHash(t *testing.T) person.PasswordHash {
	t.Helper()
	// argon2id PHC shape (RFC 9106); satisfies NewPasswordHash's parse.
	raw := "$argon2id$v=19$m=65536,t=3,p=2$c29tZXNhbHRzb21lc2FsdA$kJiHa+vQqI2/9R8sHnSr5h7vrXxoy0YZv2v5cPmw3lI"
	h, err := person.NewPasswordHash(raw)
	if err != nil {
		t.Fatalf("person.NewPasswordHash: %v", err)
	}
	return h
}

// newPerson builds a canonical Person for projection tests.
func newPerson(t *testing.T) *person.Person {
	t.Helper()
	p, err := person.New(testPersonID, mustEmail(t, testEmail), "Alice", "Liddell", passwordHash(t), testNow)
	if err != nil {
		t.Fatalf("person.New: %v", err)
	}
	return p
}

// newPersonAt builds a Person with caller-specified ID, email, and names.
func newPersonAt(t *testing.T, id person.ID, mailAddr, first, last string) *person.Person {
	t.Helper()
	p, err := person.New(id, mustEmail(t, mailAddr), first, last, passwordHash(t), testNow)
	if err != nil {
		t.Fatalf("person.New(%s): %v", id, err)
	}
	return p
}

// newMembership builds an Active Membership (JoinedAt=testNow) for the given tenant + person.
func newMembership(t *testing.T, id membership.ID, personID person.ID, tenantID tenant.ID) *membership.Membership {
	t.Helper()
	m, err := membership.New(id, personID, tenantID, membership.ID(""), testNow)
	if err != nil {
		t.Fatalf("membership.New(%s): %v", id, err)
	}
	return m
}

// newRole builds a role with HierarchyLevelDefault, isSystemDefault=false, isSuperAdmin=false.
func newRole(t *testing.T, id role.ID, tenantID tenant.ID, name string) *role.Role {
	t.Helper()
	r, err := role.New(id, tenantID, name, false, role.HierarchyLevelDefault, false, testNow)
	if err != nil {
		t.Fatalf("role.New(%s): %v", id, err)
	}
	return r
}

// newSuperAdminRole builds a role with isSuperAdmin=true, isSystemDefault=true.
func newSuperAdminRole(t *testing.T, id role.ID, tenantID tenant.ID) *role.Role {
	t.Helper()
	r, err := role.New(id, tenantID, "SuperAdmin", true, role.HierarchyLevelMin, true, testNow)
	if err != nil {
		t.Fatalf("role.New SuperAdmin: %v", err)
	}
	return r
}

// newEdge builds an active rolehierarchy.Edge for the child → parent pair.
func newEdge(t *testing.T, id rolehierarchy.ID, tenantID tenant.ID, child, parent role.ID) *rolehierarchy.Edge {
	t.Helper()
	e, err := rolehierarchy.New(id, tenantID, child, parent, membership.ID(""), "", testNow)
	if err != nil {
		t.Fatalf("rolehierarchy.New: %v", err)
	}
	return e
}

// newTenant builds a minimal Tenant (no statutory/contact/settings/prefs).
func newTenant(t *testing.T, id tenant.ID, slugStr string) *tenant.Tenant {
	t.Helper()
	tn, err := tenant.New(id, mustSlug(t, slugStr), "Acme Pharma Pvt Ltd", "Acme Pharma", mustEmail(t, "admin@acme.test"), testNow)
	if err != nil {
		t.Fatalf("tenant.New: %v", err)
	}
	return tn
}

// newFullyPopulatedTenant builds a Tenant with all VOs set for projection round-trip tests.
func newFullyPopulatedTenant(t *testing.T) *tenant.Tenant {
	t.Helper()
	tn := newTenant(t, testTenantID, testTenantSlugStr)

	// Statutory — valid PAN/GST (position-4 = P = Individual).
	p, err := pan.New("ABCPE1234F")
	if err != nil {
		t.Fatalf("pan.New: %v", err)
	}
	g, err := gst.New("29ABCPE1234F1Z5")
	if err != nil {
		t.Fatalf("gst.New: %v", err)
	}
	dl, err := druglicence.New("KA-MUM-2024-12345")
	if err != nil {
		t.Fatalf("druglicence.New: %v", err)
	}
	stat, err := tenant.NewStatutory(g, p, dl)
	if err != nil {
		t.Fatalf("tenant.NewStatutory: %v", err)
	}
	if err := tn.UpdateStatutory(stat, testNow); err != nil {
		t.Fatalf("UpdateStatutory: %v", err)
	}

	// AdminContact.
	ph, err := phone.New("+919999999999")
	if err != nil {
		t.Fatalf("phone.New: %v", err)
	}
	addr, err := postaladdress.New("12 MG Road", "Bengaluru", "Bengaluru Urban", "Karnataka", "KA", "560001")
	if err != nil {
		t.Fatalf("postaladdress.New: %v", err)
	}
	ac := tenant.NewAdminContact(ph, addr)
	if err := tn.UpdateAdminContact(ac, testNow); err != nil {
		t.Fatalf("UpdateAdminContact: %v", err)
	}

	// Settings.
	policy, err := tenant.NewPasswordPolicy(14, true, true, true, false, 8, 20)
	if err != nil {
		t.Fatalf("NewPasswordPolicy: %v", err)
	}
	if err := tn.UpdateSettings(tenant.NewSettings(policy), testNow); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}

	// Prefs.
	prefs, err := tenant.NewDisplayPreferences("en-IN", "Asia/Kolkata", "DD-MMM-YYYY", "INR")
	if err != nil {
		t.Fatalf("NewDisplayPreferences: %v", err)
	}
	if err := tn.UpdateDisplayPreferences(prefs, testNow); err != nil {
		t.Fatalf("UpdateDisplayPreferences: %v", err)
	}
	return tn
}

// metaTenantAdminPermission returns the catalogue Meta.TenantAdmin permission.
func metaTenantAdminPermission(t *testing.T) *permission.Permission {
	t.Helper()
	p, err := permission.Create(permission.IdentityPermissions.Meta.TenantAdmin)
	if err != nil {
		t.Fatalf("permission.Create: %v", err)
	}
	return p
}
