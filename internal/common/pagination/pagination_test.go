package pagination_test

import (
	"errors"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/pagination"
)

func TestEncodeDecodeRoundTrip(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 5, 18, 12, 34, 56, 0, time.UTC)
	original := pagination.Cursor{
		SortValue: ts,
		ID:        "019234ab-cd56-7890-abcd-ef0123456789",
	}

	token := pagination.Encode(original)
	if token == "" {
		t.Fatal("encode returned empty token for non-zero cursor")
	}

	decoded, err := pagination.Decode(token)
	if err != nil {
		t.Fatalf("decode round-trip: %v", err)
	}
	if !decoded.SortValue.Equal(original.SortValue) {
		t.Errorf("sort value = %v, want %v", decoded.SortValue, original.SortValue)
	}
	if decoded.ID != original.ID {
		t.Errorf("id = %q, want %q", decoded.ID, original.ID)
	}
}

func TestEncodeEmptyCursor(t *testing.T) {
	t.Parallel()

	got := pagination.Encode(pagination.Cursor{})
	if got != "" {
		t.Errorf("empty cursor encoded as %q, want empty string", got)
	}
}

func TestDecodeEmptyString(t *testing.T) {
	t.Parallel()

	c, err := pagination.Decode("")
	if err != nil {
		t.Fatalf("decode empty: %v", err)
	}
	if !c.SortValue.IsZero() || c.ID != "" {
		t.Errorf("empty string should decode to zero cursor, got %+v", c)
	}
}

func TestDecodeRejectsGarbage(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		input string
	}{
		{"not_base64", "not!!base64@@@"},
		{"valid_base64_but_not_json", pagination.Encode(pagination.Cursor{SortValue: time.Now(), ID: "x"}) + "garbage"},
		{"truncated", "eyJzIjoi"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := pagination.Decode(tc.input)
			if err == nil {
				t.Fatal("expected error decoding garbage; got nil")
			}
			if !errors.Is(err, pagination.ErrInvalidCursor) {
				t.Errorf("error %v should wrap ErrInvalidCursor", err)
			}
		})
	}
}

func TestClampPageSize(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   int
		want int
	}{
		{"zero_returns_default", 0, pagination.DefaultPageSize},
		{"negative_returns_default", -5, pagination.DefaultPageSize},
		{"one_passes_through", 1, 1},
		{"in_range_passes_through", 75, 75},
		{"exactly_max_passes_through", pagination.MaxPageSize, pagination.MaxPageSize},
		{"above_max_caps", pagination.MaxPageSize + 1, pagination.MaxPageSize},
		{"way_above_max_caps", 100_000, pagination.MaxPageSize},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := pagination.ClampPageSize(tc.in)
			if got != tc.want {
				t.Errorf("ClampPageSize(%d) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func TestBuildPage_PartialPage(t *testing.T) {
	t.Parallel()

	// Caller fetched 3 rows with LIMIT (page_size=5)+1=6; got fewer back.
	items := []int{1, 2, 3}
	page := pagination.BuildPage(items, 5, func(i int) pagination.Cursor {
		return pagination.Cursor{SortValue: time.Unix(int64(i), 0).UTC(), ID: ""}
	})
	if page.HasMore {
		t.Error("partial page should have has_more=false")
	}
	if page.NextCursor != "" {
		t.Errorf("partial page should have empty next_cursor; got %q", page.NextCursor)
	}
	if len(page.Items) != 3 {
		t.Errorf("got %d items, want 3", len(page.Items))
	}
}

func TestBuildPage_FullPageDetectsMore(t *testing.T) {
	t.Parallel()

	// Caller fetched LIMIT page_size+1 = 6 rows; the 6th signals "more exists".
	items := []int{1, 2, 3, 4, 5, 6}
	page := pagination.BuildPage(items, 5, func(i int) pagination.Cursor {
		return pagination.Cursor{SortValue: time.Unix(int64(i), 0).UTC(), ID: "id-" + string(rune('0'+i))}
	})
	if !page.HasMore {
		t.Error("full page should have has_more=true")
	}
	if page.NextCursor == "" {
		t.Error("full page should have non-empty next_cursor")
	}
	if len(page.Items) != 5 {
		t.Errorf("got %d items, want 5 (peek row dropped)", len(page.Items))
	}
	// Cursor must point at the LAST item of the returned page (item 5),
	// not the peek row (item 6) — that's what makes "give me the next page"
	// correctly continue from after item 5.
	decoded, err := pagination.Decode(page.NextCursor)
	if err != nil {
		t.Fatalf("decode next_cursor: %v", err)
	}
	if decoded.SortValue != time.Unix(5, 0).UTC() {
		t.Errorf("next_cursor points at sort=%v, want %v (item 5)", decoded.SortValue, time.Unix(5, 0).UTC())
	}
}

func TestBuildPage_ExactlyPageSize(t *testing.T) {
	t.Parallel()

	// Caller fetched LIMIT page_size+1 = 6 rows; got exactly 5 back.
	// This means EXACTLY one page worth of data exists; no more.
	items := []int{1, 2, 3, 4, 5}
	page := pagination.BuildPage(items, 5, func(i int) pagination.Cursor {
		return pagination.Cursor{SortValue: time.Unix(int64(i), 0).UTC(), ID: ""}
	})
	if page.HasMore {
		t.Error("exactly-page-size result should have has_more=false")
	}
	if page.NextCursor != "" {
		t.Errorf("last page should have empty next_cursor; got %q", page.NextCursor)
	}
}
