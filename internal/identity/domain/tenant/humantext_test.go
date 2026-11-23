package tenant_test

import (
	"errors"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/humantext"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// rlo is U+202E (right-to-left override): it reorders the text around it.
var rlo = string(rune(0x202e))

var rloName = "Acme" + rlo + "Ltd"

func requireInvalidCharacters(t *testing.T, err error, field string) {
	t.Helper()
	var he *humantext.Error
	if !errors.Is(err, tenant.ErrInvalid) || !errors.As(err, &he) || he.Field != field {
		t.Fatalf("err = %v, want tenant.ErrInvalid + humantext.Error for %q", err, field)
	}
}

func TestNew_RefusesNamesWithDirectionOverrideOrControlCharacters(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ name, legal, display, field string }{
		{"display name", "Acme Pvt Ltd", rloName, "display_name"},
		{"legal name", rloName, "Acme", "legal_name"},
		{"control character", "Acme\x07Ltd", "Acme", "legal_name"},
		{"line feed", "Acme", "Acme\nLtd", "display_name"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, err := tenant.New(tenant.ID("11111111-1111-1111-1111-111111111111"), mustSlug(t, "acme-pharma"), c.legal, c.display, mustEmail(t, "a@b.io"), testNow)
			requireInvalidCharacters(t, err, c.field)
		})
	}
}

func legacyTenant(t *testing.T) *tenant.Tenant {
	t.Helper()
	return tenant.UnmarshalFromDB(tenant.Snapshot{
		ID: tenant.ID("11111111-1111-1111-1111-111111111111"), Slug: mustSlug(t, "acme-pharma"),
		LegalName: rloName, DisplayName: rloName, Status: tenant.StatusActive, CreatedAt: testNow,
	})
}

func TestUpdateProfile_RefusesAChangedNameButAcceptsAnUnchangedLegacyOne(t *testing.T) {
	t.Parallel()
	tn := legacyTenant(t)

	// A full replace that re-sends the stored (legacy) values changes nothing and is accepted.
	if err := tn.UpdateProfile(rloName, rloName, testNow); err != nil {
		t.Fatalf("unchanged legacy values must be accepted: %v", err)
	}
	// Changing one field to another bad value is refused, naming that field.
	requireInvalidCharacters(t, tn.UpdateProfile(rloName, rloName+"x", testNow), "display_name")
	requireInvalidCharacters(t, tn.UpdateProfile(rloName+"x", rloName, testNow), "legal_name")
	if tn.DisplayName() != rloName || tn.LegalName() != rloName {
		t.Fatal("a refused update must change nothing")
	}
	// Fixing one field while re-sending the other legacy value is accepted.
	if err := tn.UpdateProfile("Acme Pvt Ltd", rloName, testNow); err != nil {
		t.Fatalf("fixing one field: %v", err)
	}
}

func TestSuspend_RefuseReasonsWithControlCharacters(t *testing.T) {
	t.Parallel()
	active := func() *tenant.Tenant {
		return tenant.UnmarshalFromDB(tenant.Snapshot{ID: tenant.ID("11111111-1111-1111-1111-111111111111"),
			Slug: mustSlug(t, "acme-pharma"), LegalName: "Acme", DisplayName: "Acme", Status: tenant.StatusActive, CreatedAt: testNow})
	}
	requireInvalidCharacters(t, active().Suspend("unpaid"+rlo+"invoice", testNow), "reason")
}
