package crmleadtest

import "context"

// InlineTenantUoW satisfies pg.TenantUnitOfWork for unit tests: it runs the
// closure inline, with no transaction (the in-memory fakes have none).
type InlineTenantUoW struct{}

// WithinTenantTx runs fn on ctx unchanged.
func (InlineTenantUoW) WithinTenantTx(ctx context.Context, _ string, fn func(ctx context.Context) error) error {
	return fn(ctx)
}
