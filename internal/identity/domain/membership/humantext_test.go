package membership_test

import (
	"errors"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/humantext"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
)

var rlo = string(rune(0x202e))

func TestUpdateProfile_RefusesChangedFieldsWithDirectionOverrideButAcceptsUnchangedLegacyOnes(t *testing.T) {
	t.Parallel()
	legacy := membership.UnmarshalFromDB(membership.Snapshot{
		ID: membership.ID("0190a000-0000-7000-8000-000000000001"), PersonID: "0190a000-0000-7000-8000-000000000002",
		TenantID: "0190a000-0000-7000-8000-000000000003", Status: membership.StatusActive,
		Designation: "Head" + rlo + "Sales", Department: "Sales", StatusMessage: "ok",
	})
	// Re-sending the stored legacy designation unchanged is accepted.
	if err := legacy.UpdateProfile("Head"+rlo+"Sales", "Sales", "ok", testNow); err != nil {
		t.Fatalf("unchanged legacy designation must be accepted: %v", err)
	}
	for _, c := range []struct{ field, d, dep, sm string }{
		{"designation", "Head" + rlo + "Sales2", "Sales", "ok"},
		{"department", "Head" + rlo + "Sales", "Sa\x00les", "ok"},
		{"status_message", "Head" + rlo + "Sales", "Sales", "in a\nmeeting"},
	} {
		err := legacy.UpdateProfile(c.d, c.dep, c.sm, testNow)
		var he *humantext.Error
		if !errors.Is(err, membership.ErrInvalid) || !errors.As(err, &he) || he.Field != c.field {
			t.Errorf("%s: err = %v, want membership.ErrInvalid + humantext.Error", c.field, err)
		}
	}
	if legacy.Department() != "Sales" || legacy.StatusMessage() != "ok" {
		t.Fatal("a refused update must change nothing")
	}
}

func TestDeactivate_RefusesAReasonWithControlCharacters(t *testing.T) {
	t.Parallel()
	err := newMembership(t).Deactivate("left"+rlo+"company", testNow)
	var he *humantext.Error
	if !errors.Is(err, membership.ErrInvalid) || !errors.As(err, &he) || he.Field != "reason" {
		t.Fatalf("err = %v", err)
	}
}
