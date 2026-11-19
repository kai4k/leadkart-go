package command

import "github.com/leadkart/leadkart-go/internal/identity/domain/authority"

// rankClaim renders the ADR 0069 rank for the JWT `rank` claim. It is the one
// place a rank is minted: login and refresh both call it with the rank that
// permissions.Resolver.ResolveAuth computed from the same role load as the
// permission claims. Impersonation tokens carry no rank (authority.NewActor
// forces Admin-equivalent for them).
func rankClaim(r authority.Rank) *int {
	n := r.Int()
	return &n
}
