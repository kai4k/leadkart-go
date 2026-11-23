package authority

import (
	"strings"
	"testing"
	"time"
)

// Version is the precondition of a replace-overrides save: two override states
// have the same version exactly when they are the same set (Equal), so a save
// made from a read that no longer matches the stored state is told so instead
// of being applied.
func TestOverridesVersion_SameSetSameVersion_AnyChangeADifferentOne(t *testing.T) {
	t.Parallel()
	a, b := Override{Name: "a"}, Override{Name: "b"}
	for _, tc := range []struct {
		name string
		x, y Overrides
		same bool
	}{
		{"identical, any order", ov([]Override{a, b}, "r", "s"), ov([]Override{b, a}, "s", "r"), true},
		{"nil and empty lists", ov(nil), Overrides{Granted: []Override{}, Revoked: []string{}}, true},
		{"a name listed twice is one name", ov([]Override{a, a}, "r", "r"), ov([]Override{a}, "r"), true},
		{"same expiry, different location", ov([]Override{{Name: "a", ExpiresAt: eqT}}), ov([]Override{{Name: "a", ExpiresAt: eqT.In(time.FixedZone("x", 3600))}}), true},
		{"a grant added", ov([]Override{a}), ov([]Override{a, b}), false},
		{"a grant removed (the expiry case: a live grant that is no longer live)", ov([]Override{a, {Name: "b", ExpiresAt: eqT}}), ov([]Override{a}), false},
		{"an expiry changed", ov([]Override{{Name: "a", ExpiresAt: eqT}}), ov([]Override{{Name: "a", ExpiresAt: eqT.Add(time.Second)}}), false},
		{"timed against permanent", ov([]Override{{Name: "a", ExpiresAt: eqT}}), ov([]Override{a}), false},
		{"no expiry reads as permanent, the epoch included", ov([]Override{{Name: "a", ExpiresAt: time.Unix(0, 0)}}), ov([]Override{a}), false},
		{"a revocation added", ov([]Override{a}), ov([]Override{a}, "r"), false},
		{"a revocation removed", ov(nil, "r", "s"), ov(nil, "r"), false},
		{"granted against revoked", ov([]Override{a}), ov(nil, "a"), false},
		{"a name that moved between the lists", ov([]Override{a}, "b"), ov([]Override{b}, "a"), false},
		{"names are not concatenated", ov(nil, "ab", "c"), ov(nil, "a", "bc"), false},
		{"a name cannot imitate two entries", ov(nil, "a\nb"), ov(nil, "a", "b"), false},
		{"a name cannot imitate an expiry", ov([]Override{{Name: "a@permanent"}}), ov([]Override{a}), false},
		{"a revoked name cannot carry the entry separator", ov(nil, "a\nb", "c"), ov(nil, "a", "b\nc"), false},
		{"a granted name cannot carry the entry separator", ov([]Override{{Name: "a@permanent\nb"}, {Name: "c"}}), ov([]Override{a, {Name: "b@permanent\nc"}}), false},
		{"a revoked name cannot imitate a grant", ov(nil, "a@permanent"), ov([]Override{a}), false},
		{"the same name granted and revoked is not the name granted twice", ov([]Override{a}, "a"), ov([]Override{a}), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			vx, vy := tc.x.Version(), tc.y.Version()
			if vx == "" || vy == "" {
				t.Fatalf("versions = %q, %q, want never empty (an empty version must never match)", vx, vy)
			}
			if (vx == vy) != tc.same {
				t.Errorf("versions %q and %q: same = %v, want %v", vx, vy, vx == vy, tc.same)
			}
			if got := tc.x.Equal(tc.y); got != tc.same {
				t.Errorf("Equal = %v but the versions say %v: the two must agree", got, tc.same)
			}
		})
	}
}

// The version says nothing about what the overrides are: it is a fixed-length
// token, the same length for an empty set and for a large one.
func TestOverridesVersion_IsOpaqueAndStable(t *testing.T) {
	t.Parallel()
	empty := ov(nil).Version()
	big := ov([]Override{{Name: "crm.leads.read"}, {Name: "crm.leads.assign", ExpiresAt: eqT}}, "identity.roles.view").Version()
	if len(empty) != len(big) {
		t.Errorf("version lengths %d and %d differ: the token must not grow with the set", len(empty), len(big))
	}
	for _, name := range []string{"crm.leads.read", "crm.leads.assign", "identity.roles.view"} {
		if strings.Contains(big, name) {
			t.Errorf("version %q contains the permission name %s", big, name)
		}
	}
	if again := ov([]Override{{Name: "crm.leads.assign", ExpiresAt: eqT}, {Name: "crm.leads.read"}}, "identity.roles.view").Version(); again != big {
		t.Errorf("the same state gave %q then %q", big, again)
	}
}
