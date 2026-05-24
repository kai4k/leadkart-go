package command

import (
	"context"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/platform/domain/leadform"
	"github.com/leadkart/leadkart-go/internal/platform/domain/unverifiedcontact"
)

// CreateUnverifiedContactCommand carries the validated input for the
// Lead Agent "register a new raw contact" use case.
type CreateUnverifiedContactCommand struct {
	Form        leadform.Form
	CreatedBy   unverifiedcontact.MembershipID
}

// CreateUnverifiedContactResult holds the new contact's ID.
type CreateUnverifiedContactResult struct {
	ContactID unverifiedcontact.ID
}

// CreateUnverifiedContactHandler orchestrates the single-aggregate
// create. The repository's Add method joins the surrounding UoW tx (if
// any) or opens its own — both shapes drain the CreatedEvent to the
// outbox in the same tx per ADR 0008.
type CreateUnverifiedContactHandler struct {
	contacts     unverifiedcontact.Repository
	now          func() time.Time
	newContactID func() unverifiedcontact.ID
}

// NewCreateUnverifiedContactHandler wires the handler.
//
// newContactID is the aggregate-ID factory per the
// `TestArch_HandlersInjectIDFactory` discipline: random sources are
// HIDDEN INPUTS that violate Pure Domain (TDL Wild Workouts canon +
// Khorikov §8). Production passes
// `func() unverifiedcontact.ID { return unverifiedcontact.ID(ids.NewV7().String()) }`;
// tests inject a deterministic counter so the minted ID is pinnable.
func NewCreateUnverifiedContactHandler(
	contacts unverifiedcontact.Repository,
	now func() time.Time,
	newContactID func() unverifiedcontact.ID,
) CreateUnverifiedContactHandler {
	if newContactID == nil {
		panic("command: NewCreateUnverifiedContactHandler newContactID required")
	}
	if now == nil {
		now = time.Now
	}
	return CreateUnverifiedContactHandler{contacts: contacts, now: now, newContactID: newContactID}
}

// Handle constructs + persists the new UnverifiedContact.
func (h CreateUnverifiedContactHandler) Handle(
	ctx context.Context,
	cmd CreateUnverifiedContactCommand,
) (CreateUnverifiedContactResult, error) {
	id := h.newContactID()
	c, err := unverifiedcontact.New(id, cmd.Form, cmd.CreatedBy, h.now())
	if err != nil {
		return CreateUnverifiedContactResult{}, fmt.Errorf("create unverified contact: %w", err)
	}
	if err := h.contacts.Add(ctx, c); err != nil {
		return CreateUnverifiedContactResult{}, fmt.Errorf("create unverified contact: persist: %w", err)
	}
	return CreateUnverifiedContactResult{ContactID: id}, nil
}
