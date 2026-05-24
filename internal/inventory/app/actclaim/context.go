package actclaim

import "context"

// ctxKey is the unexported tag used to stash the Claim onto ctx.
// Unexported per stdlib canon (context.WithValue godoc): ctx keys MUST
// be unexported to prevent cross-package collisions. Distinct from
// identity's actclaim ctxKey by type identity (each package's empty
// struct is a unique type), so the two packages can coexist without
// collision when both populate the same ctx (e.g. a future composition
// path through both modules).
type ctxKey struct{}

// WithContext returns a copy of ctx carrying c. Called by the authn
// middleware after JWT verification when claims.Act is non-nil. Zero
// Claim is dropped silently — keeps ctx chains minimal on the non-
// impersonation hot path.
func WithContext(ctx context.Context, c Claim) context.Context {
	if c.IsZero() {
		return ctx
	}
	return context.WithValue(ctx, ctxKey{}, c)
}

// FromContext returns the Claim attached to ctx, plus a presence flag.
// (Claim{}, false) for non-impersonation ctx (the overwhelming hot
// path) — callers branch on the flag to decide whether to write the
// act_* columns.
func FromContext(ctx context.Context) (Claim, bool) {
	c, ok := ctx.Value(ctxKey{}).(Claim)
	if !ok {
		return Claim{}, false
	}
	return c, true
}
