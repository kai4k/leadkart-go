package adapters

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/identity/adapters/db"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permissionrequest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// PermissionRequestRepository is the pgx/sqlc-backed implementation of
// [permissionrequest.Repository]. Tenant-scoped — every read + write
// runs under [pg.TxScopeTenant] so the connection's `app.tenant_id`
// GUC binds before queries touch the table; Postgres RLS does the rest.
//
// When ctx carries an active tx (a parent [pg.UnitOfWork] is in flight),
// Add joins that tx rather than opening its own — same shape as
// MembershipRepository.Add per ADR 0047.
type PermissionRequestRepository struct {
	pool *pgxpool.Pool
	tx   *pg.Transactor
	q    *db.Queries
}

// NewPermissionRequestRepository wires the repository.
func NewPermissionRequestRepository(pool *pgxpool.Pool, tx *pg.Transactor) *PermissionRequestRepository {
	return &PermissionRequestRepository{pool: pool, tx: tx, q: db.New(pool)}
}

// Add satisfies [permissionrequest.Repository] — persists a brand-new
// Pending Request. Translates SQLSTATE 23505 on
// uq_permission_requests_pending into
// [permissionrequest.ErrPendingRequestExists].
func (r *PermissionRequestRepository) Add(ctx context.Context, req *permissionrequest.Request) error {
	if tx, ok := pg.TxFromContext(ctx); ok {
		return r.addOnTx(ctx, tx, req)
	}
	return r.tx.WithinTxPgx(ctx, pg.TxScopeTenant, func(ctx context.Context, tx pgx.Tx) error {
		return r.addOnTx(ctx, tx, req)
	})
}

func (r *PermissionRequestRepository) addOnTx(
	ctx context.Context,
	tx pgx.Tx,
	req *permissionrequest.Request,
) error {
	q := r.q.WithTx(tx)
	if err := insertPermissionRequestRow(ctx, q, req); err != nil {
		return err
	}
	return drainPermissionRequestEvents(ctx, tx, req)
}

// GetByID satisfies [permissionrequest.Repository]. RLS-scoped read.
func (r *PermissionRequestRepository) GetByID(
	ctx context.Context,
	id permissionrequest.ID,
) (*permissionrequest.Request, error) {
	var out *permissionrequest.Request
	err := r.tx.WithinTxPgx(ctx, pg.TxScopeTenant, func(ctx context.Context, tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		got, err := loadPermissionRequest(ctx, q, id)
		if err != nil {
			return err
		}
		out = got
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// UpdateByID satisfies [permissionrequest.Repository] — TDL Sep 2024
// UpdateFn pattern: load → updateFn → persist (if shouldPersist) →
// drain events. All in one tenant-scoped transaction.
//
// Approve / Deny / Cancel all flow through here; the aggregate's state
// machine validates legality, the SQL just writes whatever the
// aggregate computed.
func (r *PermissionRequestRepository) UpdateByID(
	ctx context.Context,
	id permissionrequest.ID,
	updateFn func(*permissionrequest.Request) (bool, error),
) error {
	// When the caller is already inside a [pg.UnitOfWork] (e.g. the
	// Approve handler that writes BOTH the request decision AND the
	// membership grant in the same tx) join that tx so all writes share
	// the outbox row's commit point.
	if tx, ok := pg.TxFromContext(ctx); ok {
		return r.updateOnTx(ctx, tx, id, updateFn)
	}
	return r.tx.WithinTxPgx(ctx, pg.TxScopeTenant, func(ctx context.Context, tx pgx.Tx) error {
		return r.updateOnTx(ctx, tx, id, updateFn)
	})
}

func (r *PermissionRequestRepository) updateOnTx(
	ctx context.Context,
	tx pgx.Tx,
	id permissionrequest.ID,
	updateFn func(*permissionrequest.Request) (bool, error),
) error {
	q := r.q.WithTx(tx)
	req, err := loadPermissionRequest(ctx, q, id)
	if err != nil {
		return err
	}
	shouldPersist, err := updateFn(req)
	if err != nil {
		return err
	}
	if !shouldPersist {
		return nil
	}
	if err := persistPermissionRequestDecision(ctx, q, req); err != nil {
		return err
	}
	return drainPermissionRequestEvents(ctx, tx, req)
}

// GetPendingForMembership satisfies [permissionrequest.Repository].
// RLS-scoped read; tenant context must be bound by the caller.
func (r *PermissionRequestRepository) GetPendingForMembership(
	ctx context.Context,
	m membership.ID,
) ([]*permissionrequest.Request, error) {
	mid, err := uuid.Parse(m.String())
	if err != nil {
		return nil, fmt.Errorf("permreq repo: parse membership id %q: %w", m, err)
	}
	var out []*permissionrequest.Request
	err = r.tx.WithinTxPgx(ctx, pg.TxScopeTenant, func(ctx context.Context, tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		rows, err := q.ListPendingPermissionRequestsForMembership(ctx, pgUUID(mid))
		if err != nil {
			return fmt.Errorf("permreq repo: list pending for membership: %w", err)
		}
		out = make([]*permissionrequest.Request, 0, len(rows))
		for _, row := range rows {
			got, perr := rowToPermissionRequest(row)
			if perr != nil {
				return perr
			}
			out = append(out, got)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListPendingApprovableBy satisfies [permissionrequest.Repository].
// Keyset-paginated approver queue. ADR 0038 — first page passes the
// zero cursor; adapter applies the sentinel (now+1d, max-uuid) so the
// tuple-comparison admits every row.
func (r *PermissionRequestRepository) ListPendingApprovableBy(
	ctx context.Context,
	approver membership.ID,
	pageSize int,
	cursor pagination.Cursor,
) (pagination.Page[*permissionrequest.Request], error) {
	approverUUID, err := uuid.Parse(approver.String())
	if err != nil {
		return pagination.Page[*permissionrequest.Request]{},
			fmt.Errorf("permreq repo: parse approver id %q: %w", approver, err)
	}
	beforeAt, beforeID := cursorOrPermReqSentinel(cursor)
	limit := pageSize + 1
	var rows []db.IdentityPermissionRequest
	err = r.tx.WithinTxPgx(ctx, pg.TxScopeTenant, func(ctx context.Context, tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		got, qerr := q.ListPendingPermissionRequestsByApproverPage(ctx, db.ListPendingPermissionRequestsByApproverPageParams{
			ApproverMembershipID: pgUUID(approverUUID),
			Column2:              pgRequiredTimestamp(beforeAt),
			Column3:              pgUUID(beforeID),
			Limit:                int32(limit), //nolint:gosec // bounded by pagination.ClampPageSize (≤200)
		})
		if qerr != nil {
			return fmt.Errorf("permreq repo: list pending by approver: %w", qerr)
		}
		rows = got
		return nil
	})
	if err != nil {
		return pagination.Page[*permissionrequest.Request]{}, err
	}
	items := make([]*permissionrequest.Request, 0, len(rows))
	for _, row := range rows {
		got, perr := rowToPermissionRequest(row)
		if perr != nil {
			return pagination.Page[*permissionrequest.Request]{}, perr
		}
		items = append(items, got)
	}
	return pagination.BuildPage(items, pageSize, func(req *permissionrequest.Request) pagination.Cursor {
		return pagination.Cursor{SortValue: req.CreatedAt(), ID: req.ID().String()}
	}), nil
}

// ListByRequester satisfies [permissionrequest.Repository]. Returns
// keyset-paginated history (all states) for the requester membership.
func (r *PermissionRequestRepository) ListByRequester(
	ctx context.Context,
	requester membership.ID,
	pageSize int,
	cursor pagination.Cursor,
) (pagination.Page[*permissionrequest.Request], error) {
	requesterUUID, err := uuid.Parse(requester.String())
	if err != nil {
		return pagination.Page[*permissionrequest.Request]{},
			fmt.Errorf("permreq repo: parse requester id %q: %w", requester, err)
	}
	beforeAt, beforeID := cursorOrPermReqSentinel(cursor)
	limit := pageSize + 1
	var rows []db.IdentityPermissionRequest
	err = r.tx.WithinTxPgx(ctx, pg.TxScopeTenant, func(ctx context.Context, tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		got, qerr := q.ListPermissionRequestsByRequesterPage(ctx, db.ListPermissionRequestsByRequesterPageParams{
			RequesterMembershipID: pgUUID(requesterUUID),
			Column2:               pgRequiredTimestamp(beforeAt),
			Column3:               pgUUID(beforeID),
			Limit:                 int32(limit), //nolint:gosec // bounded by pagination.ClampPageSize (≤200)
		})
		if qerr != nil {
			return fmt.Errorf("permreq repo: list by requester: %w", qerr)
		}
		rows = got
		return nil
	})
	if err != nil {
		return pagination.Page[*permissionrequest.Request]{}, err
	}
	items := make([]*permissionrequest.Request, 0, len(rows))
	for _, row := range rows {
		got, perr := rowToPermissionRequest(row)
		if perr != nil {
			return pagination.Page[*permissionrequest.Request]{}, perr
		}
		items = append(items, got)
	}
	return pagination.BuildPage(items, pageSize, func(req *permissionrequest.Request) pagination.Cursor {
		return pagination.Cursor{SortValue: req.CreatedAt(), ID: req.ID().String()}
	}), nil
}

// ----- Helpers --------------------------------------------------------------

func loadPermissionRequest(
	ctx context.Context,
	q *db.Queries,
	id permissionrequest.ID,
) (*permissionrequest.Request, error) {
	rid, err := uuid.Parse(id.String())
	if err != nil {
		return nil, fmt.Errorf("permreq repo: parse id %q: %w", id, err)
	}
	row, err := q.GetPermissionRequestByID(ctx, pgUUID(rid))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, permissionrequest.ErrNotFound
		}
		return nil, fmt.Errorf("permreq repo: get by id: %w", err)
	}
	return rowToPermissionRequest(row)
}

func insertPermissionRequestRow(
	ctx context.Context,
	q *db.Queries,
	req *permissionrequest.Request,
) error {
	rid, err := uuid.Parse(req.ID().String())
	if err != nil {
		return fmt.Errorf("permreq repo: parse id %q: %w", req.ID(), err)
	}
	tid, err := uuid.Parse(req.TenantID().String())
	if err != nil {
		return fmt.Errorf("permreq repo: parse tenant id %q: %w", req.TenantID(), err)
	}
	mid, err := uuid.Parse(req.RequesterMembershipID().String())
	if err != nil {
		return fmt.Errorf("permreq repo: parse requester id %q: %w", req.RequesterMembershipID(), err)
	}
	err = q.InsertPermissionRequest(ctx, db.InsertPermissionRequestParams{
		ID:                    pgUUID(rid),
		TenantID:              pgUUID(tid),
		RequesterMembershipID: pgUUID(mid),
		PermissionConstant:    req.Permission().Name(),
		DurationDays:          int32(req.DurationDays()), //nolint:gosec // bounded [1,90] by aggregate
		Reason:                req.Reason(),
		CreatedAt:             pgRequiredTimestamp(req.CreatedAt()),
	})
	if err != nil {
		if isPendingRequestUniqueViolation(err) {
			return permissionrequest.ErrPendingRequestExists
		}
		return fmt.Errorf("permreq repo: insert: %w", err)
	}
	return nil
}

func persistPermissionRequestDecision(
	ctx context.Context,
	q *db.Queries,
	req *permissionrequest.Request,
) error {
	rid, err := uuid.Parse(req.ID().String())
	if err != nil {
		return fmt.Errorf("permreq repo: parse id %q: %w", req.ID(), err)
	}
	approverPg := pgtype.UUID{}
	if id := req.ApproverMembershipID(); !id.IsZero() {
		auid, perr := uuid.Parse(id.String())
		if perr != nil {
			return fmt.Errorf("permreq repo: parse approver id %q: %w", id, perr)
		}
		approverPg = pgUUID(auid)
	}
	var decisionReason *string
	if rsn := req.DecisionReason(); rsn != "" {
		decisionReason = &rsn
	}
	grantedPg := pgUUIDOpt(req.GrantedOverrideID())
	err = q.UpdatePermissionRequestDecision(ctx, db.UpdatePermissionRequestDecisionParams{
		ID:                   pgUUID(rid),
		State:                string(req.State()),
		ApproverMembershipID: approverPg,
		DecidedAt:            pgTimestamp(req.DecidedAt()),
		DecisionReason:       decisionReason,
		GrantedOverrideID:    grantedPg,
		ExpiresAt:            pgTimestamp(req.ExpiresAt()),
		UpdatedAt:            pgRequiredTimestamp(req.UpdatedAt()),
	})
	if err != nil {
		return fmt.Errorf("permreq repo: update decision: %w", err)
	}
	return nil
}

func drainPermissionRequestEvents(ctx context.Context, tx pgx.Tx, req *permissionrequest.Request) error {
	evs := req.PullEvents()
	if len(evs) == 0 {
		return nil
	}
	tid, err := uuid.Parse(req.TenantID().String())
	if err != nil {
		return fmt.Errorf("permreq repo: parse tenant id %q: %w", req.TenantID(), err)
	}
	asAny := make([]any, len(evs))
	for i, e := range evs {
		asAny[i] = e
	}
	mapped, err := mapDomainEvents(asAny)
	if err != nil {
		return fmt.Errorf("permreq repo: map events: %w", err)
	}
	return writeOutboxEvents(ctx, tx, tid, mapped)
}

func rowToPermissionRequest(row db.IdentityPermissionRequest) (*permissionrequest.Request, error) {
	perm, err := permission.TryFromConstant(row.PermissionConstant)
	if err != nil {
		return nil, fmt.Errorf("permreq repo: unknown permission %q in row: %w",
			row.PermissionConstant, err)
	}
	approverID := membership.ID("")
	if a := uuidFromPg(row.ApproverMembershipID); a != uuid.Nil {
		approverID = membership.ID(a.String())
	}
	decisionReason := ""
	if row.DecisionReason != nil {
		decisionReason = *row.DecisionReason
	}
	return permissionrequest.UnmarshalFromDB(permissionrequest.Snapshot{
		ID:                    permissionrequest.ID(uuidFromPg(row.ID).String()),
		TenantID:              tenant.ID(uuidFromPg(row.TenantID).String()),
		RequesterMembershipID: membership.ID(uuidFromPg(row.RequesterMembershipID).String()),
		Permission:            perm,
		DurationDays:          int(row.DurationDays),
		Reason:                row.Reason,
		State:                 permissionrequest.State(row.State),
		ApproverMembershipID:  approverID,
		DecidedAt:             timeFromPg(row.DecidedAt),
		DecisionReason:        decisionReason,
		GrantedOverrideID:     uuidFromPg(row.GrantedOverrideID),
		ExpiresAt:             timeFromPg(row.ExpiresAt),
		CreatedAt:             timeFromPg(row.CreatedAt),
		UpdatedAt:             timeFromPg(row.UpdatedAt),
	}), nil
}

// constraintPermissionRequestsPending names the partial unique index
// from migration 20260523000003 enforcing the at-most-one-pending
// invariant per (requester_membership_id, permission_constant).
const constraintPermissionRequestsPending = "uq_permission_requests_pending"

func isPendingRequestUniqueViolation(err error) bool {
	pgErr, ok := errors.AsType[*pgconn.PgError](err)
	if !ok {
		return false
	}
	if pgErr.Code != pg.SQLStateUniqueViolation {
		return false
	}
	return pgErr.ConstraintName == constraintPermissionRequestsPending
}

// permReqFirstPageBefore / permReqFirstPageBeforeID are the sentinel
// tuple values supplied on the first page (no cursor). Mirrors the
// audit.FirstPageBefore pattern — admit every row by setting the upper
// bound to a far-future timestamp + the all-FF UUID.
var (
	permReqFirstPageBefore   = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
	permReqFirstPageBeforeID = uuid.MustParse("ffffffff-ffff-ffff-ffff-ffffffffffff")
)

func cursorOrPermReqSentinel(c pagination.Cursor) (time.Time, uuid.UUID) {
	if c.ID == "" && c.SortValue.IsZero() {
		return permReqFirstPageBefore, permReqFirstPageBeforeID
	}
	id, err := uuid.Parse(c.ID)
	if err != nil {
		return permReqFirstPageBefore, permReqFirstPageBeforeID
	}
	return c.SortValue, id
}
