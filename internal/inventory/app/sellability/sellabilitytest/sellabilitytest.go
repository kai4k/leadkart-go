// Package sellabilitytest builds a [sellability.Resolver] over the default
// policy for tests.
package sellabilitytest

import (
	"context"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/commercepolicy"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/inventory/app/sellability"
)

// Policy is a [sellability.PolicyReader] that returns a fixed policy.
type Policy struct{ Value commercepolicy.Versioned }

// ReadPolicy returns the fixed policy.
func (p Policy) ReadPolicy(context.Context, tenant.ID) (commercepolicy.Versioned, error) {
	return p.Value, nil
}

// Resolver returns a resolver over the default policy (stop one month before
// the printed month, business day at the default offset) at the fixed instant.
func Resolver(now time.Time) sellability.Resolver {
	return ResolverWith(commercepolicy.Default(), now)
}

// ResolverWith returns a resolver over the given policy at the fixed instant.
func ResolverWith(p commercepolicy.Policy, now time.Time) sellability.Resolver {
	return sellability.NewResolver(Policy{Value: commercepolicy.Versioned{Version: 1, Policy: p}}, func() time.Time { return now })
}
