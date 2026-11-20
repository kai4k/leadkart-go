package authority

import (
	"slices"
	"testing"
	"time"
)

var eqT = time.Date(2026, 12, 1, 9, 0, 0, 0, time.UTC)

func ov(g []Override, r ...string) Overrides { return Overrides{Granted: g, Revoked: r} }

// Equal compares SETS in both directions: equal only when every granted name
// (with its expiry) and every revoked name appears on both sides, however often
// a name is listed. The regression: {"granted":["a","a"]} against a member
// holding {a, b} looked equal (same length, every entry of one side present in
// the other), so nothing was written and the member kept b.
func TestOverridesEqual_IsSetEqualityBothWays(t *testing.T) {
	t.Parallel()
	a, b := Override{Name: "a"}, Override{Name: "b"}
	for _, tc := range []struct {
		name  string
		x, y  Overrides
		equal bool
	}{
		{"identical", ov([]Override{a, b}, "r"), ov([]Override{b, a}, "r"), true},
		{"both empty", ov(nil), ov([]Override{}), true},
		{"duplicate entry against a different second name", ov([]Override{a, a}), ov([]Override{a, b}), false},
		{"duplicate entry, reversed", ov([]Override{a, b}), ov([]Override{a, a}), false},
		{"duplicates collapse to the same set", ov([]Override{a, a}), ov([]Override{a}), true},
		{"granted subset", ov([]Override{a}), ov([]Override{a, b}), false},
		{"granted superset", ov([]Override{a, b}), ov([]Override{a}), false},
		{"same length, different names", ov([]Override{a, b}), ov([]Override{a, {Name: "c"}}), false},
		{"revoked subset", ov(nil, "r"), ov(nil, "r", "s"), false},
		{"revoked superset", ov(nil, "r", "s"), ov(nil, "r"), false},
		{"revoked duplicate against a different second name", ov(nil, "r", "r"), ov(nil, "r", "s"), false},
		{"revoked duplicates collapse", ov(nil, "r", "r"), ov(nil, "r"), true},
		{"expiry differs", ov([]Override{{Name: "a", ExpiresAt: eqT}}), ov([]Override{{Name: "a", ExpiresAt: eqT.Add(time.Hour)}}), false},
		{"timed against permanent", ov([]Override{{Name: "a", ExpiresAt: eqT}}), ov([]Override{a}), false},
		{"same expiry, different location", ov([]Override{{Name: "a", ExpiresAt: eqT}}), ov([]Override{{Name: "a", ExpiresAt: eqT.In(time.FixedZone("x", 3600))}}), true},
		{"granted vs revoked are different lists", ov([]Override{a}), ov(nil, "a"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.x.Equal(tc.y); got != tc.equal {
				t.Errorf("x.Equal(y) = %v, want %v", got, tc.equal)
			}
			if got := tc.y.Equal(tc.x); got != tc.equal {
				t.Errorf("y.Equal(x) = %v, want %v (Equal must be symmetric)", got, tc.equal)
			}
		})
	}
}

// laterThan / conferredByOverrides: only a LONGER grant, a new grant or an
// un-revoke confers anything. An unchanged timed grant confers nothing, so an
// actor who holds the name only until a date may leave it as it is; extending
// it, or making it permanent, is conferring.
func TestConferredByOverrides_OnlyLongerGrantsConfer(t *testing.T) {
	t.Parallel()
	later := eqT.Add(time.Hour)
	for _, tc := range []struct {
		name          string
		before, after Overrides
		want          []string
	}{
		{"unchanged timed grant", ov([]Override{{Name: "x", ExpiresAt: eqT}}), ov([]Override{{Name: "x", ExpiresAt: eqT}}), nil},
		{"unchanged permanent grant", ov([]Override{{Name: "x"}}), ov([]Override{{Name: "x"}}), nil},
		{"extended expiry", ov([]Override{{Name: "x", ExpiresAt: eqT}}), ov([]Override{{Name: "x", ExpiresAt: later}}), []string{"x"}},
		{"shortened expiry", ov([]Override{{Name: "x", ExpiresAt: later}}), ov([]Override{{Name: "x", ExpiresAt: eqT}}), nil},
		{"timed made permanent", ov([]Override{{Name: "x", ExpiresAt: eqT}}), ov([]Override{{Name: "x"}}), []string{"x"}},
		{"permanent made timed", ov([]Override{{Name: "x"}}), ov([]Override{{Name: "x", ExpiresAt: eqT}}), nil},
		{"new grant", ov(nil), ov([]Override{{Name: "x"}}), []string{"x"}},
		{"un-revoke", ov(nil, "x"), ov(nil), []string{"x"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := conferredByOverrides(tc.before, tc.after)
			if !slices.Equal(got, tc.want) {
				t.Errorf("conferred = %v, want %v", got, tc.want)
			}
		})
	}
}

// Through the policy: an actor who holds X only until a date can keep a member's
// UNCHANGED X grant (a re-save of the same overrides) but cannot extend it.
func TestMayChangeOverrides_AnUnchangedGrantNeedsNoDurableHolding(t *testing.T) {
	t.Parallel()
	actor := tokenActor(t, RankManager, "crm.leads.read")
	actor.holdUntil = map[string]time.Time{"crm.leads.read": eqT.Add(24 * time.Hour)}
	before := ov([]Override{{Name: "crm.leads.read", ExpiresAt: eqT}})

	same := ov([]Override{{Name: "crm.leads.read", ExpiresAt: eqT}})
	if err := actor.mayConfer(conferredByOverrides(before, same)); err != nil {
		t.Fatalf("keeping an unchanged grant was refused: %v", err)
	}
	longer := ov([]Override{{Name: "crm.leads.read", ExpiresAt: eqT.Add(time.Hour)}})
	if err := actor.mayConfer(conferredByOverrides(before, longer)); err == nil {
		t.Fatal("extending a grant the actor holds only time-bound was allowed")
	}
}

// ADR 0069 D4/D8: an approval is BOUNDED (capped at the approver's own date) for a
// name the requester does not have revoked, but a name on the requester's REVOKED
// list is an un-revoke, which is conferral through the overrides path and is
// REFUSED when the approver holds the name only until a date: the receiver would
// keep it after the approver's grant ended.
func TestApproval_RevokedNameHeldOnlyTimeBoundIsRefusedNotCapped(t *testing.T) {
	t.Parallel()
	const name = "crm.leads.read"
	until := eqT.Add(24 * time.Hour)
	actor := tokenActor(t, RankManager, name) // a manager must hold what it approves
	actor.holdUntil = map[string]time.Time{name: until}

	// not revoked: the approval's expiry is capped at the approver's grant
	wanted := eqT.Add(30 * 24 * time.Hour)
	if got := actor.CapApprovalExpiry(Member{valid: true, tenantID: tenantOne}, name, wanted); !got.Equal(until) {
		t.Errorf("cap = %v, want the approver's own end %v", got, until)
	}

	// revoked: the same grant is an un-revoke, and conferring it needs a durable hold
	before := ov(nil, name)
	after := ov([]Override{{Name: name, ExpiresAt: until}})
	if err := actor.mayConfer(conferredByOverrides(before, after)); err == nil {
		t.Fatal("un-revoking a name the approver holds only time-bound was allowed")
	}
	// a durable holder may
	durable := tokenActor(t, RankAdmin, name)
	if err := durable.mayConfer(conferredByOverrides(before, after)); err != nil {
		t.Fatalf("a durable holder was refused: %v", err)
	}
}
