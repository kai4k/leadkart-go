package query_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/app/query"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// recordingSearcher records what the handler asked of the index.
type recordingSearcher struct {
	tenantID tenant.ID
	q        string
	limit    int
	hits     []query.MemberHit
	err      error
}

func (r *recordingSearcher) SearchActiveMembers(_ context.Context, tenantID tenant.ID, q string, limit int) ([]query.MemberHit, error) {
	r.tenantID, r.q, r.limit = tenantID, q, limit
	return r.hits, r.err
}

func TestSearchMembers_ValidatesAndClampsBeforeTheIndexIsAsked(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		q         string
		limit     int
		wantErr   error
		wantQ     string
		wantLimit int
	}{
		{"too short", "a", 0, query.ErrMemberSearchQueryTooShort, "", 0},
		{"blank after trim", "     ", 0, query.ErrMemberSearchQueryTooShort, "", 0},
		{"one character padded with spaces", "  a  ", 0, query.ErrMemberSearchQueryTooShort, "", 0},
		{"too long", strings.Repeat("a", 101), 0, query.ErrMemberSearchQueryTooLong, "", 0},
		{"exactly 100 characters", strings.Repeat("a", 100), 0, nil, strings.Repeat("a", 100), 10},
		{"100 multi-byte characters are 100, not 300", strings.Repeat("र", 100), 0, nil, strings.Repeat("र", 100), 10},
		{"trimmed, default limit", "  ravi  ", 0, nil, "ravi", 10},
		{"limit over the cap is 20", "ravi", 500, nil, "ravi", 20},
		{"negative limit is the default", "ravi", -3, nil, "ravi", 10},
		{"limit 7 stands", "ravi", 7, nil, "ravi", 7},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			idx := &recordingSearcher{}
			got, err := query.NewSearchMembersHandler(idx).Handle(t.Context(), query.SearchMembersQuery{TenantID: testTenantID, Q: c.q, Limit: c.limit})
			if !errors.Is(err, c.wantErr) {
				t.Fatalf("err = %v, want %v", err, c.wantErr)
			}
			if c.wantErr != nil {
				if idx.q != "" {
					t.Errorf("a refused query still reached the index: %q", idx.q)
				}
				return
			}
			if idx.q != c.wantQ || idx.limit != c.wantLimit || idx.tenantID != testTenantID {
				t.Errorf("index asked (%q, %d, %s), want (%q, %d, %s)", idx.q, idx.limit, idx.tenantID, c.wantQ, c.wantLimit, testTenantID)
			}
			if got == nil {
				t.Error("no match must be an empty list, never nil")
			}
		})
	}
}

func TestSearchMembers_ZeroTenantAndIndexFailureAreErrors(t *testing.T) {
	t.Parallel()
	if _, err := query.NewSearchMembersHandler(&recordingSearcher{}).Handle(t.Context(), query.SearchMembersQuery{Q: "ravi"}); err == nil {
		t.Error("a zero tenant was accepted")
	}
	boom := errors.New("boom")
	if _, err := query.NewSearchMembersHandler(&recordingSearcher{err: boom}).Handle(t.Context(), query.SearchMembersQuery{TenantID: testTenantID, Q: "ravi"}); !errors.Is(err, boom) {
		t.Errorf("err = %v, want it to wrap the index error", err)
	}
	defer func() {
		if recover() == nil {
			t.Error("a nil index did not panic")
		}
	}()
	_ = query.NewSearchMembersHandler(nil) // arch-test:ignore-err - panic check
}
