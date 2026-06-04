package app

import (
	"github.com/leadkart/leadkart-go/internal/tasks/app/command"
	"github.com/leadkart/leadkart-go/internal/tasks/app/query"
)

// Application is the Tasks facade. Every external port (HTTP,
// subscribers, jobs) takes an Application and dispatches directly
// into its handler fields. Mirror of crm/app.Application.
type Application struct {
	Commands Commands
	Queries  Queries
}

// Commands aggregates all Tasks command handlers.
type Commands struct {
	CreateWorkItem        command.CreateWorkItemHandler
	StartWorkItem         command.StartWorkItemHandler
	CompleteWorkItem      command.CompleteWorkItemHandler
	CancelWorkItem        command.CancelWorkItemHandler
	ReassignWorkItem      command.ReassignWorkItemHandler
	MarkOverdue           command.MarkOverdueHandler
	AutoCreateFromCallLog command.AutoCreateFromCallLogHandler
	AutoCreateFollowUp    command.AutoCreateFollowUpHandler
	AutoCompleteBySource  command.AutoCompleteBySourceHandler
}

// Queries aggregates all Tasks query handlers.
type Queries struct {
	GetWorkItem   query.GetWorkItemHandler
	ListWorkItems query.ListWorkItemsHandler
	Dashboard     query.DashboardHandler
}
