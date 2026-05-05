// Package pg holds platform-level Postgres primitives shared across modules:
// the migrations test (this package) plus future pgxpool factory + tenancy
// `SET LOCAL app.tenant_id` helpers consumed by every module's repository
// adapter.
//
// Per ADR 0006 (multi-tenancy via Postgres RLS + SET LOCAL) and ADR 0019
// (testing with testcontainers).
package pg
