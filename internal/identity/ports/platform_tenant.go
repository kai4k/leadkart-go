package ports

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"

	"github.com/leadkart/leadkart-go/internal/common/slug"
	"github.com/leadkart/leadkart-go/internal/identity/app"
	"github.com/leadkart/leadkart-go/internal/identity/app/query"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/identity/ports/authn"
)

// NewPlatformTenantResolver adapts the GetTenantBySlug query to authn.PlatformTenantFunc: the id of the
// tenant whose slug is "platform", read from the database. The platform tenant's id never changes, so a
// found id is cached for the life of the process; a miss or an error is not cached (the tenant may be
// created after start-up) and answers "" so callers fall back to what the token proves.
func NewPlatformTenantResolver(log *slog.Logger, a app.Application) authn.PlatformTenantFunc {
	var cached atomic.Pointer[tenant.ID]
	return func(ctx context.Context) tenant.ID {
		if id := cached.Load(); id != nil {
			return *id
		}
		s, err := slug.New(tenant.PlatformSlug)
		if err != nil {
			return ""
		}
		v, err := a.Queries.GetTenantBySlug.Handle(ctx, query.GetTenantBySlugQuery{Slug: s})
		if err != nil {
			// "Not found" is the normal state before the platform tenant is registered; anything else
			// is a lookup failure worth seeing (the actor degrades to the pre-G2p classification).
			if !errors.Is(err, tenant.ErrNotFound) {
				log.WarnContext(ctx, "platform tenant lookup failed", "err", err)
			}
			return ""
		}
		if v.ID == "" {
			return ""
		}
		id := tenant.ID(v.ID)
		cached.Store(&id)
		return id
	}
}
