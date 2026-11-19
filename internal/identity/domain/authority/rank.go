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
// A value outside [0, 99] is no rank at all and reads as the user tier, never owner.
func (r Rank) Tier() Tier {
	switch {
	case r < RankOwner:
		return TierUser
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

// levelOf is a role's effective level: out-of-range levels are RankNone, and
// level 0 counts only on the CompanyOwner system role. On any other role it
// counts as the user tier (RankUser), so a stray level-0 custom role (POST
// /roles decoded an omitted level as 0) fails closed instead of making its
// holder the most senior non-owner. The data migration re-levels stored rows
// the same way (ADR 0069 R2).
func levelOf(r *role.Role) Rank {
	lvl := ClampRank(r.HierarchyLevel())
	if lvl == RankOwner && !r.IsCompanyOwner() {
		return RankUser
	}
	return lvl
}

// RankOf returns the most senior level among live roles; RankNone when none.
// A level-0 role that is not the CompanyOwner system role counts as RankUser.
func RankOf(roles []*role.Role) Rank {
	best := RankNone
	for _, r := range roles {
		if r == nil || r.IsDeleted() {
			continue
		}
		if lvl := levelOf(r); lvl < best {
			best = lvl
		}
	}
	return best
}
