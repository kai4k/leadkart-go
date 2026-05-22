package role_test

import (
	"errors"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// freshRole mints a clean non-system-default role in the supplied tenant.
func freshRole(t *testing.T, tid tenant.ID, name string) *role.Role {
	t.Helper()
	r, err := role.New(
		role.ID(ids.NewV7().String()),
		tid,
		name,
		false, role.HierarchyLevelDefault, false,
	)
	if err != nil {
		t.Fatalf("role.New: %v", err)
	}
	// Drain the CreatedEvent so subsequent PullEvents reads only the
	// hierarchy mutations under test.
	_ = r.PullEvents()
	return r
}

func freshTenantID(t *testing.T) tenant.ID {
	t.Helper()
	return tenant.ID(ids.NewV7().String())
}

func TestRole_ChangeParent_DetectsDirectCycle(t *testing.T) {
	t.Parallel()
	tid := freshTenantID(t)
	r := freshRole(t, tid, "Sales")

	err := r.ChangeParent(r.ID(), func(role.ID) ([]role.ID, error) { return nil, nil })
	if !errors.Is(err, role.ErrHierarchyCycle) {
		t.Fatalf("self-parent: got %v want ErrHierarchyCycle", err)
	}
	if r.HasParent() {
		t.Fatalf("self-parent attempt mutated state: ParentRoleID=%q", r.ParentRoleID())
	}
}

func TestRole_ChangeParent_DetectsIndirectCycle(t *testing.T) {
	t.Parallel()
	tid := freshTenantID(t)
	r := freshRole(t, tid, "Sales")
	parentCandidate := role.ID(ids.NewV7().String())

	// ancestorLookup says the proposed parent's ancestor chain INCLUDES r.id
	// — that's the indirect cycle: r → parentCandidate → ... → r.
	err := r.ChangeParent(parentCandidate, func(id role.ID) ([]role.ID, error) {
		if id == parentCandidate {
			return []role.ID{role.ID(ids.NewV7().String()), r.ID()}, nil
		}
		return nil, nil
	})
	if !errors.Is(err, role.ErrHierarchyCycle) {
		t.Fatalf("indirect cycle: got %v want ErrHierarchyCycle", err)
	}
	if r.HasParent() {
		t.Fatalf("indirect cycle mutated state")
	}
}

func TestRole_ChangeParent_AcceptsValidParent(t *testing.T) {
	t.Parallel()
	tid := freshTenantID(t)
	r := freshRole(t, tid, "Junior")
	parent := role.ID(ids.NewV7().String())

	err := r.ChangeParent(parent, func(role.ID) ([]role.ID, error) { return nil, nil })
	if err != nil {
		t.Fatalf("ChangeParent: %v", err)
	}
	if r.ParentRoleID() != parent {
		t.Fatalf("ParentRoleID: got %q want %q", r.ParentRoleID(), parent)
	}
	if !r.HasParent() {
		t.Fatalf("HasParent: got false want true")
	}
}

func TestRole_ChangeParent_AcceptsClearingParent(t *testing.T) {
	t.Parallel()
	tid := freshTenantID(t)
	r := freshRole(t, tid, "Manager")
	parent := role.ID(ids.NewV7().String())
	if err := r.ChangeParent(parent, func(role.ID) ([]role.ID, error) { return nil, nil }); err != nil {
		t.Fatalf("seed parent: %v", err)
	}
	_ = r.PullEvents()

	// Clearing the parent passes ancestorLookup=nil (allowed for the
	// zero-ID branch).
	if err := r.ChangeParent(role.ID(""), nil); err != nil {
		t.Fatalf("clear parent: %v", err)
	}
	if r.HasParent() {
		t.Fatalf("HasParent after clear: got true want false")
	}
}

func TestRole_ChangeParent_RecordsEvent(t *testing.T) {
	t.Parallel()
	tid := freshTenantID(t)
	r := freshRole(t, tid, "Junior")
	parent := role.ID(ids.NewV7().String())

	if err := r.ChangeParent(parent, func(role.ID) ([]role.ID, error) { return nil, nil }); err != nil {
		t.Fatalf("ChangeParent: %v", err)
	}
	evs := r.PullEvents()
	if len(evs) != 1 {
		t.Fatalf("events: got %d want 1", len(evs))
	}
	pc, ok := evs[0].(role.ParentChangedEvent)
	if !ok {
		t.Fatalf("event type: got %T want ParentChangedEvent", evs[0])
	}
	if pc.NewParentID != parent {
		t.Fatalf("NewParentID: got %q want %q", pc.NewParentID, parent)
	}
	if !pc.OldParentID.IsZero() {
		t.Fatalf("OldParentID: got %q want zero", pc.OldParentID)
	}
}

func TestRole_ChangeParent_IdempotentSameParent(t *testing.T) {
	t.Parallel()
	tid := freshTenantID(t)
	r := freshRole(t, tid, "Junior")
	parent := role.ID(ids.NewV7().String())
	if err := r.ChangeParent(parent, func(role.ID) ([]role.ID, error) { return nil, nil }); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_ = r.PullEvents()

	// Setting to the same parent emits no event.
	if err := r.ChangeParent(parent, func(role.ID) ([]role.ID, error) { return nil, nil }); err != nil {
		t.Fatalf("idempotent set: %v", err)
	}
	if got := r.PullEvents(); len(got) != 0 {
		t.Fatalf("idempotent events: got %d want 0", len(got))
	}
}

func TestRole_ChangeParent_RejectsDeleted(t *testing.T) {
	t.Parallel()
	tid := freshTenantID(t)
	r := freshRole(t, tid, "Junior")
	if err := r.Delete("admin@test"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	err := r.ChangeParent(role.ID(ids.NewV7().String()),
		func(role.ID) ([]role.ID, error) { return nil, nil })
	if !errors.Is(err, role.ErrDeleted) {
		t.Fatalf("deleted: got %v want ErrDeleted", err)
	}
}

func TestRole_ChangeParent_RequiresLookupForNonClear(t *testing.T) {
	t.Parallel()
	tid := freshTenantID(t)
	r := freshRole(t, tid, "Junior")
	err := r.ChangeParent(role.ID(ids.NewV7().String()), nil)
	if !errors.Is(err, role.ErrInvalid) {
		t.Fatalf("nil lookup: got %v want ErrInvalid", err)
	}
}
