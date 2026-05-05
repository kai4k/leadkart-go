module github.com/leadkart/leadkart-go

go 1.25.7

// Plan target is Go 1.26+ (per `docs/adr/0034`); bump once toolchain is
// verified locally. Build deps via `tool` directive (Go 1.24+) — replaces
// the older `tools.go` + build-tag hack.
//
// tool (
//     github.com/sqlc-dev/sqlc/cmd/sqlc
//     github.com/pressly/goose/v3/cmd/goose
//     github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen
//     go.uber.org/mock/mockgen
//     golang.org/x/vuln/cmd/govulncheck
//     mvdan.cc/gofumpt
// )

require (
	github.com/google/uuid v1.6.0
	github.com/jackc/pgx/v5 v5.9.2
	github.com/pressly/goose/v3 v3.27.1
)

require (
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/mfridman/interpolate v0.0.2 // indirect
	github.com/sethvargo/go-retry v0.3.0 // indirect
	go.uber.org/multierr v1.11.0 // indirect
	golang.org/x/sync v0.20.0 // indirect
	golang.org/x/text v0.36.0 // indirect
)
