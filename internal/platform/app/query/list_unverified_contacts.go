// Package query holds Platform CQRS query handlers.
//
// Read-side handlers project aggregate state into wire-shaped views.
// Boundary discipline (ADR 0047): handlers depend on domain repository
// interfaces; concrete pgx-backed impls live in adapters/.
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

// UnverifiedContactView is the wire-shaped projection. Field names
// avoid the lifecycle-State vs geo-State name clash by using StateGeo
// for the latter (mirrors the column rename in the DB migration).
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

// ListUnverifiedContactsReader is the read-side interface — declared
// here in app/ per Cheney "accept interfaces". Implementation lives in
// internal/platform/adapters.
type ListUnverifiedContactsReader interface {
	ListUnverifiedContactsPage(
		ctx context.Context,
		state string,
		cursor pagination.Cursor,
		pageSize int,
	) ([]UnverifiedContactView, error)
}

// ListUnverifiedContactsHandler returns a keyset-paginated page of
// unverified contacts. Platform-only at the HTTP layer.
type ListUnverifiedContactsHandler struct {
	reader ListUnverifiedContactsReader
}

// NewListUnverifiedContactsHandler wires the handler.
func NewListUnverifiedContactsHandler(reader ListUnverifiedContactsReader) ListUnverifiedContactsHandler {
	return ListUnverifiedContactsHandler{reader: reader}
}

// Handle runs the query. The reader fetches LIMIT+1 + the handler
// builds the wire page via pagination.BuildPage.
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
		// CreatedAt is RFC3339Nano string at the read-model boundary;
		// reader emits time.Time-shape on Cursor (sort_value time.Time).
		// We rebuild via a fresh time.Parse in the encoder side; simpler
		// shape: have the reader emit the time.Time on the cursor
		// directly. For Slice 1 we stay with the string + parse seam —
		// the cost is one Parse per last-row on a paginated request,
		// well below noise.
		return pagination.Cursor{
			SortValue: parseSortValue(v.CreatedAt),
			ID:        v.ID,
		}
	}), nil
}

// parseSortValue is the inverse of the RFC3339Nano serialisation
// reader-side. Returns the zero time on parse failure — handlers
// running into this branch are talking to a broken adapter, not user
// input, so the failure mode is "no next page" + a CI failure when
// observed in tests rather than a runtime error surfaced to the user.
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

// (intentional blank — discourages adding tenant-specific impls here.)
var _ = unverifiedcontact.StateNew // anchor import; prevents future drift
