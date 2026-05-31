package adapters

import (
	"context"
	"fmt"

	"github.com/leadkart/leadkart-go/internal/common/cache"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
)

// PersonStampReader is the interface [SecurityStampCache] requires from
// the PersonRepository. Declared here so tests can inject in-memory fakes.
type PersonStampReader interface {
	GetByID(ctx context.Context, id person.ID) (*person.Person, error)
}

// SecurityStampCache is the typed cache facade for the
// PersonID → SecurityStamp hot path (every authenticated request).
// TTL: 30 s ([cache.SecurityStampTTL]) — defense-in-depth against stale
// tokens when the cascade subscriber hasn't invalidated the key yet.
type SecurityStampCache struct {
	facade *cache.Facade[person.ID, string]
}

// NewSecurityStampCache wires the facade. Reader is consulted only on miss
// (singleflight-coalesced). L2-only ([cache.WithOmitL1]): the per-Person
// keyspace has too low an L1 hit-ratio to justify ristretto's complexity,
// and L1+L2 has an Invalidate race (L1 can refill from stale L2 before Del
// completes). Single-tier L2 is the canonical shape for security-bearing
// caches (Stripe / GitHub PAT / banking).
func NewSecurityStampCache(hybrid *cache.HybridCache, persons PersonStampReader) *SecurityStampCache {
	if hybrid == nil {
		panic("adapters: NewSecurityStampCache hybrid cache required")
	}
	if persons == nil {
		panic("adapters: NewSecurityStampCache persons reader required")
	}
	facade := cache.NewFacade[person.ID, string](
		hybrid,
		"identity.security_stamp",
		securityStampKey,
		func(ctx context.Context, id person.ID) (string, error) {
			p, err := persons.GetByID(ctx, id)
			if err != nil {
				return "", err
			}
			return p.SecurityStamp().String(), nil
		},
		cache.WithTTL(cache.SecurityStampTTL()),
		cache.WithOmitL1(),
	)
	return &SecurityStampCache{facade: facade}
}

// securityStampKey produces the Redis key, namespaced under `identity:`.
func securityStampKey(id person.ID) string {
	return fmt.Sprintf("identity:security_stamp:person=%s", id)
}

// Get returns the current SecurityStamp for the PersonID, populating from
// the read-through factory on miss.
func (c *SecurityStampCache) Get(ctx context.Context, id person.ID) (string, error) {
	return c.facade.Get(ctx, id)
}

// Invalidate evicts the cached stamp. Called by cascade subscribers
// (PasswordChanged, EmailChanged, Anonymised, GloballySuspended,
// GlobalSuspensionLifted) to close the eventual-consistency window.
func (c *SecurityStampCache) Invalidate(ctx context.Context, id person.ID) error {
	return c.facade.Invalidate(ctx, id)
}

// SecurityStampValidator wraps the cache for claim-vs-stored comparison.
// Consumed by the authn middleware via [authn.StampValidator].
type SecurityStampValidator struct {
	cache *SecurityStampCache
}

// NewSecurityStampValidator wires the validator.
func NewSecurityStampValidator(c *SecurityStampCache) *SecurityStampValidator {
	if c == nil {
		panic("adapters: NewSecurityStampValidator cache required")
	}
	return &SecurityStampValidator{cache: c}
}

// IsFresh reports whether claimStamp matches the stored stamp.
// Returns (false, nil) for stale, (true, nil) for fresh, or (_, err)
// for lookup failures (caller treats as 401 per "fail closed on auth").
func (v *SecurityStampValidator) IsFresh(
	ctx context.Context,
	personID string,
	claimStamp string,
) (bool, error) {
	if personID == "" {
		return false, nil
	}
	stored, err := v.cache.Get(ctx, person.ID(personID))
	if err != nil {
		return false, fmt.Errorf("security stamp validate: %w", err)
	}
	return stored == claimStamp, nil
}
