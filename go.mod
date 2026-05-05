module github.com/leadkart/leadkart-go

go 1.25

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
