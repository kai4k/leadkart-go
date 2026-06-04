package query

import (
	"context"

	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// HierarchyReader is the query-local copy of the same interface
// declared in app/command/services.go. Per TDL canon: each handler
// declares its own outbound interfaces ("accept interfaces, return
// structs"), so command + query each have their own — the composition
// root binds the same concrete identity-adapter to both.
//
// Returns a slice that ALWAYS INCLUDES membershipID itself when the
// membership exists in the tenant.
type HierarchyReader interface {
	ListSubordinateMembershipIDs(ctx context.Context, tenantID tenant.ID, membershipID string) ([]string, error)
}
