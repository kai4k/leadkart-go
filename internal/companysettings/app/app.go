// Package app holds the companysettings module Application facade.
//
// Per TDL Wild Workouts canonical layout: an Application{Commands, Queries}
// struct aggregates concrete handler structs as fields. HTTP, event subscribers
// and jobs call `app.Commands.X.Handle(...)` directly.
package app

import (
	"github.com/leadkart/leadkart-go/internal/companysettings/app/command"
	"github.com/leadkart/leadkart-go/internal/companysettings/app/query"
)

// Application is the companysettings facade.
type Application struct {
	Commands Commands
	Queries  Queries
}

// Commands aggregates all companysettings command handlers.
type Commands struct {
	ChangeSettings command.ChangeSettingsHandler
	AddGSTRate     command.AddGSTRateHandler
	UpdateGSTRate  command.UpdateGSTRateHandler
	SeedSettings   command.SeedSettingsHandler
	Republish      command.RepublishHandler
}

// Queries aggregates all companysettings query handlers.
type Queries struct {
	GetSettings  query.GetSettingsHandler
	ListHistory  query.ListHistoryHandler
	ListGSTRates query.ListGSTRatesHandler
}
