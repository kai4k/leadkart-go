package command_test

import (
	"context"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// fixedNow is the deterministic instant test fixtures pass to
// handlers per the clock-injection refactor.
var fixedNow = time.Date(2026, 5, 26, 10, 0, 0, 0, time.UTC)

// fakeHierarchy is the in-test HierarchyReader. Subordinates is a
// map keyed by the actor membership ID with each value being the set
// of memberships visible to that actor (transitively + including the
// actor itself per the contract).
type fakeHierarchy struct {
	Subordinates map[string][]string
}

func (f *fakeHierarchy) ListSubordinateMembershipIDs(_ context.Context, _ tenant.ID, membershipID string) ([]string, error) {
	if subs, ok := f.Subordinates[membershipID]; ok {
		return append([]string{}, subs...), nil
	}
	// Per contract: a known membership always sees itself. We
	// approximate "known" as "appears anywhere as an actor or subordinate."
	for actor, subs := range f.Subordinates {
		if actor == membershipID {
			return append([]string{}, subs...), nil
		}
		for _, s := range subs {
			if s == membershipID {
				return []string{membershipID}, nil
			}
		}
	}
	return nil, nil
}

// fakeMemberships is the in-test MembershipReader. Active is the set
// of memberships that pass the existence probe.
type fakeMemberships struct {
	Active map[string]struct{}
}

func (f *fakeMemberships) ExistsActiveInTenant(_ context.Context, _ tenant.ID, membershipID string) (bool, error) {
	_, ok := f.Active[membershipID]
	return ok, nil
}

func newFakeMemberships(ids ...string) *fakeMemberships {
	m := &fakeMemberships{Active: map[string]struct{}{}}
	for _, id := range ids {
		m.Active[id] = struct{}{}
	}
	return m
}
