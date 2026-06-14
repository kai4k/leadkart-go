// Package command holds the Notifications CQRS command handlers. Per ADR 0047
// boundary discipline: handlers depend only on the domain repository interface
// + an injected clock + ID factory — never pgx or a concrete adapter.
package command

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/notifications/domain/notification"
)

// CreateNotificationCommand creates one user-facing notification. Sourced
// almost exclusively by the cross-module subscribers (subscriber-decides);
// manual creation via HTTP is rare but supported.
type CreateNotificationCommand struct {
	TenantID              tenant.ID
	RecipientMembershipID membership.ID
	Category              notification.Category
	Title                 string
	Body                  string
	SourceModule          string
	SourceEntityType      string
	SourceEntityID        string
	DeepLink              string
	CreatedByMembershipID membership.ID
}

// CreateNotificationResult reports the new id and whether a row was actually
// written. Created=false is the idempotent dedup no-op (a same-source
// notification to this recipient already exists in the window).
type CreateNotificationResult struct {
	NotificationID notification.ID
	Created        bool
}

// CreateNotificationHandler creates notifications.
type CreateNotificationHandler struct {
	repo  notification.Repository
	now   func() time.Time
	newID func() notification.ID
}

// NewCreateNotificationHandler wires the handler. newID is required
// (TestArch_HandlersInjectIDFactory); now nil → time.Now.
func NewCreateNotificationHandler(repo notification.Repository, now func() time.Time, newID func() notification.ID) CreateNotificationHandler {
	if newID == nil {
		panic("command: NewCreateNotificationHandler newID required")
	}
	if now == nil {
		now = time.Now
	}
	return CreateNotificationHandler{repo: repo, now: now, newID: newID}
}

// Handle creates the notification, treating a dedup-window collision as success.
func (h CreateNotificationHandler) Handle(ctx context.Context, cmd CreateNotificationCommand) (CreateNotificationResult, error) {
	n, err := notification.New(notification.NewInput{
		ID:                    h.newID(),
		TenantID:              cmd.TenantID,
		RecipientMembershipID: cmd.RecipientMembershipID,
		Category:              cmd.Category,
		Title:                 cmd.Title,
		Body:                  cmd.Body,
		SourceModule:          cmd.SourceModule,
		SourceEntityType:      cmd.SourceEntityType,
		SourceEntityID:        cmd.SourceEntityID,
		DeepLink:              cmd.DeepLink,
		CreatedByMembershipID: cmd.CreatedByMembershipID,
		Now:                   h.now(),
	})
	if err != nil {
		return CreateNotificationResult{}, fmt.Errorf("create notification: %w", err)
	}
	if aerr := h.repo.Add(ctx, n); aerr != nil {
		if errors.Is(aerr, notification.ErrDuplicateInDedupWindow) {
			return CreateNotificationResult{NotificationID: n.ID(), Created: false}, nil
		}
		return CreateNotificationResult{}, fmt.Errorf("create notification: %w", aerr)
	}
	return CreateNotificationResult{NotificationID: n.ID(), Created: true}, nil
}
