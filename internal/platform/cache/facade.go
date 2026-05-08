package cache

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/redis/go-redis/v9"
)

// Facade is the typed read-through cache facade per
// `coding-standards.md` "Cache facade per concern". Wraps one
// [HybridCache] + a resource-specific factory + key-shaping function
// + per-facade TTL. Domain code injects the typed facade interface
// (e.g. `ISecurityStampCache`) and never touches HybridCache directly.
//
// K is the lookup key (typed — `MembershipID`, `(PersonID, TenantID)`,
// etc.). V is the cached value type. Both are constrained `any`; the
// facade does not impose serialisation constraints since [Codec]
// handles encode/decode.
//
// Concurrency contract:
//
// After [Facade.Invalidate], [Facade.InvalidateMany], or [Facade.Set]
// returns, no subsequent [Facade.Get] will observe the value that was
// cached at the time of the mutation, even if a factory call that
// started before the mutation is still in flight. This is the
// "stale-write fence" guarantee that lets cascade subscribers
// invalidate fearlessly without worrying that an in-flight Get will
// race them and re-populate stale data.
//
// The fence is a per-facade generation counter ([Facade.gen]):
// every mutation bumps it; every factory invocation captures the
// generation at start and refuses to commit its result to L1+L2 if
// the generation has advanced. The factory result is still returned
// to the caller — it represents the source-of-truth at factory-call
// time, which the caller asked for; only the cache write is skipped.
// The next Get re-queries the source.
type Facade[K comparable, V any] struct {
	cache   *HybridCache
	keyer   func(K) string
	factory func(ctx context.Context, key K) (V, error)
	ttl     TTL
	name    string

	// gen monotonically increases on every Invalidate / InvalidateMany
	// / Set. Read by the Get-miss factory closure to decide whether
	// its result is still safe to commit to the cache. See the
	// "Concurrency contract" doc on the type.
	gen atomic.Uint64
}

// FacadeOption tunes a [Facade] at construction.
type FacadeOption func(opts *facadeOptions)

type facadeOptions struct {
	ttl TTL
}

// WithTTL overrides the per-facade TTL. Default is [DefaultTTL].
func WithTTL(ttl TTL) FacadeOption {
	return func(o *facadeOptions) { o.ttl = ttl }
}

// NewFacade wires a typed facade.
//
//   - name appears in logs + metric labels.
//   - keyer maps K → canonical Redis key (e.g.
//     "identity:security_stamp:tenant=abc:membership=def").
//   - factory is the source-of-truth fetch on miss (typically a
//     repository call). MUST be context-aware.
//
// The returned Facade is safe for concurrent use.
func NewFacade[K comparable, V any](
	cache *HybridCache,
	name string,
	keyer func(K) string,
	factory func(ctx context.Context, key K) (V, error),
	opts ...FacadeOption,
) *Facade[K, V] {
	o := facadeOptions{ttl: DefaultTTL}
	for _, opt := range opts {
		opt(&o)
	}
	return &Facade[K, V]{
		cache:   cache,
		keyer:   keyer,
		factory: factory,
		ttl:     o.ttl,
		name:    name,
	}
}

// Get returns the cached value for key.
//
// Lookup order:
//
//  1. L1 (ristretto) — bytes decoded into V via Codec.
//  2. L2 (Redis) — bytes decoded; on hit, L1 populated.
//  3. singleflight Do — coalesces concurrent misses; the chosen call
//     runs the factory, populates L2 + L1, returns the value.
//
// Errors from the factory propagate. L1/L2 transport errors fall
// through to the next layer (cache outage doesn't fail the request)
// but ARE logged at WARN.
func (f *Facade[K, V]) Get(ctx context.Context, key K) (V, error) {
	keyStr := f.keyer(key)

	// L1 hit?
	if raw, found := f.cache.L1.Get(keyStr); found {
		var v V
		if err := f.cache.Codec.Decode(raw, &v); err == nil {
			return v, nil
		}
		// Decode failure on L1 is unexpected — value was encoded by
		// us. Log + fall through to L2 + factory.
		f.cache.Logger.Warn("cache: L1 decode failed", "facade", f.name, "key", keyStr)
	}

	// L2 hit?
	switch raw, err := f.cache.L2.Get(ctx, keyStr).Bytes(); {
	case err == nil:
		var v V
		if decodeErr := f.cache.Codec.Decode(raw, &v); decodeErr != nil {
			f.cache.Logger.Warn("cache: L2 decode failed",
				"facade", f.name, "key", keyStr, "err", decodeErr)
			break
		}
		f.cache.L1.SetWithTTL(keyStr, raw, int64(len(raw)), f.ttl.L1)
		return v, nil
	case !errors.Is(err, redis.Nil):
		f.cache.Logger.Warn("cache: L2 read failed",
			"facade", f.name, "key", keyStr, "err", err)
	}

	// Miss → singleflight to coalesce concurrent factory invocations.
	val, err, _ := f.cache.Group.Do(f.name+":"+keyStr, func() (any, error) {
		// Stale-write fence: capture the generation BEFORE the source
		// read so any concurrent Invalidate/Set after this point causes
		// us to skip the cache write below. See the "Concurrency
		// contract" doc on Facade.
		genBefore := f.gen.Load()
		v, err := f.factory(ctx, key)
		if err != nil {
			return v, err
		}
		raw, encErr := f.cache.Codec.Encode(v)
		if encErr != nil {
			f.cache.Logger.Warn("cache: encode failed",
				"facade", f.name, "key", keyStr, "err", encErr)
			return v, nil
		}
		if f.gen.Load() != genBefore {
			// A mutation (Invalidate/InvalidateMany/Set) ran while we
			// were reading the source. Our factory result reflects the
			// pre-mutation source — committing it would re-poison the
			// cache the mutation just cleared. Return the value to the
			// caller (they asked for "what's the source-of-truth right
			// now?", and at the time we read it that WAS true) but skip
			// the L1/L2 write. Next Get will re-query.
			return v, nil
		}
		// L2 first (durable across L1 evictions); L1 populated even on
		// L2 set failure so a single Redis blip doesn't spam factory.
		if err := f.cache.L2.Set(ctx, keyStr, raw, f.ttl.L2).Err(); err != nil {
			f.cache.Logger.Warn("cache: L2 write failed",
				"facade", f.name, "key", keyStr, "err", err)
		}
		f.cache.L1.SetWithTTL(keyStr, raw, int64(len(raw)), f.ttl.L1)
		return v, nil
	})
	if err != nil {
		var zero V
		return zero, err
	}
	return val.(V), nil
}

// Invalidate evicts a single key from BOTH L1 and L2. Domain event
// subscribers call this after the source-of-truth row mutates.
//
// Three things must happen atomically from the caller's perspective:
//
//  1. Bump the generation BEFORE evicting, so any factory call already
//     in flight (whose genBefore was captured pre-bump) skips its
//     cache write. Without this, the factory could repopulate the
//     cache with stale data milliseconds after we cleared it.
//  2. Evict L1 — and Wait for ristretto's async Del buffer to drain
//     so the eviction is observable to the next L1.Get. Per ristretto
//     v2 Cache.Wait godoc: "Wait blocks until all buffered writes
//     have been applied".
//  3. Evict L2 — synchronous via go-redis, returns the redis error
//     if any. Caller may choose to retry.
//
// Step 1 is the load-bearing fence. Step 2 ensures observability.
// Together they make the contract "after Invalidate returns, the
// next Get does not see the value that was cached at the time of
// Invalidate" hold under concurrent Get + Invalidate.
//
// L1 eviction is best-effort (ristretto returns no error). L2
// eviction returns the redis error if any; caller may choose to retry.
func (f *Facade[K, V]) Invalidate(ctx context.Context, key K) error {
	f.gen.Add(1)
	keyStr := f.keyer(key)
	f.cache.L1.Del(keyStr)
	f.cache.L1.Wait()
	if err := f.cache.L2.Del(ctx, keyStr).Err(); err != nil {
		return fmt.Errorf("cache %s: invalidate %q: %w", f.name, keyStr, err)
	}
	return nil
}

// InvalidateMany evicts a batch of keys; partial failures returned
// via the redis error. Used for Person-level cascades over per-
// Membership cached resources (e.g. password change → revoke security
// stamps for all of that Person's Memberships) — caller pre-enumerates
// the keys.
//
// One generation bump fences ALL keys against in-flight factory
// writes — the gen counter is per-facade, not per-key, so a single
// Add(1) is sufficient. One Wait() after the Del loop drains the L1
// buffer for all keys at once — cheaper than waiting per-key.
func (f *Facade[K, V]) InvalidateMany(ctx context.Context, keys []K) error {
	if len(keys) == 0 {
		return nil
	}
	f.gen.Add(1)
	strs := make([]string, len(keys))
	for i, k := range keys {
		s := f.keyer(k)
		f.cache.L1.Del(s)
		strs[i] = s
	}
	f.cache.L1.Wait()
	if err := f.cache.L2.Del(ctx, strs...).Err(); err != nil {
		return fmt.Errorf("cache %s: invalidate %d keys: %w", f.name, len(strs), err)
	}
	return nil
}

// Set explicitly stores a value bypassing the factory. Useful for
// pre-warming or write-through patterns where the caller already has
// the canonical value.
//
// Bumps the generation FIRST so any concurrent factory holding a
// pre-Set source read skips its write — Set is the authoritative
// truth at this moment, an in-flight factory's older read should
// not overwrite it.
func (f *Facade[K, V]) Set(ctx context.Context, key K, value V) error {
	keyStr := f.keyer(key)
	raw, err := f.cache.Codec.Encode(value)
	if err != nil {
		return fmt.Errorf("cache %s: encode: %w", f.name, err)
	}
	f.gen.Add(1)
	if err := f.cache.L2.Set(ctx, keyStr, raw, f.ttl.L2).Err(); err != nil {
		return fmt.Errorf("cache %s: L2 set: %w", f.name, err)
	}
	f.cache.L1.SetWithTTL(keyStr, raw, int64(len(raw)), f.ttl.L1)
	f.cache.L1.Wait()
	return nil
}

// TTL returns the configured TTL — exported so facade implementations
// can surface the value in their public API if needed.
func (f *Facade[K, V]) TTL() TTL { return f.ttl }

// Name returns the facade label set at construction.
func (f *Facade[K, V]) Name() string { return f.name }
