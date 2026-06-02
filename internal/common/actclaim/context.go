package actclaim

import "context"

// ctxKey is the unexported tag used to stash the Claim onto ctx.
// Unexported per stdlib canon (context.WithValue godoc): ctx keys MUST be
// unexported to prevent cross-package collisions.
type ctxKey struct{}

// WithContext returns a copy of ctx carrying c. Called by the authn
// middleware after JWT verification when claims.Act is non-nil. A zero
// Claim is dropped silently — keeps ctx chains minimal on the
// non-impersonation hot path.
func WithContext(ctx context.Context, c Claim) context.Context {
	if c.IsZero() {
		return ctx
	}
	return context.WithValue(ctx, ctxKey{}, c)
}

// FromContext returns the Claim attached to ctx, plus a presence flag.
// (Claim{}, false) for non-impersonation ctx (the overwhelming hot path)
// — callers branch on the flag to decide whether to write the act_*
// metadata. Returning the zero Claim with ok=false is safe; it satisfies
// the "no act context" semantic identically.
func FromContext(ctx context.Context) (Claim, bool) {
	c, ok := ctx.Value(ctxKey{}).(Claim)
	if !ok {
		return Claim{}, false
	}
	return c, true
}
