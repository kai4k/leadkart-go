package query_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/clock"
	"github.com/leadkart/leadkart-go/internal/identity/app/query"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/refreshtoken"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

type stubFamilyRepo struct {
	families []*refreshtoken.Family
	listErr  error
}

func (s *stubFamilyRepo) Add(_ context.Context, _ *refreshtoken.Family) error { return nil }
func (s *stubFamilyRepo) UpdateByID(_ context.Context, _ refreshtoken.FamilyID, _ func(*refreshtoken.Family) (bool, error)) error {
	return nil
}
func (s *stubFamilyRepo) GetByID(_ context.Context, _ refreshtoken.FamilyID) (*refreshtoken.Family, error) {
	return nil, refreshtoken.ErrNotFound
}
func (s *stubFamilyRepo) GetByTokenHash(_ context.Context, _ refreshtoken.TokenHash) (*refreshtoken.Family, error) {
	return nil, refreshtoken.ErrNotFound
}
func (s *stubFamilyRepo) ListActiveForPerson(_ context.Context, _ person.ID) ([]*refreshtoken.Family, error) {
	return s.families, s.listErr
}

var _ refreshtoken.Repository = (*stubFamilyRepo)(nil)

func TestListSessions_ReturnsViews(t *testing.T) {
	t.Parallel()
	clock.Set(time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC))
	t.Cleanup(clock.Reset)

	hash, err := refreshtoken.NewTokenHash("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatalf("NewTokenHash: %v", err)
	}
	f, err := refreshtoken.NewFamily(
		refreshtoken.FamilyID("11111111-1111-1111-1111-111111111111"),
		person.ID("p1"),
		tenant.ID("22222222-2222-2222-2222-222222222222"),
		"iphone-15",
		hash,
		14*24*time.Hour,
	)
	if err != nil {
		t.Fatalf("NewFamily: %v", err)
	}
	repo := &stubFamilyRepo{families: []*refreshtoken.Family{f}}
	h := query.NewListSessionsHandler(repo)

	views, err := h.Handle(t.Context(), query.ListSessionsQuery{PersonID: person.ID("p1")})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(views) != 1 {
		t.Fatalf("len(views) = %d, want 1", len(views))
	}
	if views[0].FamilyID != f.ID().String() {
		t.Errorf("FamilyID = %q, want %q", views[0].FamilyID, f.ID().String())
	}
	if views[0].DeviceLabel != "iphone-15" {
		t.Errorf("DeviceLabel = %q", views[0].DeviceLabel)
	}
	if views[0].CreatedAt.IsZero() {
		t.Error("CreatedAt zero")
	}
}

func TestListSessions_RejectsZeroPersonID(t *testing.T) {
	t.Parallel()
	repo := &stubFamilyRepo{}
	h := query.NewListSessionsHandler(repo)
	_, err := h.Handle(t.Context(), query.ListSessionsQuery{PersonID: person.ID("")})
	if err == nil {
		t.Fatal("expected error on zero PersonID")
	}
}

func TestListSessions_PropagatesRepoError(t *testing.T) {
	t.Parallel()
	repoErr := errors.New("db down")
	repo := &stubFamilyRepo{listErr: repoErr}
	h := query.NewListSessionsHandler(repo)
	_, err := h.Handle(t.Context(), query.ListSessionsQuery{PersonID: person.ID("p1")})
	if !errors.Is(err, repoErr) {
		t.Fatalf("err = %v, want repoErr", err)
	}
}

func TestNewListSessionsHandler_PanicsOnNilRepo(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic")
		}
	}()
	_ = query.NewListSessionsHandler(nil)
}
