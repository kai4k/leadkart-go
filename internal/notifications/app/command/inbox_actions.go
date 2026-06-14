package command

import (
	"context"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/notifications/domain/notification"
)

// MarkNotificationReadCommand flips one notification unread → read. The
// recipient is taken from the JWT; a notification owned by a different
// membership is reported as ErrNotFound (no existence leak).
type MarkNotificationReadCommand struct {
	TenantID              tenant.ID
	NotificationID        notification.ID
	RecipientMembershipID membership.ID
}

// MarkNotificationReadHandler marks one notification read.
type MarkNotificationReadHandler struct {
	repo notification.Repository
	now  func() time.Time
}

// NewMarkNotificationReadHandler wires the handler (now nil → time.Now).
func NewMarkNotificationReadHandler(repo notification.Repository, now func() time.Time) MarkNotificationReadHandler {
	if now == nil {
		now = time.Now
	}
	return MarkNotificationReadHandler{repo: repo, now: now}
}

// Handle marks the notification read.
func (h MarkNotificationReadHandler) Handle(ctx context.Context, cmd MarkNotificationReadCommand) error {
	err := h.repo.UpdateByID(ctx, cmd.TenantID, cmd.NotificationID, func(n *notification.Notification) (bool, error) {
		if n.RecipientMembershipID() != cmd.RecipientMembershipID {
			return false, notification.ErrNotFound // not yours → 404, no leak
		}
		if merr := n.MarkRead(h.now()); merr != nil {
			return false, merr
		}
		return true, nil
	})
	if err != nil {
		return fmt.Errorf("mark notification read: %w", err)
	}
	return nil
}

// DismissNotificationCommand flips one notification → dismissed.
type DismissNotificationCommand struct {
	TenantID              tenant.ID
	NotificationID        notification.ID
	RecipientMembershipID membership.ID
}

// DismissNotificationHandler dismisses one notification.
type DismissNotificationHandler struct {
	repo notification.Repository
	now  func() time.Time
}

// NewDismissNotificationHandler wires the handler (now nil → time.Now).
func NewDismissNotificationHandler(repo notification.Repository, now func() time.Time) DismissNotificationHandler {
	if now == nil {
		now = time.Now
	}
	return DismissNotificationHandler{repo: repo, now: now}
}

// Handle dismisses the notification.
func (h DismissNotificationHandler) Handle(ctx context.Context, cmd DismissNotificationCommand) error {
	err := h.repo.UpdateByID(ctx, cmd.TenantID, cmd.NotificationID, func(n *notification.Notification) (bool, error) {
		if n.RecipientMembershipID() != cmd.RecipientMembershipID {
			return false, notification.ErrNotFound
		}
		if derr := n.Dismiss(h.now()); derr != nil {
			return false, derr
		}
		return true, nil
	})
	if err != nil {
		return fmt.Errorf("dismiss notification: %w", err)
	}
	return nil
}

// MarkAllReadCommand bulk-flips every unread notification for the recipient.
type MarkAllReadCommand struct {
	TenantID              tenant.ID
	RecipientMembershipID membership.ID
}

// MarkAllReadResult reports how many notifications were flipped.
type MarkAllReadResult struct {
	MarkedRead int64
}

// MarkAllReadHandler bulk-reads the recipient's inbox.
type MarkAllReadHandler struct {
	repo notification.Repository
	now  func() time.Time
}

// NewMarkAllReadHandler wires the handler (now nil → time.Now).
func NewMarkAllReadHandler(repo notification.Repository, now func() time.Time) MarkAllReadHandler {
	if now == nil {
		now = time.Now
	}
	return MarkAllReadHandler{repo: repo, now: now}
}

// Handle marks every unread notification for the recipient as read.
func (h MarkAllReadHandler) Handle(ctx context.Context, cmd MarkAllReadCommand) (MarkAllReadResult, error) {
	n, err := h.repo.UpdateAllUnreadForRecipient(ctx, cmd.TenantID, cmd.RecipientMembershipID, h.now().UnixNano())
	if err != nil {
		return MarkAllReadResult{}, fmt.Errorf("mark all read: %w", err)
	}
	return MarkAllReadResult{MarkedRead: n}, nil
}
