package role

import (
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// Event is the marker interface for Role domain events. Sealed via
// the unexported isRoleEvent() method — only types in this package
// can satisfy the contract, preventing accidental external impls.
type Event interface {
	isRoleEvent()
}

// CreatedEvent fires when a new Role is constructed via [New].
// The Role's full identity + flags + hierarchy travel on the event so
// downstream subscribers (cache invalidation, audit log, integration-
// event mapper) don't need to re-load the aggregate.
type CreatedEvent struct {
	RoleID          ID
	TenantID        tenant.ID
	Name            string
	IsSystemDefault bool
	HierarchyLevel  int
	IsSuperAdmin    bool
	At              time.Time
}

func (CreatedEvent) isRoleEvent() {}

// RenamedEvent fires when a non-system-default role's name changes.
// Carries both the old and new name so audit subscribers can render
// the diff without re-loading prior state.
type RenamedEvent struct {
	RoleID   ID
	TenantID tenant.ID
	OldName  string
	NewName  string
	At       time.Time
}

func (RenamedEvent) isRoleEvent() {}
