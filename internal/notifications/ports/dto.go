// Package ports holds the Notifications module's inbound HTTP + subscriber
// adapters. HTTP handlers translate request/response shapes into
// [app.Application] calls. Per ADR 0046 the wire shapes here are MIRRORED in
// api/openapi.yaml — adding a route or field requires a paired spec update
// (CI-gated by the route-spec-drift test).
package ports

import "time"

// NotificationDto is the wire shape of one inbox notification.
type NotificationDto struct {
	ID               string     `json:"id"`
	Category         string     `json:"category"`
	Title            string     `json:"title"`
	Body             string     `json:"body,omitzero"`
	State            string     `json:"state"`
	SourceModule     string     `json:"source_module,omitzero"`
	SourceEntityType string     `json:"source_entity_type,omitzero"`
	SourceEntityID   string     `json:"source_entity_id,omitzero"`
	DeepLink         string     `json:"deep_link,omitzero"`
	CreatedAt        time.Time  `json:"created_at"`
	ReadAt           *time.Time `json:"read_at,omitzero"`
	DismissedAt      *time.Time `json:"dismissed_at,omitzero"`
}

// ListNotificationsResponse is the wire shape for GET /api/v1/notifications.
type ListNotificationsResponse struct {
	Items      []NotificationDto `json:"items"`
	HasMore    bool              `json:"has_more"`
	NextCursor string            `json:"next_cursor,omitzero"`
}

// UnreadCountResponse is the wire shape for GET /api/v1/notifications/unread-count.
type UnreadCountResponse struct {
	UnreadCount int64 `json:"unread_count"`
}

// MarkAllReadResponse is the wire shape for POST /api/v1/notifications/read-all.
type MarkAllReadResponse struct {
	MarkedRead int64 `json:"marked_read"`
}

// errorResponse is the wire shape for non-success responses (RFC 7807-ish).
type errorResponse struct {
	Type    string `json:"type,omitzero"`
	Title   string `json:"title,omitzero"`
	Status  int    `json:"status,omitzero"`
	Detail  string `json:"detail,omitzero"`
	Error   string `json:"error"`
	Message string `json:"message,omitzero"`
}

const (
	errCodeUnauthenticated       = "unauthenticated"
	errCodeInvalidCursor         = "invalid_cursor"
	errCodeInvalidState          = "invalid_state"
	errCodeInvalidCategory       = "invalid_category"
	errCodeInvalidNotificationID = "invalid_notification_id"
	errCodeNotificationNotFound  = "notification_not_found"
	errCodeInvalidTransition     = "invalid_transition"
	errCodeInternalError         = "internal_error"
)
