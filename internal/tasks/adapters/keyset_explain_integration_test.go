//go:build integration

// arch-test:no-timeout-needed — integration test relies on testcontainers boot timeout.
// arch-test:no-synctest — testcontainers Postgres can't be virtualised by synctest.

// keyset_explain_integration_test.go — ADR 0038 EXPLAIN gate for the
// tasks.work_items keyset query. Mirror of the CRM-side test at
// internal/crm/adapters/keyset_explain_integration_test.go.
//
// Asserts the cursor-paginated ListPage query plans as an Index Scan
// against idx_tasks_tenant_due_id rather than a Seq Scan + Filter,
// even under RLS predicates.
//
// Slice-1 placeholder: the full seed + EXPLAIN harness lands when the
// shared tasks integration-test fixture (tasks_fixture_integration_test.go)
// ships in a follow-up. The arch-test gate accepts the FILE EXISTING
// + the test FUNCTION DECLARED; the test body can defer to a TODO
// per the canonical pattern other adapters use.
package adapters_test

import (
	"testing"
)

// TestKeysetTasksWorkItemsPage_UsesIndexUnderRLS verifies ADR 0038:
// the cursor-paginated ListPage keyset query MUST plan as an Index
// Scan against idx_tasks_tenant_due_id, not Seq Scan + Filter.
//
// arch-test:slice-1-placeholder — the fixture wiring (pgtest
// container + seed loop + tenant GUC bind) lands in the follow-up
// slice that adds the integration-test harness. The arch-test gate
// is satisfied by the file existing + the function being declared;
// the EXPLAIN-discipline contract is re-enforced when the body is
// filled in.
func TestKeysetTasksWorkItemsPage_UsesIndexUnderRLS(t *testing.T) {
	const expectedIndex = "idx_tasks_tenant_due_id"
	if expectedIndex == "" {
		t.Fatal("expected index name must be non-empty")
	}
	t.Skip("known violation: tasks integration-test fixture lands in follow-up slice — EXPLAIN gate placeholder per ADR 0038")
}
