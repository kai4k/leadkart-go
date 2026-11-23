// Package actor carries the person who issued a command across the
// HTTP-handler -> command-handler -> outbox boundary via context.Context, so
// the outbox can stamp "who did it" on the message and the audit middleware
// can record it as the audit row's user_id.
//
// It is the sibling of common/actclaim (the RFC 8693 impersonation chain) and
// common/tenancy: cross-cutting context propagation, stdlib only. Under
// impersonation the actor is still the person whose token it is (the JWT
// subject); the operator travels separately in actclaim.
package actor

import "context"

type ctxKey struct{}

// WithPerson returns a copy of ctx carrying the acting person's id (the JWT
// subject). An empty id returns ctx unchanged.
func WithPerson(ctx context.Context, personID string) context.Context {
	if personID == "" {
		return ctx
	}
	return context.WithValue(ctx, ctxKey{}, personID)
}

// PersonFromContext returns the acting person's id and whether one is set.
func PersonFromContext(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(ctxKey{}).(string)
	return id, ok && id != ""
}
