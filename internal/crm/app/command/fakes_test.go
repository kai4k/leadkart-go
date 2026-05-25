package command_test

import (
	"context"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/crm/domain/assignmenthistory"
	"github.com/leadkart/leadkart-go/internal/crm/domain/assignmenthistory/assignmenthistorytest"
	"github.com/leadkart/leadkart-go/internal/crm/domain/calllog"
	"github.com/leadkart/leadkart-go/internal/crm/domain/calllog/calllogtest"
	"github.com/leadkart/leadkart-go/internal/crm/domain/crmlead"
	"github.com/leadkart/leadkart-go/internal/crm/domain/crmlead/crmleadtest"
)

// The per-aggregate fakes live in the canonical <aggregate>test/
// directories per TDL Wild Workouts canon — co-located with the
// aggregate they fake. The newFakeX helpers below are one-line
// aliases so existing tests don't need rewriting at the call sites.

func newFakeLeads() *crmleadtest.FakeRepository    { return crmleadtest.NewFakeRepository() }
func newFakeCallLogs() *calllogtest.FakeRepository { return calllogtest.NewFakeRepository() }
func newFakeHistory() *assignmenthistorytest.FakeRepository {
	return assignmenthistorytest.NewFakeRepository()
}

// fakeUoW satisfies pg.UnitOfWork without opening a real tx. Just
// runs the closure inline.
type fakeUoW struct{}

func (fakeUoW) WithinTx(ctx context.Context, _ pg.TxScope, fn func(ctx context.Context) error) error {
	return fn(ctx)
}

// newTestLeadID is the test-side CrmLead ID factory injected into
// command handlers per the `TestArch_HandlersInjectIDFactory`
// discipline. Production passes the equivalent shape from main.go.
func newTestLeadID() crmlead.ID { return crmlead.ID(ids.NewV7().String()) }

// newTestCallID is the test-side CallLog ID factory.
func newTestCallID() calllog.ID { return calllog.ID(ids.NewV7().String()) }

// newTestHistoryID is the test-side AssignmentHistory ID factory.
func newTestHistoryID() assignmenthistory.ID { return assignmenthistory.ID(ids.NewV7().String()) }
