package tenant_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/humantext"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// The columns check length() <= 200, which counts characters; so does the domain.
// 200 Devanagari letters are 600 bytes and must be accepted, 201 refused.
func TestTenantNames_AreCountedInCharacters(t *testing.T) {
	t.Parallel()
	ok, over := strings.Repeat("अ", 200), strings.Repeat("अ", 201)
	if _, err := tenant.New(newID(t), mustSlug(t, "hindi-co"), ok, ok, mustEmail(t, "a@b.io"), testNow); err != nil {
		t.Fatalf("200 characters (600 bytes) refused on create: %v", err)
	}
	for field, args := range map[string][2]string{"legal_name": {over, "D"}, "display_name": {"L", over}} {
		if _, err := tenant.New(newID(t), mustSlug(t, "hindi-co"), args[0], args[1], mustEmail(t, "a@b.io"), testNow); !errors.Is(err, tenant.ErrInvalid) {
			t.Errorf("create with 201-character %s: err = %v, want ErrInvalid", field, err)
		}
	}
	tn, err := tenant.New(newID(t), mustSlug(t, "hindi-co"), "L", "D", mustEmail(t, "a@b.io"), testNow)
	if err != nil {
		t.Fatal(err)
	}
	if err := tn.UpdateProfile(ok, ok, testNow); err != nil {
		t.Fatalf("200 characters refused on update: %v", err)
	}
	if err := tn.UpdateProfile(over, ok, testNow); !errors.Is(err, tenant.ErrInvalid) {
		t.Errorf("update with a 201-character legal name: err = %v", err)
	}
	if err := tn.UpdateProfile(ok, over, testNow); !errors.Is(err, tenant.ErrInvalid) {
		t.Errorf("update with a 201-character display name: err = %v", err)
	}
}

// An unchanged stored name is not measured again (only a changed one is), but a
// changed one that is still over the cap is refused.
func TestUpdateProfile_OnlyAChangedOverlongNameIsRefused(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("n", 250)
	fresh := func() *tenant.Tenant {
		return tenant.UnmarshalFromDB(tenant.Snapshot{ID: newID(t), Slug: mustSlug(t, "long-co"), LegalName: long, DisplayName: long, Status: tenant.StatusActive})
	}
	tn := fresh()
	if err := tn.UpdateProfile(long, long, testNow); err != nil {
		t.Fatalf("re-sending the stored over-long names: %v", err)
	}
	tn = fresh()
	if err := tn.UpdateProfile(long, "Short", testNow); err != nil {
		t.Fatalf("changing only the display name beside an over-long legal name: %v", err)
	}
	tn = fresh()
	if err := tn.UpdateProfile("Short", long, testNow); err != nil {
		t.Fatalf("changing only the legal name beside an over-long display name: %v", err)
	}
	for name, args := range map[string][2]string{"legal": {long + "x", "Short"}, "display": {"Short", long + "x"}} {
		err := fresh().UpdateProfile(args[0], args[1], testNow)
		var he *humantext.Error
		if !errors.Is(err, tenant.ErrInvalid) || !errors.As(err, &he) || he.WireCode() != "too_long" {
			t.Errorf("a changed over-long %s name: err = %v, want too_long", name, err)
		}
	}
}
