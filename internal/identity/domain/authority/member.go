package authority

import (
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// Member is the authority-relevant projection of a membership (ADR 0069).
type Member struct {
	id         membership.ID
	tenantID   tenant.ID
	rank       Rank
	owner      bool
	superAdmin bool
	active     bool
	reportsTo  membership.ID
}

// MemberOf projects m with its live roles (callers load them via
// role.Repository.GetByIDs(m.RoleAssignments()) inside their transaction).
func MemberOf(m *membership.Membership, roles []*role.Role) Member {
	out := Member{id: m.ID(), tenantID: m.TenantID(), rank: RankOf(roles),
		active: m.Status() == membership.StatusActive, reportsTo: m.ReportsTo()}
	for _, r := range roles {
		if r == nil || r.IsDeleted() {
			continue
		}
		out.owner = out.owner || r.IsCompanyOwner()
		out.superAdmin = out.superAdmin || r.IsSuperAdmin()
	}
	return out
}

// ID is the membership id.
func (m Member) ID() membership.ID { return m.id }

// TenantID is the membership's tenant.
func (m Member) TenantID() tenant.ID { return m.tenantID }

// Rank is the most senior live role level (RankNone when none).
func (m Member) Rank() Rank { return m.rank }

// IsOwner reports whether the member holds the CompanyOwner role.
func (m Member) IsOwner() bool { return m.owner }

// IsSuperAdmin reports whether the member holds a super-admin role.
func (m Member) IsSuperAdmin() bool { return m.superAdmin }

// IsActive reports whether the membership is active.
func (m Member) IsActive() bool { return m.active }

// ReportsTo is the member's current manager (zero when none).
func (m Member) ReportsTo() membership.ID { return m.reportsTo }
