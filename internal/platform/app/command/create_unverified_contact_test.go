package command_test

import (
	"context"
	"errors"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/platform/app/command"
	"github.com/leadkart/leadkart-go/internal/platform/domain/unverifiedcontact"
	"github.com/leadkart/leadkart-go/internal/platform/platformtest"
)

// failingContactRepo's Add always errors — to assert the handler wraps
// (rather than swallows) a repository failure. Add is the only method the
// happy path touches; the others panic if hit.
type failingContactRepo struct{ err error }

func (r failingContactRepo) Add(context.Context, *unverifiedcontact.UnverifiedContact) error {
	return r.err
}

func (failingContactRepo) UpdateByID(context.Context, unverifiedcontact.ID, func(*unverifiedcontact.UnverifiedContact) (bool, error)) error {
	panic("not used")
}

func (failingContactRepo) GetByID(context.Context, unverifiedcontact.ID) (*unverifiedcontact.UnverifiedContact, error) {
	panic("not used")
}

// TestCreateUnverifiedContact_PersistsAndDrainsCreatedEvent: contact
// lands in the repo and the CreatedEvent drains off the aggregate (the
// outbox writer maps it to UnverifiedContactCreatedV1 — adapter tests).
// C2 — review-pass.
func TestCreateUnverifiedContact_PersistsAndDrainsCreatedEvent(t *testing.T) {
	t.Parallel()

	contacts := platformtest.NewFakeUnverifiedContactRepository()
	h := command.NewCreateUnverifiedContactHandler(contacts, nowFunc, func() unverifiedcontact.ID { return unverifiedcontact.ID(ids.NewV7().String()) })

	agentID := unverifiedcontact.MembershipID(ids.NewV7().String())
	out, err := h.Handle(t.Context(), command.CreateUnverifiedContactCommand{
		Form:      sampleForm(t),
		CreatedBy: agentID,
	})
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if out.ContactID == "" {
		t.Fatal("expected non-empty ContactID")
	}

	// Aggregate landed in the fake.
	got, err := contacts.GetByID(t.Context(), out.ContactID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.State() != unverifiedcontact.StateNew {
		t.Errorf("state=%q want new", got.State())
	}
	if got.CreatedByMembershipID() != agentID {
		t.Errorf("CreatedByMembershipID=%q want %q", got.CreatedByMembershipID(), agentID)
	}
	if got.Form().MobileE164() != "+919876543210" {
		t.Errorf("form not round-tripped: %q", got.Form().MobileE164())
	}

	// Add drains the CreatedEvent; the mapper turns it into
	// UnverifiedContactCreatedV1 (integrationevents arch tests).
	if len(contacts.DrainedEvents) != 1 {
		t.Fatalf("expected 1 drained event, got %d", len(contacts.DrainedEvents))
	}
	if _, ok := contacts.DrainedEvents[0].(unverifiedcontact.CreatedEvent); !ok {
		t.Errorf("expected CreatedEvent, got %T", contacts.DrainedEvents[0])
	}
}

// TestCreateUnverifiedContact_RepositoryErrorBubbles: the handler must wrap,
// not swallow, a repository failure.
func TestCreateUnverifiedContact_RepositoryErrorBubbles(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("db down")
	h := command.NewCreateUnverifiedContactHandler(failingContactRepo{err: wantErr}, nowFunc, func() unverifiedcontact.ID { return unverifiedcontact.ID(ids.NewV7().String()) })

	_, err := h.Handle(t.Context(), command.CreateUnverifiedContactCommand{
		Form:      sampleForm(t),
		CreatedBy: unverifiedcontact.MembershipID(ids.NewV7().String()),
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want wrapped %v", err, wantErr)
	}
}
