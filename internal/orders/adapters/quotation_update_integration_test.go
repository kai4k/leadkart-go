//go:build integration

// arch-test:no-timeout-needed — shared pgtest container + pgxpool conn; short
// tenant-scoped txs only.

package adapters_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/common/tenancy"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/orders/adapters"
	"github.com/leadkart/leadkart-go/internal/orders/domain/quotation"
)

// TestQuotationRepository_UpdateByID_ReviseApproveReject pins the UpdateFn
// path against real Postgres: JSONB revision history grows durably across
// Revise, Approve freezes state + approver, and Reject persists the reason —
// none of which the fake-backed lifecycle test proves.
func TestQuotationRepository_UpdateByID_ReviseApproveReject(t *testing.T) {
	tx := pg.NewTransactor(ordersPool(t))
	repo := adapters.NewQuotationRepository(ordersPool(t), tx)
	tid := tenant.ID(uuid.NewString())
	actor := membership.ID(uuid.NewString())

	newQuote := func() quotation.ID {
		q, err := quotation.New(quotation.NewInput{
			ID:                    quotation.ID(uuid.NewString()),
			TenantID:              tid,
			CustomerLeadID:        quotation.CustomerLeadID(uuid.NewString()),
			InitialItems:          []quotation.LineItem{sampleLineItem()},
			InitialNote:           "first pass",
			CreatedByMembershipID: actor,
			Now:                   nowUTC(),
		})
		require.NoError(t, err)
		ctx := tenancy.WithID(t.Context(), tenancy.ID(tid.String()))
		require.NoError(t, repo.Add(ctx, q))
		return q.ID()
	}

	// Revise twice → revision history persists through JSONB round trips.
	qid := newQuote()
	for i := 0; i < 2; i++ {
		item := sampleLineItem()
		item.Quantity = int32(20 + i)
		require.NoError(t, repo.UpdateByID(t.Context(), tid, qid, func(q *quotation.Quotation) (bool, error) {
			return true, q.Revise(quotation.ReviseInput{
				Items:               []quotation.LineItem{item},
				Note:                "rev",
				RevisedByMembership: actor,
				Now:                 nowUTC(),
			})
		}))
	}
	got, err := repo.GetByID(t.Context(), tid, qid)
	require.NoError(t, err)
	require.Equal(t, int64(3), got.CurrentRevision().Number)
	require.Equal(t, int32(21), got.CurrentRevision().Items[0].Quantity)
	require.Len(t, got.Revisions(), 3)

	// Approve → state + approver persist; tip revision frozen.
	require.NoError(t, repo.UpdateByID(t.Context(), tid, qid, func(q *quotation.Quotation) (bool, error) {
		return true, q.Approve(actor, nowUTC())
	}))
	got, err = repo.GetByID(t.Context(), tid, qid)
	require.NoError(t, err)
	require.Equal(t, quotation.StateApproved, got.State())
	require.NotNil(t, got.ApprovedAt())
	require.Equal(t, actor, *got.ApprovedByMembershipID())

	// Reject on a fresh quote → reason + rejector persist.
	qid2 := newQuote()
	require.NoError(t, repo.UpdateByID(t.Context(), tid, qid2, func(q *quotation.Quotation) (bool, error) {
		return true, q.Reject(actor, "price too high", nowUTC())
	}))
	got, err = repo.GetByID(t.Context(), tid, qid2)
	require.NoError(t, err)
	require.Equal(t, quotation.StateRejected, got.State())
	require.Equal(t, "price too high", got.RejectionReason())
	require.NotNil(t, got.RejectedAt())

	// UpdateByID on a missing id surfaces ErrNotFound.
	err = repo.UpdateByID(t.Context(), tid, quotation.ID(uuid.NewString()), func(q *quotation.Quotation) (bool, error) {
		return true, nil
	})
	require.ErrorIs(t, err, quotation.ErrNotFound)
}
