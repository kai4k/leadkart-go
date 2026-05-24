package query_test

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/platform/app/query"
)

// fakeListReader satisfies [query.ListUnverifiedContactsReader] with an
// in-memory slice + optional state filter. C2 — review-pass: the
// handler test owns its own narrow fake (the platformtest package's
// FakeUnverifiedContactRepository is the WRITE-side fake; the read
// path uses a different interface per ADR 0047).
type fakeListReader struct {
	rows []query.UnverifiedContactView
}

func (f *fakeListReader) ListUnverifiedContactsPage(
	_ context.Context,
	state string,
	cursor pagination.Cursor,
	pageSize int,
) ([]query.UnverifiedContactView, error) {
	// State filter — empty == all.
	var filtered []query.UnverifiedContactView
	for _, r := range f.rows {
		if state != "" && r.State != state {
			continue
		}
		filtered = append(filtered, r)
	}
	// Stable sort by CreatedAt DESC — mirrors the production SQL
	// ORDER BY (created_at DESC, id DESC).
	sort.SliceStable(filtered, func(i, j int) bool {
		return filtered[i].CreatedAt > filtered[j].CreatedAt
	})
	// Cursor — skip until past the cursor's id+sort_value.
	if !cursor.SortValue.IsZero() && cursor.ID != "" {
		drop := 0
		for i, r := range filtered {
			if r.ID == cursor.ID {
				drop = i + 1
				break
			}
		}
		filtered = filtered[drop:]
	}
	if pageSize > 0 && len(filtered) > pageSize {
		filtered = filtered[:pageSize]
	}
	return filtered, nil
}

func mkView(id string, state string, createdAt time.Time) query.UnverifiedContactView {
	return query.UnverifiedContactView{
		ID:                    id,
		State:                 state,
		ContactName:           "Test " + id[:6],
		MobileE164:            "+919876543210",
		City:                  "Pune",
		StateGeo:              "Maharashtra",
		CreatedAt:             createdAt.Format(time.RFC3339Nano),
		CreatedByMembershipID: ids.NewV7().String(),
	}
}

// TestListUnverifiedContacts_HappyPath — happy path. Handler builds
// the wire-shaped page via pagination.BuildPage. C2.
func TestListUnverifiedContacts_HappyPath(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	reader := &fakeListReader{
		rows: []query.UnverifiedContactView{
			mkView(ids.NewV7().String(), "new", now),
			mkView(ids.NewV7().String(), "verified", now.Add(-time.Hour)),
		},
	}

	h := query.NewListUnverifiedContactsHandler(reader)
	page, err := h.Handle(context.Background(), query.ListUnverifiedContactsQuery{
		PageSize: 50,
	})
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(page.Items))
	}
	// HasMore is false — we have 2 + asked for 50.
	if page.HasMore {
		t.Error("HasMore expected false")
	}
}

// TestListUnverifiedContacts_StateFilter — only rows matching state.
func TestListUnverifiedContacts_StateFilter(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	reader := &fakeListReader{
		rows: []query.UnverifiedContactView{
			mkView(ids.NewV7().String(), "new", now),
			mkView(ids.NewV7().String(), "verified", now.Add(-time.Hour)),
			mkView(ids.NewV7().String(), "rejected", now.Add(-2*time.Hour)),
		},
	}

	h := query.NewListUnverifiedContactsHandler(reader)
	page, err := h.Handle(context.Background(), query.ListUnverifiedContactsQuery{
		State:    "new",
		PageSize: 50,
	})
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(page.Items))
	}
	if page.Items[0].State != "new" {
		t.Errorf("state=%q want new", page.Items[0].State)
	}
}

// TestListUnverifiedContacts_PaginationHasMore — when more rows exist
// than page-size, HasMore is true + NextCursor non-empty. The reader
// fetches LIMIT+1; the handler drops the +1 and surfaces it as cursor.
func TestListUnverifiedContacts_PaginationHasMore(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	rows := make([]query.UnverifiedContactView, 0, 5)
	for i := range 5 {
		rows = append(rows, mkView(ids.NewV7().String(), "new", now.Add(-time.Duration(i)*time.Minute)))
	}
	reader := &fakeListReader{rows: rows}

	h := query.NewListUnverifiedContactsHandler(reader)
	page, err := h.Handle(context.Background(), query.ListUnverifiedContactsQuery{
		PageSize: 3,
	})
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if len(page.Items) != 3 {
		t.Fatalf("expected 3 items, got %d", len(page.Items))
	}
	if !page.HasMore {
		t.Error("HasMore expected true")
	}
	if page.NextCursor == "" {
		t.Error("NextCursor expected non-empty")
	}
}
