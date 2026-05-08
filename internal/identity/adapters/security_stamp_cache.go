package adapters

import (
	"context"
	"fmt"

	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/platform/cache"
)

// PersonStampReader is the consumer-side interface
// [SecurityStampCache] needs from the PersonRepository. Declared here
// (Cheney "accept interfaces") so tests can substitute in-memory
// fakes without spinning testcontainers Postgres for cache-shape
// assertions. *PersonRepository implicitly satisfies this contract.
type PersonStampReader interface {
	GetByID(ctx context.Context, id person.ID) (*person.Person, error)
}

// SecurityStampCache is the typed cache facade for the
// (PersonID → SecurityStamp) hot path consulted on every authenticated
// request via the JWT TokenValidated middleware. Per
// `coding-standards.md` "Cache facade per concern" + `audit-checklist.md
// §12b` (ported as Go-side doctrine): every cached read path is
// wrapped in a typed facade. Raw `*cache.HybridCache` injection in
// handlers / repositories / queries is an audit finding.
//
// TTL: 30 seconds per [cache.SecurityStampTTL] — Auth0 / Okta session-
// validation refresh window. Defense-in-depth: stale tokens fail
// validation within ~30s of password rotation even if the async
// cascade subscriber that explicitly invalidates the key hasn't run
// yet (forwarder-lag window).
type SecurityStampCache struct {
	facade *cache.Facade[person.ID, string]
}

// NewSecurityStampCache wires the facade against the supplied
// HybridCache + read-through person reader. The reader is consulted
// only on cache miss (singleflight-coalesced when concurrent misses
// race).
//
// L2-only ([cache.WithOmitL1]): SecurityStamp lookups are per-Person
// with traffic fanned out across the keyspace — the L1 in-process
// hit ratio is too low to earn ristretto's complexity. More
// importantly, the L1+L2 pattern has an inherent eventual-consistency
// race on Invalidate (a concurrent Get's L2.Get can race
// Invalidate's L2.Del + then refill L1 from L2 with the stale value
// before the Del completes; the L1 entry then persists for up to
// the L1 TTL, defeating the point of explicit invalidation). For a
// security-bearing facade where revocation must propagate within
// the next request, single-tier L2 is the canonical shape (Stripe /
// GitHub PAT / banking session-validation patterns). Per-request
// cost: one Redis roundtrip (~0.5-1ms within the same DC).
//
// HybridCache (L1+L2) remains the right shape for high-cache-hit-
// ratio reference data (future TenantSettings / Pincode / etc.).
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
		cache.WithTTL(cache.SecurityStampTTL),
		cache.WithOmitL1(),
	)
	return &SecurityStampCache{facade: facade}
}

// securityStampKey produces the canonical Redis key for the cached
// stamp. Format namespaces under `identity:` so a future module's
// cache (e.g. `crm:`) cannot collide.
func securityStampKey(id person.ID) string {
	return fmt.Sprintf("identity:security_stamp:person=%s", id)
}

// Get returns the current SecurityStamp string for the supplied
// PersonID, populating the cache from the read-through factory on
// miss. Repository errors propagate; cache transport errors fall
// through to the next layer (cache outage doesn't fail the request).
func (c *SecurityStampCache) Get(ctx context.Context, id person.ID) (string, error) {
	return c.facade.Get(ctx, id)
}

// Invalidate evicts the cached stamp for the supplied PersonID.
// Called by Person-level cascade subscribers
// (PasswordChanged / EmailChanged / Anonymised / GloballySuspended /
// GlobalSuspensionLifted) to close the eventual-consistency window
// faster than the 30-second TTL would.
//
// Per the EDA doctrine (`feedback_eda_ddd_default_thinking.md`): the
// invalidation is the SECOND half of the cascade — the first half
// being the family-revocation subscriber. Both are wired against the
// same integration events.
func (c *SecurityStampCache) Invalidate(ctx context.Context, id person.ID) error {
	return c.facade.Invalidate(ctx, id)
}

// SecurityStampValidator wraps the cache + claim-vs-stored comparison.
// Consumed by the authn middleware via the
// [authn.StampValidator] consumer-side interface.
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

// IsFresh reports whether the supplied claim stamp matches the
// currently-stored stamp for the given Person. Returns (false, nil)
// for a stale token, (true, nil) for fresh, or (_, err) for repository
// / cache transport failures (caller should treat as 401 either way
// per security.md "fail closed on auth").
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
