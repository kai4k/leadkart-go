// Package app holds the Platform module Application facade.
//
// Per TDL Wild Workouts: an Application{Commands, Queries} struct aggregates
// concrete handler structs; ports call app.Commands.X.Handle(...) directly —
// no service interface, no mediator. The composition root (cmd/api/main.go)
// wires adapters into each handler. Tests build a partial Application with
// fakes for the handlers under test.
package app

import (
	"github.com/leadkart/leadkart-go/internal/platform/app/command"
	"github.com/leadkart/leadkart-go/internal/platform/app/query"
)

// Application is the Platform facade. Ports take an Application and dispatch
// directly into its handler fields.
type Application struct {
	Commands Commands
	Queries  Queries
}

// Commands aggregates the Platform command handlers, one field per use case.
type Commands struct {
	CreateUnverifiedContact command.CreateUnverifiedContactHandler
	LogVerificationCall     command.LogVerificationCallHandler
	VerifyUnverifiedContact command.VerifyUnverifiedContactHandler
	RejectUnverifiedContact command.RejectUnverifiedContactHandler
	PurchaseLead            command.PurchaseLeadHandler
	TopupLeadCredits        command.TopupLeadCreditsHandler
}

// Queries aggregates the Platform read-side query handlers.
type Queries struct {
	ListUnverifiedContacts query.ListUnverifiedContactsHandler
	BrowseMarketplace      query.BrowseMarketplaceHandler
	GetLeadCreditBalance   query.GetLeadCreditBalanceHandler
}
