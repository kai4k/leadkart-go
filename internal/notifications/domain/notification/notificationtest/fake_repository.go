// Package notificationtest provides the in-memory FakeRepository
// implementing [notification.Repository]. TDL canon.
package notificationtest

import (
	"cmp"
	"context"
	"slices"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/notifications/domain/notification"
)

// DedupWindow is the in-memory mirror of the partial unique index
// behaviour: a duplicate Add inside this window for the same
// (recipient, source_entity_type, source_entity_id, category) returns
// [notification.ErrDuplicateInDedupWindow]. Tests pin the window
// behaviour via the Now() function — control time, control dedup.
//
// Production migration sets this via index expression
// `WHERE created_at > now() - interval '5 minutes'`; the fake
// honours the same semantic.
var DedupWindow = 5 * time.Minute

// FakeRepository is an in-memory [notification.Repository].
type FakeRepository struct {
	Store         map[notification.ID]*notification.Notification
	DrainedEvents []notification.Event
}

// NewFakeRepository constructs an empty fake.
func NewFakeRepository() *FakeRepository {
	return &FakeRepository{Store: make(map[notification.ID]*notification.Notification)}
}

// Compile-time interface conformance.
var _ notification.Repository = (*FakeRepository)(nil)

// Add satisfies [notification.Repository]. Enforces the partial-unique
// dedup invariant.
func (r *FakeRepository) Add(_ context.Context, n *notification.Notification) error {
	if n.SourceEntityType() != "" && n.SourceEntityID() != "" {
		// Walk existing rows for the same recipient + source + category
		// within the dedup window. Inefficient O(N) — fine for tests.
		newWindow := n.CreatedAt().Add(-DedupWindow)
		for _, existing := range r.Store {
			if existing.TenantID() != n.TenantID() ||
				existing.RecipientMembershipID() != n.RecipientMembershipID() ||
				existing.Category() != n.Category() ||
				existing.SourceEntityType() != n.SourceEntityType() ||
				existing.SourceEntityID() != n.SourceEntityID() {
				continue
			}
			if existing.CreatedAt().After(newWindow) {
				return notification.ErrDuplicateInDedupWindow
			}
		}
	}
	r.Store[n.ID()] = n
	r.DrainedEvents = append(r.DrainedEvents, n.PullEvents()...)
	return nil
}

// GetByID satisfies [notification.Repository].
func (r *FakeRepository) GetByID(
	_ context.Context, tenantID tenant.ID, id notification.ID,
) (*notification.Notification, error) {
	n, ok := r.Store[id]
	if !ok || n.TenantID() != tenantID {
		return nil, notification.ErrNotFound
	}
	return n, nil
}

// UpdateByID satisfies [notification.Repository].
func (r *FakeRepository) UpdateByID(
	ctx context.Context, tenantID tenant.ID, id notification.ID,
	mutator func(*notification.Notification) (bool, error),
) error {
	n, err := r.GetByID(ctx, tenantID, id)
	if err != nil {
		return err
	}
	changed, mErr := mutator(n)
	if mErr != nil {
		return mErr
	}
	if !changed {
		return nil
	}
	r.DrainedEvents = append(r.DrainedEvents, n.PullEvents()...)
	return nil
}

// ListPageForRecipient satisfies [notification.Repository]. Returns
// recipient inbox ordered newest first.
func (r *FakeRepository) ListPageForRecipient(
	_ context.Context, tenantID tenant.ID, recipient membership.ID,
	filter notification.ListFilter, _ pagination.Cursor, pageSize int,
) (pagination.Page[*notification.Notification], error) {
	if pageSize <= 0 {
		pageSize = 50
	}
	matched := make([]*notification.Notification, 0)
	for _, n := range r.Store {
		if n.TenantID() != tenantID || n.RecipientMembershipID() != recipient {
			continue
		}
		if filter.State != "" && n.State() != filter.State {
			continue
		}
		if filter.Category != "" && n.Category() != filter.Category {
			continue
		}
		matched = append(matched, n)
	}
	slices.SortFunc(matched, func(a, b *notification.Notification) int {
		return cmp.Or(
			b.CreatedAt().Compare(a.CreatedAt()), // created_at DESC
			cmp.Compare(b.ID(), a.ID()),          // id DESC tiebreaker
		)
	})
	hasMore := false
	if len(matched) > pageSize {
		matched = matched[:pageSize]
		hasMore = true
	}
	return pagination.Page[*notification.Notification]{
		Items:   matched,
		HasMore: hasMore,
	}, nil
}

// CountUnreadForRecipient satisfies [notification.Repository].
func (r *FakeRepository) CountUnreadForRecipient(
	_ context.Context, tenantID tenant.ID, recipient membership.ID,
) (int64, error) {
	var count int64
	for _, n := range r.Store {
		if n.TenantID() == tenantID &&
			n.RecipientMembershipID() == recipient &&
			n.State() == notification.StateUnread {
			count++
		}
	}
	return count, nil
}

// UpdateAllUnreadForRecipient satisfies [notification.Repository].
// Flips every unread row to read with the supplied timestamp. The
// signature takes int64 unix-nano per the canonical Postgres
// timestamptz translation; tests pass time.Now().UnixNano().
func (r *FakeRepository) UpdateAllUnreadForRecipient(
	_ context.Context, tenantID tenant.ID, recipient membership.ID, readAt int64,
) (int64, error) {
	var affected int64
	at := time.Unix(0, readAt).UTC()
	for _, n := range r.Store {
		if n.TenantID() == tenantID &&
			n.RecipientMembershipID() == recipient &&
			n.State() == notification.StateUnread {
			if err := n.MarkRead(at); err == nil {
				affected++
				r.DrainedEvents = append(r.DrainedEvents, n.PullEvents()...)
			}
		}
	}
	return affected, nil
}
