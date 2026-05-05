// Package tenancy carries the request-scoped tenant identifier through
// context.Context. Read by HTTP middleware, propagated to repositories,
// consumed by pgxpool's AfterAcquire callback to issue
// `SET LOCAL app.tenant_id = $1` per transaction (ADR 0006).
//
// Per ADR 0033: NO `ICurrentTenant` interface. The package functions
// `WithID` / `FromContext` / `MustFromContext` ARE the contract.
package tenancy

import "context"

// ID is a tenant identifier — UUIDv7 wrapped as a string for cheap context
// passing and direct Postgres binding.
//
// Stored as text in context.Context (zero-alloc copy) and bound as `uuid`
// via pgx's automatic conversion at SQL layer. The wrapper type prevents
// accidental misuse with bare `string` IDs from other domains.
type ID string

// IsZero reports whether the ID is the empty zero value.
func (i ID) IsZero() bool { return i == "" }

// String returns the underlying UUID string.
func (i ID) String() string { return string(i) }

// ctxKey is unexported per Go canon — using an unexported type as the
// key prevents collision with other packages that might use a string
// like "tenant" as their own context key.
type ctxKey struct{ name string }

// tenantKey is the singleton context key for tenant.ID values.
var tenantKey = ctxKey{name: "tenant"} //nolint:gochecknoglobals // canonical Go ctx-key pattern

// WithID returns a new context carrying the supplied tenant ID.
//
// HTTP middleware calls this after decoding the JWT; downstream code
// reads via FromContext. Watermill subscribers do the same after parsing
// the event metadata header.
func WithID(ctx context.Context, id ID) context.Context {
	return context.WithValue(ctx, tenantKey, id)
}

// FromContext returns the tenant ID and true if present, or the zero ID
// and false if absent. Use this in code that must tolerate the absence
// of a tenant (e.g. cross-tenant operator paths).
func FromContext(ctx context.Context) (ID, bool) {
	id, ok := ctx.Value(tenantKey).(ID)
	return id, ok
}

// MustFromContext returns the tenant ID, panicking if absent or zero.
// Use this in repository code where the absence of a tenant is a
// programmer error (the request should have been rejected upstream).
//
// NEVER call this in HTTP handlers — handlers should return an error
// for missing tenant, never panic.
func MustFromContext(ctx context.Context) ID {
	id, ok := FromContext(ctx)
	if !ok || id.IsZero() {
		panic("tenancy: tenant required in context")
	}
	return id
}
