package authority

import "github.com/leadkart/leadkart-go/internal/identity/domain/role"

// Rank is a member's seniority (ADR 0069 H1): the numerically lowest
// HierarchyLevel among their live roles. Lower = more senior.
type Rank int

// Tier anchors of the rank ladder (ADR 0069 H1); each equals its role level.
const (
	RankOwner    Rank = role.HierarchyLevelOwner
	RankAdmin    Rank = role.HierarchyLevelAdmin
	RankManager  Rank = role.HierarchyLevelManager
	RankTeamLead Rank = role.HierarchyLevelTeamLead
	RankUser     Rank = role.HierarchyLevelDefault
	// RankNone is the rank of a member with no live role, or a token minted before the rank claim (fail closed).
	RankNone Rank = role.HierarchyLevelNoRole
)

// Tier is the wire label of a rank band.
type Tier string

// Tier values (ADR 0069 H1).
const (
	TierOwner    Tier = "owner"
	TierAdmin    Tier = "admin"
	TierManager  Tier = "manager"
	TierTeamLead Tier = "team_lead"
	TierUser     Tier = "user"
)

// Tier bands: 0 owner; 1–10 admin; 11–20 manager; 21–30 team lead; 31–99 user.
func (r Rank) Tier() Tier {
	switch {
	case r <= RankOwner:
		return TierOwner
	case r <= RankAdmin:
		return TierAdmin
	case r <= RankManager:
		return TierManager
	case r <= RankTeamLead:
		return TierTeamLead
	default:
		return TierUser
	}
}

// Int is the numeric (JWT / wire) form.
func (r Rank) Int() int { return int(r) }

// ClampRank maps an untrusted integer (a JWT claim) into [RankOwner, RankNone];
// out-of-range values fail closed to RankNone.
func ClampRank(n int) Rank {
	if n < int(RankOwner) || n > int(RankNone) {
		return RankNone
	}
	return Rank(n)
}

// RankOf returns the most senior level among live roles; RankNone when none.
func RankOf(roles []*role.Role) Rank {
	best := RankNone
	for _, r := range roles {
		if r == nil || r.IsDeleted() {
			continue
		}
		if lvl := Rank(r.HierarchyLevel()); lvl < best {
			best = lvl
		}
	}
	return best
}
