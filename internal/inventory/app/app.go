// Package app holds the Inventory Application facade.
//
// Mirror of internal/identity/app per TDL Wild Workouts canonical
// layout: an Application{Commands, Queries} struct aggregates concrete
// handler structs as fields. HTTP + future event subscribers call
// `app.Commands.X.Handle(...)` directly — no service interface, no
// mediator.
package app

import (
	"github.com/leadkart/leadkart-go/internal/inventory/app/command"
	"github.com/leadkart/leadkart-go/internal/inventory/app/query"
)

// Application is the Inventory facade.
type Application struct {
	Commands Commands
	Queries  Queries
}

// Commands aggregates all Inventory command handlers.
type Commands struct {
	CreateProduct     command.CreateProductHandler
	UpdateProduct     command.UpdateProductHandler
	DeleteProduct     command.DeleteProductHandler
	AddBatch          command.AddBatchHandler
	LogStockMovement  command.LogStockMovementHandler
}

// Queries aggregates all Inventory query handlers.
type Queries struct {
	GetProduct             query.GetProductHandler
	ListProductsPage       query.ListProductsPageHandler
	GetBatch               query.GetBatchHandler
	ListBatchesByProduct   query.ListBatchesByProductHandler
	ListBatchMovementsPage query.ListBatchMovementsPageHandler
}
