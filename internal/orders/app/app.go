// Package app holds the Orders module Application facade.
//
// TDL Wild Workouts layout: an Application{Commands, Queries} struct aggregates
// concrete handler structs as fields. HTTP + event subscribers call
// `app.Commands.X.Handle(...)` / `app.Queries.Y.Handle(...)` directly — no
// service interface, no mediator. The composition root (cmd/api + cmd/worker)
// builds the Application by wiring concrete adapters into each constructor.
package app

import (
	"github.com/leadkart/leadkart-go/internal/orders/app/command"
	"github.com/leadkart/leadkart-go/internal/orders/app/query"
)

// Application is the Orders facade.
type Application struct {
	Commands Commands
	Queries  Queries
}

// Commands aggregates all Orders command handlers (BRD §6.4 + ADR 0063 saga).
type Commands struct {
	CreateQuotation    command.CreateQuotationHandler
	ReviseQuotation    command.ReviseQuotationHandler
	ApproveQuotation   command.ApproveQuotationHandler
	RejectQuotation    command.RejectQuotationHandler
	RecordTokenPayment command.RecordTokenPaymentHandler
	ConfirmOrder       command.ConfirmOrderHandler
	PackOrder          command.PackOrderHandler
	InvoiceOrder       command.InvoiceOrderHandler
	AttachConsignment  command.AttachConsignmentHandler
	MarkOrderDelivered command.MarkOrderDeliveredHandler
	CompleteOrder      command.CompleteOrderHandler
	CancelOrder        command.CancelOrderHandler
	RecordPayment      command.RecordPaymentHandler
	MintCreditNote     command.MintCreditNoteHandler
}

// Queries aggregates all Orders query handlers.
type Queries struct {
	GetOrder            query.GetOrderHandler
	GetQuotation        query.GetQuotationHandler
	GetInvoiceByOrder   query.GetInvoiceByOrderHandler
	ListPaymentsByOrder query.ListPaymentsByOrderHandler
}
