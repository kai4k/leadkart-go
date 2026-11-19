// Package query holds the Notifications CQRS query handlers. Strict CQRS:
// handlers return flat read-model Views, never the aggregate
// (TestArch_QueryHandlersReturnReadModelsNotAggregates).
package query

import (
	"context"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/notifications/domain/notification"
)

// NotificationView is the flat read model for the inbox.
type NotificationView struct {
	ID               string
	Category         string
	Title            string
	Body             string
	State            string
	SourceModule     string
	SourceEntityType string
	SourceEntityID   string
	DeepLink         string
	CreatedAtUTC     time.Time
	ReadAtUTC        *time.Time
	DismissedAtUTC   *time.Time
}

func projectNotification(n *notification.Notification) NotificationView {
	return NotificationView{
		ID:               n.ID().String(),
		Category:         n.Category().String(),
		Title:            n.Title(),
		Body:             n.Body(),
		State:            n.State().String(),
		SourceModule:     n.SourceModule(),
		SourceEntityType: n.SourceEntityType(),
		SourceEntityID:   n.SourceEntityID(),
		DeepLink:         n.DeepLink(),
		CreatedAtUTC:     n.CreatedAt().UTC(),
		ReadAtUTC:        n.ReadAt(),
		DismissedAtUTC:   n.DismissedAt(),
	}
}

// ListInboxQuery requests the recipient's paginated inbox.
type ListInboxQuery struct {
	TenantID              tenant.ID
	RecipientMembershipID membership.ID
	Filter                notification.ListFilter
	Cursor                pagination.Cursor
	PageSize              int
}

// ListInboxHandler returns the recipient's inbox as read-model Views.
type ListInboxHandler struct {
	repo notification.Repository
}

// NewListInboxHandler wires the handler.
func NewListInboxHandler(repo notification.Repository) ListInboxHandler {
	return ListInboxHandler{repo: repo}
}

// Handle returns the inbox page projected to Views.
func (h ListInboxHandler) Handle(ctx context.Context, q ListInboxQuery) (pagination.Page[NotificationView], error) {
	page, err := h.repo.ListPageForRecipient(ctx, q.TenantID, q.RecipientMembershipID, q.Filter, q.Cursor, pagination.ClampPageSize(q.PageSize))
	if err != nil {
		return pagination.Page[NotificationView]{}, fmt.Errorf("list inbox: %w", err)
	}
	views := make([]NotificationView, 0, len(page.Items))
	for _, n := range page.Items {
		views = append(views, projectNotification(n))
	}
	return pagination.Page[NotificationView]{Items: views, HasMore: page.HasMore, NextCursor: page.NextCursor}, nil
}

// UnreadCountQuery requests the recipient's unread badge count.
type UnreadCountQuery struct {
	TenantID              tenant.ID
	RecipientMembershipID membership.ID
}

// UnreadCountHandler returns the unread count.
type UnreadCountHandler struct {
	repo notification.Repository
}

// NewUnreadCountHandler wires the handler.
func NewUnreadCountHandler(repo notification.Repository) UnreadCountHandler {
	return UnreadCountHandler{repo: repo}
}

// Handle returns the recipient's unread notification count.
func (h UnreadCountHandler) Handle(ctx context.Context, q UnreadCountQuery) (int64, error) {
	n, err := h.repo.CountUnreadForRecipient(ctx, q.TenantID, q.RecipientMembershipID)
	if err != nil {
		return 0, fmt.Errorf("unread count: %w", err)
	}
	return n, nil
}
