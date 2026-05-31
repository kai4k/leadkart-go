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
	"github.com/leadkart/leadkart-go/internal/common/pgconv"
	"github.com/leadkart/leadkart-go/internal/identity/adapters/db"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permissionrequest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// PermissionRequestRepository is the pgx/sqlc-backed [permissionrequest.Repository].
// Tenant-scoped: all reads and writes run under [pg.TxScopeTenant].
// Joins an ambient UoW tx when present (ADR 0047).
type PermissionRequestRepository struct {
	pool *pgxpool.Pool
	tx   *pg.Transactor
	q    *db.Queries
}

// NewPermissionRequestRepository wires the repository.
func NewPermissionRequestRepository(pool *pgxpool.Pool, tx *pg.Transactor) *PermissionRequestRepository {
	return &PermissionRequestRepository{pool: pool, tx: tx, q: db.New(pool)}
}

// Add satisfies [permissionrequest.Repository]. Translates SQLSTATE 23505
// on uq_permission_requests_pending to [permissionrequest.ErrPendingRequestExists].
// GUC bound from req.TenantID() (ADR 0062).
func (r *PermissionRequestRepository) Add(ctx context.Context, req *permissionrequest.Request) error {
	if tx, ok := pg.TxFromContext(ctx); ok {
		return r.addOnTx(ctx, tx, req)
	}
	return r.tx.WithinTxPgxTenant(ctx, req.TenantID().String(), func(ctx context.Context, tx pgx.Tx) error {
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

// GetByID satisfies [permissionrequest.Repository]. GUC bound from the
// explicit tenantID parameter (ADR 0062).
func (r *PermissionRequestRepository) GetByID(
	ctx context.Context,
	tenantID tenant.ID,
	id permissionrequest.ID,
) (*permissionrequest.Request, error) {
	var out *permissionrequest.Request
	err := r.tx.WithinTxPgxTenant(ctx, tenantID.String(), func(ctx context.Context, tx pgx.Tx) error {
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

// UpdateByID satisfies [permissionrequest.Repository]. TDL UpdateFn
// pattern: load → updateFn → persist → drain events. GUC bound from
// tenantID (ADR 0062). Approve/Deny/Cancel all route here.
func (r *PermissionRequestRepository) UpdateByID(
	ctx context.Context,
	tenantID tenant.ID,
	id permissionrequest.ID,
	updateFn func(*permissionrequest.Request) (bool, error),
) error {
	// Join an ambient UoW tx when present (e.g. Approve writes both the
	// request decision and the membership grant in the same commit).
	if tx, ok := pg.TxFromContext(ctx); ok {
		return r.updateOnTx(ctx, tx, id, updateFn)
	}
	return r.tx.WithinTxPgxTenant(ctx, tenantID.String(), func(ctx context.Context, tx pgx.Tx) error {
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

// GetPendingForMembership satisfies [permissionrequest.Repository]. GUC
// bound from the explicit tenantID parameter (ADR 0062).
func (r *PermissionRequestRepository) GetPendingForMembership(
	ctx context.Context,
	tenantID tenant.ID,
	m membership.ID,
) ([]*permissionrequest.Request, error) {
	mid, err := uuid.Parse(m.String())
	if err != nil {
		return nil, fmt.Errorf("permreq repo: parse membership id %q: %w", m, err)
	}
	var out []*permissionrequest.Request
	err = r.tx.WithinTxPgxTenant(ctx, tenantID.String(), func(ctx context.Context, tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		rows, err := q.ListPendingPermissionRequestsForMembership(ctx, pgconv.PgUUID(mid))
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
// Keyset-paginated approver queue (ADR 0038). Zero cursor uses a far-
// future sentinel to admit all rows. GUC bound from tenantID (ADR 0062).
func (r *PermissionRequestRepository) ListPendingApprovableBy(
	ctx context.Context,
	tenantID tenant.ID,
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
	err = r.tx.WithinTxPgxTenant(ctx, tenantID.String(), func(ctx context.Context, tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		got, qerr := q.ListPendingPermissionRequestsByApproverPage(ctx, db.ListPendingPermissionRequestsByApproverPageParams{
			ApproverMembershipID: pgconv.PgUUID(approverUUID),
			Column2:              pgconv.PgRequiredTimestamp(beforeAt),
			Column3:              pgconv.PgUUID(beforeID),
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
// keyset-paginated request history (all states). GUC bound from tenantID
// (ADR 0062).
func (r *PermissionRequestRepository) ListByRequester(
	ctx context.Context,
	tenantID tenant.ID,
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
	err = r.tx.WithinTxPgxTenant(ctx, tenantID.String(), func(ctx context.Context, tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		got, qerr := q.ListPermissionRequestsByRequesterPage(ctx, db.ListPermissionRequestsByRequesterPageParams{
			RequesterMembershipID: pgconv.PgUUID(requesterUUID),
			Column2:               pgconv.PgRequiredTimestamp(beforeAt),
			Column3:               pgconv.PgUUID(beforeID),
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
	row, err := q.GetPermissionRequestByID(ctx, pgconv.PgUUID(rid))
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
		ID:                    pgconv.PgUUID(rid),
		TenantID:              pgconv.PgUUID(tid),
		RequesterMembershipID: pgconv.PgUUID(mid),
		PermissionConstant:    req.Permission().Name(),
		DurationDays:          int32(req.DurationDays()), //nolint:gosec // bounded [1,90] by aggregate
		Reason:                req.Reason(),
		CreatedAt:             pgconv.PgRequiredTimestamp(req.CreatedAt()),
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
		approverPg = pgconv.PgUUID(auid)
	}
	decisionReason := pgconv.ZeroToNil(req.DecisionReason())
	grantedPg := pgconv.PgUUIDOrNull(req.GrantedOverrideID())
	err = q.UpdatePermissionRequestDecision(ctx, db.UpdatePermissionRequestDecisionParams{
		ID:                   pgconv.PgUUID(rid),
		State:                string(req.State()),
		ApproverMembershipID: approverPg,
		DecidedAt:            pgconv.PgTimestamp(req.DecidedAt()),
		DecisionReason:       decisionReason,
		GrantedOverrideID:    grantedPg,
		ExpiresAt:            pgconv.PgTimestamp(req.ExpiresAt()),
		UpdatedAt:            pgconv.PgRequiredTimestamp(req.UpdatedAt()),
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
	if a := pgconv.UUIDFromPg(row.ApproverMembershipID); a != uuid.Nil {
		approverID = membership.ID(a.String())
	}
	decisionReason := ""
	if row.DecisionReason != nil {
		decisionReason = *row.DecisionReason
	}
	return permissionrequest.UnmarshalFromDB(permissionrequest.Snapshot{
		ID:                    permissionrequest.ID(pgconv.UUIDFromPg(row.ID).String()),
		TenantID:              tenant.ID(pgconv.UUIDFromPg(row.TenantID).String()),
		RequesterMembershipID: membership.ID(pgconv.UUIDFromPg(row.RequesterMembershipID).String()),
		Permission:            perm,
		DurationDays:          int(row.DurationDays),
		Reason:                row.Reason,
		State:                 permissionrequest.State(row.State),
		ApproverMembershipID:  approverID,
		DecidedAt:             pgconv.TimeFromPg(row.DecidedAt),
		DecisionReason:        decisionReason,
		GrantedOverrideID:     pgconv.UUIDFromPg(row.GrantedOverrideID),
		ExpiresAt:             pgconv.TimeFromPg(row.ExpiresAt),
		CreatedAt:             pgconv.TimeFromPg(row.CreatedAt),
		UpdatedAt:             pgconv.TimeFromPg(row.UpdatedAt),
	}), nil
}

// constraintPermissionRequestsPending names the partial unique index
// (migration 20260523000003) enforcing at-most-one-pending per
// (requester_membership_id, permission_constant).
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

// permReqFirstPageBefore / permReqFirstPageBeforeID are first-page
// sentinels (no cursor): far-future timestamp + all-FF UUID admits all rows.
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
