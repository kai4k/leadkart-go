package command

import (
	"context"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/platform/domain/leadform"
	"github.com/leadkart/leadkart-go/internal/platform/domain/unverifiedcontact"
)

// CreateUnverifiedContactCommand is the Lead Agent "register a new raw
// contact" input.
type CreateUnverifiedContactCommand struct {
	Form      leadform.Form
	CreatedBy unverifiedcontact.MembershipID
}

// CreateUnverifiedContactResult holds the new contact's ID.
type CreateUnverifiedContactResult struct {
	ContactID unverifiedcontact.ID
}

// CreateUnverifiedContactHandler creates a single contact aggregate.
// Add drains the CreatedEvent to the outbox in the same tx (ADR 0008).
type CreateUnverifiedContactHandler struct {
	contacts     unverifiedcontact.Repository
	now          func() time.Time
	newContactID func() unverifiedcontact.ID
}

// NewCreateUnverifiedContactHandler wires the handler.
//
// newContactID is injected per TestArch_HandlersInjectIDFactory: random
// sources are hidden inputs that violate Pure Domain (TDL canon +
// Khorikov §8). Tests inject a deterministic counter for pinnable IDs.
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
