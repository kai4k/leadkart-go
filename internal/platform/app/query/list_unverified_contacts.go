// Package query holds Platform CQRS query handlers: read-side handlers that
// project aggregate state into wire-shaped views. Per ADR 0047, handlers depend
// on domain repository interfaces; concrete pgx impls live in adapters/.
package query

import (
	"context"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/platform/domain/unverifiedcontact"
)

// ListUnverifiedContactsQuery carries the filter + paging input.
// State filter is optional — empty string means "all states".
type ListUnverifiedContactsQuery struct {
	State    string // empty = no filter
	Cursor   pagination.Cursor
	PageSize int
}

// UnverifiedContactView is the wire-shaped projection. StateGeo disambiguates
// the geo state from the lifecycle State (mirrors the DB column rename).
type UnverifiedContactView struct {
	ID                    string
	State                 string // lifecycle: new | in_call | verified | rejected | busy
	ContactName           string
	MobileE164            string
	City                  string
	StateGeo              string // geo: Indian state name (e.g. "Maharashtra")
	CreatedAt             string // RFC3339Nano string at this boundary
	CreatedByMembershipID string
}

// ListUnverifiedContactsReader is the read-side interface, declared with its
// consumer per Cheney "accept interfaces". Impl lives in platform/adapters.
type ListUnverifiedContactsReader interface {
	ListUnverifiedContactsPage(
		ctx context.Context,
		state string,
		cursor pagination.Cursor,
		pageSize int,
	) ([]UnverifiedContactView, error)
}

// ListUnverifiedContactsHandler returns a keyset-paginated page of unverified
// contacts. Platform-only at the HTTP layer.
type ListUnverifiedContactsHandler struct {
	reader ListUnverifiedContactsReader
}

// NewListUnverifiedContactsHandler wires the handler.
func NewListUnverifiedContactsHandler(reader ListUnverifiedContactsReader) ListUnverifiedContactsHandler {
	return ListUnverifiedContactsHandler{reader: reader}
}

// Handle fetches LIMIT+1 from the reader and builds the wire page.
func (h ListUnverifiedContactsHandler) Handle(
	ctx context.Context,
	q ListUnverifiedContactsQuery,
) (pagination.Page[UnverifiedContactView], error) {
	size := pagination.ClampPageSize(q.PageSize)
	rows, err := h.reader.ListUnverifiedContactsPage(ctx, q.State, q.Cursor, size+1)
	if err != nil {
		return pagination.Page[UnverifiedContactView]{}, fmt.Errorf("list unverified contacts: %w", err)
	}
	return pagination.BuildPage(rows, size, func(v UnverifiedContactView) pagination.Cursor {
		// CreatedAt is an RFC3339Nano string at the read-model boundary; we
		// reparse it for the cursor's time.Time sort value. One Parse per
		// last-row per page — negligible, so we keep the string+parse seam
		// rather than have the reader emit time.Time on the cursor.
		return pagination.Cursor{
			SortValue: parseSortValue(v.CreatedAt),
			ID:        v.ID,
		}
	}), nil
}

// parseSortValue inverts the reader-side RFC3339Nano serialisation. Returns
// zero time on parse failure: this branch means a broken adapter, not bad user
// input, so the failure mode is "no next page" plus a CI failure under test,
// not a runtime error surfaced to the user.
func parseSortValue(rfc string) time.Time {
	if rfc == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, rfc)
	if err != nil {
		return time.Time{}
	}
	return t
}

var _ = unverifiedcontact.StateNew // anchor import; prevents drift
