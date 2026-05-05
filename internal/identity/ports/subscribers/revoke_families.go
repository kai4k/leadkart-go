package subscribers

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/ThreeDotsLabs/watermill/message"

	"github.com/leadkart/leadkart-go/internal/identity/adapters"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/refreshtoken"
	"github.com/leadkart/leadkart-go/internal/identity/integrationevents"
	"github.com/leadkart/leadkart-go/internal/platform/messaging"
)

// RevokeFamiliesOnSecurityChange is the choreographed reaction to
// Person-level security mutations: any change that rotates the
// SecurityStamp (password change, anonymisation, future global
// suspend) MUST revoke every refresh-token family for that Person
// across tenants.
//
// Per `security.md` "SecurityStamp rotation triggers": revoke families
// on password/email/role change + logout-all + admin password reset +
// anonymisation. v0.2 wires the password-change + anonymise reactions;
// future steps add email-change + role-change.
//
// The handler is idempotent by domain construction: Family.Revoke is
// no-op on already-revoked families. Re-delivery of the same event
// (despite the inbox dedup) is safe.
type RevokeFamiliesOnSecurityChange struct {
	families *adapters.RefreshTokenFamilyRepository
	log      *slog.Logger
}

// NewRevokeFamiliesOnSecurityChange wires the subscriber.
func NewRevokeFamiliesOnSecurityChange(
	families *adapters.RefreshTokenFamilyRepository,
	log *slog.Logger,
) *RevokeFamiliesOnSecurityChange {
	if log == nil {
		log = slog.Default()
	}
	return &RevokeFamiliesOnSecurityChange{families: families, log: log}
}

// HandlePasswordChanged is the [messaging.SubscriberHandler] for the
// `identity.person_password_changed.v1` topic. Other events on the
// shared topic short-circuit silently.
func (h *RevokeFamiliesOnSecurityChange) HandlePasswordChanged(
	ctx context.Context, _ string, msg *message.Message,
) error {
	expected := integrationevents.PersonPasswordChangedV1{}.Topic()
	if msg.Metadata.Get(messaging.HeaderEventType) != expected {
		return nil
	}
	var evt integrationevents.PersonPasswordChangedV1
	if err := json.Unmarshal(msg.Payload, &evt); err != nil {
		return fmt.Errorf("subscribers: decode %s: %w", expected, err)
	}
	return h.revokeAll(ctx, evt.PersonID.String(), "password_changed")
}

// HandleAnonymised is the handler for `identity.person_anonymised.v1`.
func (h *RevokeFamiliesOnSecurityChange) HandleAnonymised(
	ctx context.Context, _ string, msg *message.Message,
) error {
	expected := integrationevents.PersonAnonymisedV1{}.Topic()
	if msg.Metadata.Get(messaging.HeaderEventType) != expected {
		return nil
	}
	var evt integrationevents.PersonAnonymisedV1
	if err := json.Unmarshal(msg.Payload, &evt); err != nil {
		return fmt.Errorf("subscribers: decode %s: %w", expected, err)
	}
	return h.revokeAll(ctx, evt.PersonID.String(), "person_anonymised")
}

// revokeAll lists every NON-revoked family for the Person + revokes
// each via the UpdateByID UpdateFn pattern. Each revoke emits a
// [refreshtoken.RevokedEvent] which the repo drains to outbox as a
// [integrationevents.RefreshTokenFamilyRevokedV1] — downstream SIEM
// + audit subscribers see the cascade.
func (h *RevokeFamiliesOnSecurityChange) revokeAll(
	ctx context.Context, personID string, reason string,
) error {
	pid := person.ID(personID)
	actives, err := h.families.ListActiveForPerson(ctx, pid)
	if err != nil {
		return fmt.Errorf("subscribers: list families for %s: %w", personID, err)
	}
	if len(actives) == 0 {
		return nil
	}
	for _, f := range actives {
		err := h.families.UpdateByID(ctx, f.ID(), func(family *refreshtoken.Family) (bool, error) {
			if err := family.Revoke(reason); err != nil {
				return false, err
			}
			return true, nil
		})
		if err != nil {
			h.log.ErrorContext(ctx, "revoke family failed",
				"person_id", personID, "family_id", f.ID(), "reason", reason, "err", err)
			return fmt.Errorf("revoke family %s: %w", f.ID(), err)
		}
	}
	h.log.InfoContext(ctx, "revoked families on security change",
		"person_id", personID, "reason", reason, "count", len(actives))
	return nil
}
