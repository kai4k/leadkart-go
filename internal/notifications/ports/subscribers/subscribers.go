// Package subscribers holds the Notifications module's inbound event consumers.
// Subscriber-decides (Udi Dahan): this module subscribes to OTHER modules'
// integration events and decides recipient + content; producers don't know
// about notifications. Cross-module by integration-event contract only.
//
// v1 wires the events that carry a DIRECT recipient membership (no identity /
// role lookup): CRM lead-assigned, Tasks work-item created + overdue, Orders
// order-confirmed. Role-fan-out categories (lead_converted to the management
// chain, work_item_overdue hierarchy, stock alerts) are forward-declared in the
// notification.Category catalogue and added as their recipient-resolution
// (identity hierarchy reads) lands.
package subscribers

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	crmevents "github.com/leadkart/leadkart-go/internal/crm/integrationevents"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/notifications/app/command"
	"github.com/leadkart/leadkart-go/internal/notifications/domain/notification"
	ordersevents "github.com/leadkart/leadkart-go/internal/orders/integrationevents"
	tasksevents "github.com/leadkart/leadkart-go/internal/tasks/integrationevents"
)

// Handler names — CI-stable. DO NOT rename.
const (
	HandlerLeadAssigned    = "notifications.subscribers.LeadAssigned"
	HandlerWorkItemCreated = "notifications.subscribers.WorkItemCreated"
	HandlerWorkItemOverdue = "notifications.subscribers.WorkItemOverdue"
	HandlerOrderConfirmed  = "notifications.subscribers.OrderConfirmed"
)

// arch-test:idempotency-via-dedup-window — every subscriber routes through
// CreateNotification → repository.Add, whose time-windowed dedup probe
// suppresses a re-delivered source event (ErrDuplicateInDedupWindow → success).
// The router IdempotencyMiddleware is the primary dedup; this is the safety net.

// midString maps a uuid to its string form, or "" for the nil uuid (so the
// optional created_by maps to NULL and a missing recipient is detectable).
func midString(u uuid.UUID) string {
	if u == uuid.Nil {
		return ""
	}
	return u.String()
}

// LeadAssignedSubscriber notifies the new assignee of a CRM lead.
type LeadAssignedSubscriber struct {
	create command.CreateNotificationHandler
	log    *slog.Logger
}

// NewLeadAssignedSubscriber wires the subscriber. log is required.
func NewLeadAssignedSubscriber(create command.CreateNotificationHandler, log *slog.Logger) *LeadAssignedSubscriber {
	if log == nil {
		panic("subscribers: NewLeadAssignedSubscriber log required")
	}
	return &LeadAssignedSubscriber{create: create, log: log}
}

// Handle is the typed cqrs handler for crm.lead_assigned.v1.
func (h *LeadAssignedSubscriber) Handle(ctx context.Context, evt *crmevents.CrmLeadAssignedV1) error {
	recipient := midString(evt.AssigneeMembershipID)
	if recipient == "" {
		return nil // unassigned — nobody to notify
	}
	leadID := evt.LeadID.String()
	_, err := h.create.Handle(ctx, command.CreateNotificationCommand{
		TenantID:              tenant.ID(evt.TenantID().String()),
		RecipientMembershipID: membership.ID(recipient),
		Category:              notification.CategoryLeadAssigned,
		Title:                 "A new lead was assigned to you",
		SourceModule:          "crm",
		SourceEntityType:      "lead",
		SourceEntityID:        leadID,
		DeepLink:              "/crm/leads/" + leadID,
		CreatedByMembershipID: membership.ID(midString(evt.AssignedByMembershipID)),
	})
	if err != nil {
		return fmt.Errorf("notifications lead-assigned: %w", err)
	}
	return nil
}

// WorkItemCreatedSubscriber notifies the assignee of a newly created task.
type WorkItemCreatedSubscriber struct {
	create command.CreateNotificationHandler
	log    *slog.Logger
}

// NewWorkItemCreatedSubscriber wires the subscriber. log is required.
func NewWorkItemCreatedSubscriber(create command.CreateNotificationHandler, log *slog.Logger) *WorkItemCreatedSubscriber {
	if log == nil {
		panic("subscribers: NewWorkItemCreatedSubscriber log required")
	}
	return &WorkItemCreatedSubscriber{create: create, log: log}
}

// Handle is the typed cqrs handler for tasks.work_item_created.v1.
func (h *WorkItemCreatedSubscriber) Handle(ctx context.Context, evt *tasksevents.WorkItemCreatedV1) error {
	recipient := midString(evt.AssignedToMembershipID)
	if recipient == "" {
		return nil // unassigned task — nobody to notify yet
	}
	workItemID := evt.WorkItemID.String()
	title := "New task assigned to you"
	if evt.Title != "" {
		title = "New task: " + evt.Title
	}
	_, err := h.create.Handle(ctx, command.CreateNotificationCommand{
		TenantID:              tenant.ID(evt.TenantID().String()),
		RecipientMembershipID: membership.ID(recipient),
		Category:              notification.CategoryWorkItemAssigned,
		Title:                 title,
		SourceModule:          "tasks",
		SourceEntityType:      "work_item",
		SourceEntityID:        workItemID,
		DeepLink:              "/tasks/" + workItemID,
		CreatedByMembershipID: membership.ID(midString(evt.AssignedByMembershipID)),
	})
	if err != nil {
		return fmt.Errorf("notifications work-item-created: %w", err)
	}
	return nil
}

// WorkItemOverdueSubscriber notifies the assignee that their task is overdue.
type WorkItemOverdueSubscriber struct {
	create command.CreateNotificationHandler
	log    *slog.Logger
}

// NewWorkItemOverdueSubscriber wires the subscriber. log is required.
func NewWorkItemOverdueSubscriber(create command.CreateNotificationHandler, log *slog.Logger) *WorkItemOverdueSubscriber {
	if log == nil {
		panic("subscribers: NewWorkItemOverdueSubscriber log required")
	}
	return &WorkItemOverdueSubscriber{create: create, log: log}
}

// Handle is the typed cqrs handler for tasks.work_item_overdue.v1. Cron-sourced,
// so there is no human creator — created_by is left empty (NULL).
func (h *WorkItemOverdueSubscriber) Handle(ctx context.Context, evt *tasksevents.WorkItemOverdueV1) error {
	recipient := midString(evt.AssignedToMembershipID)
	if recipient == "" {
		return nil
	}
	workItemID := evt.WorkItemID.String()
	_, err := h.create.Handle(ctx, command.CreateNotificationCommand{
		TenantID:              tenant.ID(evt.TenantID().String()),
		RecipientMembershipID: membership.ID(recipient),
		Category:              notification.CategoryWorkItemOverdue,
		Title:                 "A task assigned to you is overdue",
		SourceModule:          "tasks",
		SourceEntityType:      "work_item",
		SourceEntityID:        workItemID,
		DeepLink:              "/tasks/" + workItemID,
	})
	if err != nil {
		return fmt.Errorf("notifications work-item-overdue: %w", err)
	}
	return nil
}

// OrderConfirmedSubscriber notifies the order confirmer that the order is set.
type OrderConfirmedSubscriber struct {
	create command.CreateNotificationHandler
	log    *slog.Logger
}

// NewOrderConfirmedSubscriber wires the subscriber. log is required.
func NewOrderConfirmedSubscriber(create command.CreateNotificationHandler, log *slog.Logger) *OrderConfirmedSubscriber {
	if log == nil {
		panic("subscribers: NewOrderConfirmedSubscriber log required")
	}
	return &OrderConfirmedSubscriber{create: create, log: log}
}

// Handle is the typed cqrs handler for orders.order_confirmed.v1.
func (h *OrderConfirmedSubscriber) Handle(ctx context.Context, evt *ordersevents.OrderConfirmedV1) error {
	recipient := evt.ConfirmedByMembershipID
	if recipient == "" {
		return nil
	}
	_, err := h.create.Handle(ctx, command.CreateNotificationCommand{
		TenantID:              tenant.ID(evt.TenantID),
		RecipientMembershipID: membership.ID(recipient),
		Category:              notification.CategoryOrderConfirmed,
		Title:                 "Order confirmed",
		SourceModule:          "orders",
		SourceEntityType:      "order",
		SourceEntityID:        evt.OrderID,
		DeepLink:              "/orders/" + evt.OrderID,
		CreatedByMembershipID: membership.ID(recipient),
	})
	if err != nil {
		return fmt.Errorf("notifications order-confirmed: %w", err)
	}
	return nil
}
