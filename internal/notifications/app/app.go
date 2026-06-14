// Package app holds the Notifications Application facade — an
// Application{Commands, Queries} aggregate of concrete handler structs, per the
// TDL Wild Workouts canonical layout. HTTP + event subscribers call
// app.Commands.X.Handle(...) directly; no service interface, no mediator.
package app

import (
	"github.com/leadkart/leadkart-go/internal/notifications/app/command"
	"github.com/leadkart/leadkart-go/internal/notifications/app/query"
)

// Application is the Notifications facade.
type Application struct {
	Commands Commands
	Queries  Queries
}

// Commands aggregates all Notifications command handlers.
type Commands struct {
	CreateNotification command.CreateNotificationHandler
	MarkRead           command.MarkNotificationReadHandler
	Dismiss            command.DismissNotificationHandler
	MarkAllRead        command.MarkAllReadHandler
}

// Queries aggregates all Notifications query handlers.
type Queries struct {
	ListInbox   query.ListInboxHandler
	UnreadCount query.UnreadCountHandler
}
