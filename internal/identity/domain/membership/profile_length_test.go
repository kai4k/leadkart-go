package membership_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/humantext"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
)

// designation and department are capped at 120 and the status message at 280, in
// UTF-8 BYTES (the unit of every other identity name rule), measured after trimming.
func TestUpdateProfile_FieldsAreCappedAfterTrimming(t *testing.T) {
	t.Parallel()
	cases := []struct {
		field string
		max   int
		set   func(*membership.Membership, string) error
	}{
		{"designation", 120, func(m *membership.Membership, v string) error { return m.UpdateProfile(v, "", "", testNow) }},
		{"department", 120, func(m *membership.Membership, v string) error { return m.UpdateProfile("", v, "", testNow) }},
		{"status_message", 280, func(m *membership.Membership, v string) error { return m.UpdateProfile("", "", v, testNow) }},
	}
	for _, c := range cases {
		t.Run(c.field, func(t *testing.T) {
			t.Parallel()
			m := newMembership(t)
			if err := c.set(m, "  "+strings.Repeat("a", c.max)+"  "); err != nil {
				t.Fatalf("%d characters padded with spaces was refused: %v", c.max, err)
			}
			err := c.set(m, strings.Repeat("a", c.max+1))
			var he *humantext.Error
			if !errors.Is(err, membership.ErrInvalid) || !errors.As(err, &he) || he.Field != c.field || he.WireCode() != "too_long" {
				t.Fatalf("%d characters: err = %v, want ErrInvalid + too_long on %s", c.max+1, err, c.field)
			}
			// characters, not bytes: the cap's worth of 3-byte Devanagari letters is accepted
			if err := c.set(m, strings.Repeat("अ", c.max)); err != nil {
				t.Errorf("%d Devanagari characters (%d bytes) was refused: %v", c.max, 3*c.max, err)
			}
			if err := c.set(m, strings.Repeat("अ", c.max+1)); !errors.Is(err, membership.ErrInvalid) {
				t.Errorf("%d Devanagari characters was accepted", c.max+1)
			}
		})
	}
}

// A stored value over the cap (saved before it existed) is accepted when re-sent
// unchanged, and never blocks an edit of another field.
func TestUpdateProfile_AnUnchangedOverlongStoredValueStaysAccepted(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("d", 200)
	legacy := membership.UnmarshalFromDB(membership.Snapshot{
		ID: membership.ID("0190a000-0000-7000-8000-000000000001"), PersonID: "0190a000-0000-7000-8000-000000000002",
		TenantID: "0190a000-0000-7000-8000-000000000003", Status: membership.StatusActive,
		Designation: long, Department: "Sales", StatusMessage: "ok",
	})
	if err := legacy.UpdateProfile(long, "Sales", "ok", testNow); err != nil {
		t.Fatalf("re-sending the stored overlong designation: %v", err)
	}
	if err := legacy.UpdateProfile(long, "Marketing", "ok", testNow); err != nil {
		t.Fatalf("editing another field beside an overlong stored one: %v", err)
	}
	if err := legacy.UpdateProfile(long+"x", "Marketing", "ok", testNow); !errors.Is(err, membership.ErrInvalid) {
		t.Errorf("changing the overlong value to another overlong one: err = %v, want ErrInvalid", err)
	}
}
