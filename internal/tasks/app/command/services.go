package command

import (
	"context"

	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// HierarchyReader exposes the BRD §6.7 "manager sees their team"
// visibility traversal. The implementation walks the membership
// reports-to tree (and / or the role-hierarchy edges) to produce the
// set of memberships that `membershipID` may act on behalf of —
// transitively, including indirect reports.
//
// Returns a slice that ALWAYS INCLUDES membershipID itself when the
// membership exists in the tenant. Returns ([], nil) when the
// membership is unknown to the tenant.
//
// Per ADR 0047 boundary discipline: the Tasks app/ layer NEVER imports
// internal/identity/adapters/. The interface is declared here (next to
// the handler that consumes it); the composition root in cmd/api/main.go
// binds an identity-adapter-backed concrete impl.
type HierarchyReader interface {
	ListSubordinateMembershipIDs(ctx context.Context, tenantID tenant.ID, membershipID string) ([]string, error)
}

// MembershipReader is the lightweight "does this membership exist +
// active in this tenant" probe used by Reassign + CreateWorkItem.
type MembershipReader interface {
	ExistsActiveInTenant(ctx context.Context, tenantID tenant.ID, membershipID string) (bool, error)
}
