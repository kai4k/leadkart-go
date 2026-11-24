package integrationevents

import (
	"time"

	"github.com/google/uuid"
)

// ProductCreatedV1 — a Product was created in the given tenant.
// Consumed by audit + search-index subscribers + frontend list cache.
type ProductCreatedV1 struct {
	ProductID             uuid.UUID `json:"product_id"`
	TenantIDClaim         uuid.UUID `json:"tenant_id"`
	SKU                   string    `json:"sku"`
	Name                  string    `json:"name"`
	CreatedAt             time.Time `json:"created_at"`
	CreatedByMembershipID uuid.UUID `json:"created_by_membership_id"`
	OccurredAtUTC         time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (ProductCreatedV1) Topic() string { return "inventory.product_created.v1" }

// OccurredAt returns the domain timestamp.
func (e ProductCreatedV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// TenantID satisfies [TenantScoped].
func (e ProductCreatedV1) TenantID() uuid.UUID { return e.TenantIDClaim }

// ProductUpdatedV1 — one or more Product fields changed. ChangedFields
// lists the wire-stable snake_case names (sorted alphabetically) so
// downstream consumers can decide whether to refetch.
type ProductUpdatedV1 struct {
	ProductID             uuid.UUID `json:"product_id"`
	TenantIDClaim         uuid.UUID `json:"tenant_id"`
	ChangedFields         []string  `json:"changed_fields"`
	UpdatedAt             time.Time `json:"updated_at"`
	UpdatedByMembershipID uuid.UUID `json:"updated_by_membership_id"`
	OccurredAtUTC         time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (ProductUpdatedV1) Topic() string { return "inventory.product_updated.v1" }

// OccurredAt returns the domain timestamp.
func (e ProductUpdatedV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// TenantID satisfies [TenantScoped].
func (e ProductUpdatedV1) TenantID() uuid.UUID { return e.TenantIDClaim }

// ProductDeactivatedV1 — a Product's `is_active` flag transitioned to
// false. The Product remains visible to admins + report queries +
// historical-order lookups; it is no longer offered on new-order forms
// or product pickers. Products are never deleted (ADR 0073); see ProductReactivatedV1.
type ProductDeactivatedV1 struct {
	ProductID                 uuid.UUID `json:"product_id"`
	TenantIDClaim             uuid.UUID `json:"tenant_id"`
	DeactivatedAt             time.Time `json:"deactivated_at"`
	DeactivatedByMembershipID uuid.UUID `json:"deactivated_by_membership_id"`
	OccurredAtUTC             time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (ProductDeactivatedV1) Topic() string { return "inventory.product_deactivated.v1" }

// OccurredAt returns the domain timestamp.
func (e ProductDeactivatedV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// TenantID satisfies [TenantScoped].
func (e ProductDeactivatedV1) TenantID() uuid.UUID { return e.TenantIDClaim }

// ProductReactivatedV1 — an inactive Product became active again. Consumers may offer it again
// on pickers. Products are never deleted (ADR 0073): the lifecycle is active <-> inactive.
type ProductReactivatedV1 struct {
	ProductID                 uuid.UUID `json:"product_id"`
	TenantIDClaim             uuid.UUID `json:"tenant_id"`
	ReactivatedAt             time.Time `json:"reactivated_at"`
	ReactivatedByMembershipID uuid.UUID `json:"reactivated_by_membership_id"`
	OccurredAtUTC             time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (ProductReactivatedV1) Topic() string { return "inventory.product_reactivated.v1" }

// OccurredAt returns the domain timestamp.
func (e ProductReactivatedV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// TenantID satisfies [TenantScoped].
func (e ProductReactivatedV1) TenantID() uuid.UUID { return e.TenantIDClaim }

// Compile-time + runtime registration.
var (
	_ TenantScoped = ProductCreatedV1{}
	_ TenantScoped = ProductUpdatedV1{}
	_ TenantScoped = ProductDeactivatedV1{}
	_ TenantScoped = ProductReactivatedV1{}

	_ = register(ProductCreatedV1{})
	_ = register(ProductUpdatedV1{})
	_ = register(ProductDeactivatedV1{})
	_ = register(ProductReactivatedV1{})
)
