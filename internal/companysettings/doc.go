// Package companysettings is the companysettings bounded context (ADR 0070):
// one versioned commerce policy per company, its full history, and the
// company's own GST rate list.
//
// It is the single OWNER of company commerce settings. Orders and Inventory
// each keep an event-fed replica of the policy (companysettings.policy_changed.v1)
// and read it inside their own transaction, so a business rule is never fetched
// across a module boundary on a write path.
//
// Layout (TDL ports/adapters):
//
//	internal/companysettings/
//	├── domain/commercesettings   Settings aggregate, events, repository interface
//	├── app/                      command + query handlers, daily republish job
//	├── ports/                    HTTP routes, tenant-registered subscriber
//	├── adapters/                 pgx/sqlc repository, outbox drain
//	└── integrationevents/        wire records (companysettings.policy_changed.v1)
//
// The policy value object, its defaults and the registry of settings live in
// internal/common/commercepolicy, shared by the three modules because modules
// may not import each other.
package companysettings
