package cache

import (
	"context"
	"errors"
	"fmt"

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
type Facade[K comparable, V any] struct {
	cache   *HybridCache
	keyer   func(K) string
	factory func(ctx context.Context, key K) (V, error)
	ttl     TTL
	name    string
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
// ristretto's Del is asynchronous — it queues the eviction to an
// internal buffer (per the v2 Cache.Wait godoc: "Wait blocks until
// all buffered writes have been applied"). Without the Wait, a Get
// landing immediately after Invalidate races the buffer drain and
// can still return the stale value. The Wait makes Invalidate
// synchronous from the caller's perspective so the contract
// "after Invalidate returns, the next Get does not see the old
// value" holds. Acceptable cost: Invalidate runs on the rare cascade
// subscriber path, not the per-request hot path.
//
// L1 eviction is best-effort (ristretto returns no error). L2 eviction
// returns the redis error if any; caller may choose to retry.
func (f *Facade[K, V]) Invalidate(ctx context.Context, key K) error {
	keyStr := f.keyer(key)
	f.cache.L1.Del(keyStr)
	f.cache.L1.Wait()
	if err := f.cache.L2.Del(ctx, keyStr).Err(); err != nil {
		return fmt.Errorf("cache %s: invalidate %q: %w", f.name, keyStr, err)
	}
	return nil
}

// InvalidateMany evicts a batch of keys; partial failures collected
// via errors.Join. Used for Person-level cascades over per-Membership
// cached resources (e.g. password change → revoke security stamps for
// all of that Person's Memberships) — caller pre-enumerates the keys.
//
// One Wait() after the Del loop drains the L1 buffer for ALL keys at
// once — cheaper than waiting per-key.
func (f *Facade[K, V]) InvalidateMany(ctx context.Context, keys []K) error {
	if len(keys) == 0 {
		return nil
	}
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
func (f *Facade[K, V]) Set(ctx context.Context, key K, value V) error {
	keyStr := f.keyer(key)
	raw, err := f.cache.Codec.Encode(value)
	if err != nil {
		return fmt.Errorf("cache %s: encode: %w", f.name, err)
	}
	if err := f.cache.L2.Set(ctx, keyStr, raw, f.ttl.L2).Err(); err != nil {
		return fmt.Errorf("cache %s: L2 set: %w", f.name, err)
	}
	f.cache.L1.SetWithTTL(keyStr, raw, int64(len(raw)), f.ttl.L1)
	return nil
}

// TTL returns the configured TTL — exported so facade implementations
// can surface the value in their public API if needed.
func (f *Facade[K, V]) TTL() TTL { return f.ttl }

// Name returns the facade label set at construction.
func (f *Facade[K, V]) Name() string { return f.name }
