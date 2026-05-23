package command

import (
	"fmt"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/platform/domain/platformlead"
	"github.com/leadkart/leadkart-go/internal/platform/domain/unverifiedcontact"
)

// mustParseUUID converts a string UUID into [uuid.UUID]. Panics on
// invalid input — domain IDs are always valid UUIDv7 strings at this
// boundary (the aggregate factories generate them via ids.NewV7).
func mustParseUUID(s string) uuid.UUID {
	u, err := uuid.Parse(s)
	if err != nil {
		panic(fmt.Sprintf("platform.app.command: malformed UUID %q: %v", s, err))
	}
	return u
}

// leadIDUUID converts a domain [platformlead.ID] to a [uuid.UUID] for
// the wire payload field on integration events.
func leadIDUUID(id platformlead.ID) uuid.UUID { return mustParseUUID(id.String()) }

// membershipUUID converts a domain [unverifiedcontact.MembershipID] to
// a [uuid.UUID] for the wire payload field on integration events.
func membershipUUID(id unverifiedcontact.MembershipID) uuid.UUID {
	return mustParseUUID(id.String())
}

// tenantUUID converts a domain [platformlead.TenantID] to a [uuid.UUID].
func tenantUUID(id platformlead.TenantID) uuid.UUID { return mustParseUUID(id.String()) }
