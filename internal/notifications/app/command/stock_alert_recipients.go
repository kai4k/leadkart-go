package command

import (
	"context"

	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// StockAlertRecipientResolver names the people who act on stock alerts in a
// company: the memberships that manage stock. Notifications cannot read the
// identity schema, so the composition root binds this to an Identity read (the
// tasks hierarchy bridge precedent).
type StockAlertRecipientResolver interface {
	StockAlertRecipients(ctx context.Context, tenantID tenant.ID) ([]membership.ID, error)
}
