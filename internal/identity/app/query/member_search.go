package query

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// Member search bounds (GET /api/v1/users/search). Measured in characters
// after trimming.
const (
	memberSearchMinLen       = 2
	memberSearchMaxLen       = 100
	memberSearchDefaultLimit = 10
	memberSearchMaxLimit     = 20
)

// ErrMemberSearchQueryTooShort is returned for a query of fewer than 2 characters after trimming.
var ErrMemberSearchQueryTooShort = errors.New("member search: query too short (min 2 characters)")

// ErrMemberSearchQueryTooLong is returned for a query of more than 100 characters after trimming.
var ErrMemberSearchQueryTooLong = errors.New("member search: query too long (max 100 characters)")

// MemberHit is one picker row. It carries what a picker needs to show a person
// and nothing else: no email, no phone, no person id.
type MemberHit struct {
	MembershipID string
	DisplayName  string
	Designation  string
}

// MemberSearcher is the consumer-side port of the member search: ACTIVE
// members of one tenant whose display name contains q (a case-insensitive
// substring; the adapter owns pattern escaping), best first, at most limit.
type MemberSearcher interface {
	SearchActiveMembers(ctx context.Context, tenantID tenant.ID, q string, limit int) ([]MemberHit, error)
}

// SearchMembersQuery carries the validated-at-the-handler request inputs.
type SearchMembersQuery struct {
	TenantID tenant.ID
	Q        string
	Limit    int
}

// SearchMembersHandler runs the picker search.
type SearchMembersHandler struct{ index MemberSearcher }

// NewSearchMembersHandler wires the handler.
func NewSearchMembersHandler(index MemberSearcher) SearchMembersHandler {
	if index == nil {
		panic("query: NewSearchMembersHandler member search index required")
	}
	return SearchMembersHandler{index: index}
}

// Handle trims q, enforces 2-100 characters, clamps limit to 1-20 (default 10)
// and returns the matches. The tenant is explicit (ADR 0062); the adapter
// binds it for RLS and in the SQL.
func (h SearchMembersHandler) Handle(ctx context.Context, q SearchMembersQuery) ([]MemberHit, error) {
	if q.TenantID.IsZero() {
		return nil, errors.New("search_members: tenant id required")
	}
	text := strings.TrimSpace(q.Q)
	switch n := utf8.RuneCountInString(text); {
	case n < memberSearchMinLen:
		return nil, ErrMemberSearchQueryTooShort
	case n > memberSearchMaxLen:
		return nil, ErrMemberSearchQueryTooLong
	}
	limit := q.Limit
	if limit <= 0 {
		limit = memberSearchDefaultLimit
	}
	limit = min(limit, memberSearchMaxLimit)
	hits, err := h.index.SearchActiveMembers(ctx, q.TenantID, text, limit)
	if err != nil {
		return nil, fmt.Errorf("search_members: %w", err)
	}
	if hits == nil {
		hits = []MemberHit{}
	}
	return hits, nil
}
