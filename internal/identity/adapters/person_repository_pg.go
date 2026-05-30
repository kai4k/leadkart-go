package adapters

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/common/pgconv"
	"github.com/leadkart/leadkart-go/internal/identity/adapters/db"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
)

// PersonRepository is the pgx/sqlc-backed implementation of
// [person.Repository]. Person is a global aggregate (non-RLS table —
// `database.md` "Identity tenant-scoping rules"); writes still run under
// platform scope so the outbox INSERT policy is satisfied. Reads bypass
// the transactor since persons has no policies attached.
type PersonRepository struct {
	pool *pgxpool.Pool
	tx   *pg.Transactor
	q    *db.Queries
}

// NewPersonRepository wires the repository against a pool + transactor.
func NewPersonRepository(pool *pgxpool.Pool, tx *pg.Transactor) *PersonRepository {
	return &PersonRepository{pool: pool, tx: tx, q: db.New(pool)}
}

// Add satisfies [person.Repository] — persists a new Person + drains
// CreatedEvent into the outbox. When ctx carries an active tx (a
// parent [pg.UnitOfWork] is in flight), Add joins that tx rather than
// opening its own — canonical multi-aggregate composition path.
func (r *PersonRepository) Add(ctx context.Context, p *person.Person) error {
	return r.runInTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return r.addOnTx(ctx, tx, p)
	})
}

// addOnTx persists the aggregate against the supplied tx + drains
// events to the outbox. Unexported.
func (r *PersonRepository) addOnTx(ctx context.Context, tx pgx.Tx, p *person.Person) error {
	q := r.q.WithTx(tx)
	if err := insertPersonRow(ctx, q, p); err != nil {
		return err
	}
	return drainPersonEvents(ctx, tx, p)
}

// runInTx joins an ambient UoW tx when present (the transactional inbox or
// a composed multi-aggregate write), else opens its own platform-scoped tx.
// Mutators route through it so an inbox-wrapped / composed write commits in
// the SAME tx (ADR 0067 Phase-4 UoW-join contract).
func (r *PersonRepository) runInTx(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx) error) error {
	if tx, ok := pg.TxFromContext(ctx); ok {
		return fn(ctx, tx)
	}
	return r.tx.WithinTxPgx(ctx, pg.TxScopePlatform, fn)
}

// UpdateByID satisfies [person.Repository] — TDL UpdateFn closure pattern
// for ChangePassword + Anonymise + future mutations.
func (r *PersonRepository) UpdateByID(
	ctx context.Context,
	id person.ID,
	updateFn func(*person.Person) (bool, error),
) error {
	return r.runInTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		p, err := loadPerson(ctx, q, id)
		if err != nil {
			return err
		}
		shouldPersist, err := updateFn(p)
		if err != nil {
			return err
		}
		if !shouldPersist {
			return nil
		}
		if err := persistPerson(ctx, q, p); err != nil {
			return err
		}
		return drainPersonEvents(ctx, tx, p)
	})
}

// GetByID satisfies [person.Repository]. Read-only.
func (r *PersonRepository) GetByID(ctx context.Context, id person.ID) (*person.Person, error) {
	return loadPerson(ctx, r.q, id)
}

// GetByIDs satisfies [person.Repository] — batched hydration. Single
// query via `WHERE id = ANY(...)` instead of N round-trips per loop
// iteration. Brandur "What I learned running Postgres at scale" —
// batching is the canonical N+1 fix.
func (r *PersonRepository) GetByIDs(ctx context.Context, ids []person.ID) (map[person.ID]*person.Person, error) {
	if len(ids) == 0 {
		return map[person.ID]*person.Person{}, nil
	}
	uids := make([]pgtype.UUID, 0, len(ids))
	for _, id := range ids {
		u, err := parsePersonID(id)
		if err != nil {
			return nil, err
		}
		uids = append(uids, pgconv.PgUUID(u))
	}
	rows, err := r.q.GetPersonsByIDs(ctx, uids)
	if err != nil {
		return nil, fmt.Errorf("person repo: get by ids: %w", err)
	}
	out := make(map[person.ID]*person.Person, len(ids))
	for _, row := range rows {
		p, err := rowToPerson(row)
		if err != nil {
			return nil, err
		}
		out[p.ID()] = p
	}
	return out, nil
}

// UpdateLockoutState satisfies [person.Repository]. Hot-path direct
// update for the Login flow's wrong-password + lockout-clear branches
// per Wave 9.2 (lockout). Touches ONLY the four lockout columns +
// drains any events the aggregate recorded (PersonAccountLockedEvent
// on threshold crossing; PersonAccountUnlockedEvent on success-after-
// failure). Cheaper than UpdateByID's full-aggregate UPDATE on every
// failed-login attempt — high frequency under brute-force.
//
// Runs under TxScopePlatform (persons is non-RLS but outbox is, and
// the V1 events are platform-scoped); joins a parent UoW tx via
// pg.TxFromContext when one is in flight.
func (r *PersonRepository) UpdateLockoutState(ctx context.Context, p *person.Person) error {
	if tx, ok := pg.TxFromContext(ctx); ok {
		return r.updateLockoutOnTx(ctx, tx, p)
	}
	return r.tx.WithinTxPgx(ctx, pg.TxScopePlatform, func(ctx context.Context, tx pgx.Tx) error {
		return r.updateLockoutOnTx(ctx, tx, p)
	})
}

func (r *PersonRepository) updateLockoutOnTx(ctx context.Context, tx pgx.Tx, p *person.Person) error {
	q := r.q.WithTx(tx)
	uid, err := parsePersonID(p.ID())
	if err != nil {
		return err
	}
	params := db.UpdatePersonLockoutStateParams{
		ID:                pgconv.PgUUID(uid),
		FailedLoginCount:  int32(p.FailedLoginCount()), //nolint:gosec // bounded by MaxFailedLogins (=10)
		LockedUntil:       pgconv.PgTimestamp(p.LockedUntil()),
		LastFailedLoginAt: pgconv.PgTimestamp(p.LastFailedLoginAt()),
	}
	if err := q.UpdatePersonLockoutState(ctx, params); err != nil {
		return fmt.Errorf("person repo: update lockout state: %w", err)
	}
	return drainPersonEvents(ctx, tx, p)
}

// GetByEmail satisfies [person.Repository]. Cross-tenant lookup by
// globally-unique email; consumed by login flow + password-reset.
func (r *PersonRepository) GetByEmail(ctx context.Context, e email.Address) (*person.Person, error) {
	row, err := r.q.GetPersonByEmail(ctx, e.String())
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, person.ErrNotFound
		}
		return nil, fmt.Errorf("person repo: get by email: %w", err)
	}
	return rowToPerson(row)
}

// GetByPasswordResetTokenHash satisfies [person.Repository]. Hash-only
// lookup powering the confirm-password-reset flow. Backed by partial
// unique index uq_persons_password_reset_hash.
func (r *PersonRepository) GetByPasswordResetTokenHash(ctx context.Context, hash person.PasswordResetTokenHash) (*person.Person, error) {
	if hash.IsZero() {
		return nil, person.ErrNotFound
	}
	hashStr := hash.String()
	row, err := r.q.GetPersonByPasswordResetTokenHash(ctx, &hashStr)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, person.ErrNotFound
		}
		return nil, fmt.Errorf("person repo: get by password_reset_token_hash: %w", err)
	}
	return rowToPerson(row)
}

// GetByEmailChangeTokenHash satisfies [person.Repository]. Hash-only
// lookup powering the confirm-email-change flow. Backed by partial
// unique index uq_persons_email_change_hash.
func (r *PersonRepository) GetByEmailChangeTokenHash(ctx context.Context, hash person.EmailChangeTokenHash) (*person.Person, error) {
	if hash.IsZero() {
		return nil, person.ErrNotFound
	}
	hashStr := hash.String()
	row, err := r.q.GetPersonByEmailChangeTokenHash(ctx, &hashStr)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, person.ErrNotFound
		}
		return nil, fmt.Errorf("person repo: get by email_change_token_hash: %w", err)
	}
	return rowToPerson(row)
}

// ----- Helpers ---------------------------------------------------------------

func loadPerson(ctx context.Context, q *db.Queries, id person.ID) (*person.Person, error) {
	uid, err := parsePersonID(id)
	if err != nil {
		return nil, err
	}
	row, err := q.GetPersonByID(ctx, pgconv.PgUUID(uid))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, person.ErrNotFound
		}
		return nil, fmt.Errorf("person repo: get by id: %w", err)
	}
	return rowToPerson(row)
}

func insertPersonRow(ctx context.Context, q *db.Queries, p *person.Person) error {
	uid, err := parsePersonID(p.ID())
	if err != nil {
		return err
	}
	stampUUID, err := uuid.Parse(p.SecurityStamp().String())
	if err != nil {
		return fmt.Errorf("person repo: parse security_stamp: %w", err)
	}
	params := db.InsertPersonParams{
		ID:                     pgconv.PgUUID(uid),
		Email:                  p.Email().String(),
		FirstName:              p.FirstName(),
		LastName:               p.LastName(),
		PasswordHash:           p.PasswordHash().String(),
		SecurityStamp:          pgconv.PgUUID(stampUUID),
		IsActive:               p.IsActive(),
		IsAnonymised:           p.IsAnonymised(),
		CreatedAt:              pgconv.PgRequiredTimestamp(p.CreatedAt()),
		IsGloballySuspended:    p.IsGloballySuspended(),
		GlobalSuspensionReason: p.GlobalSuspensionReason(),
		GloballySuspendedAt:    pgconv.PgTimestamp(p.GloballySuspendedAt()),
		MustChangePassword:     p.MustChangePassword(),
		FailedLoginCount:       int32(p.FailedLoginCount()), //nolint:gosec // bounded by MaxFailedLogins (=10)
		LockedUntil:            pgconv.PgTimestamp(p.LockedUntil()),
		LastFailedLoginAt:      pgconv.PgTimestamp(p.LastFailedLoginAt()),
	}
	applyPendingResetTo(&params, p.PendingPasswordReset())
	applyPendingEmailChangeTo(&params, p.PendingEmailChange())
	if err := q.InsertPerson(ctx, params); err != nil {
		if isUniqueViolation(err) {
			return person.ErrEmailTaken
		}
		return fmt.Errorf("person repo: insert: %w", err)
	}
	return nil
}

// applyPendingResetTo / applyPendingEmailChangeTo project the Person's
// pending sub-states onto the params struct. Zero values map to NULL
// columns; the partial unique indexes only see the populated rows.
//
// Implemented as helpers shared by Insert + Update params so the column
// projection rules live in one place — drift between create + update
// would otherwise corrupt round-trip semantics silently.
func applyPendingResetTo(p *db.InsertPersonParams, pr person.PendingPasswordReset) {
	if pr.IsZero() {
		return
	}
	hash := pr.Hash().String()
	p.PasswordResetTokenHash = &hash
	p.PasswordResetExpiresAt = pgconv.PgTimestamp(pr.ExpiresAt())
}

func applyPendingEmailChangeTo(p *db.InsertPersonParams, ec person.PendingEmailChange) {
	if ec.IsZero() {
		return
	}
	hash := ec.Hash().String()
	newEmail := ec.NewEmail().String()
	p.PendingEmailChangeNewEmail = &newEmail
	p.PendingEmailChangeTokenHash = &hash
	p.PendingEmailChangeExpiresAt = pgconv.PgTimestamp(ec.ExpiresAt())
}

func applyPendingResetToUpdate(p *db.UpdatePersonParams, pr person.PendingPasswordReset) {
	if pr.IsZero() {
		return
	}
	hash := pr.Hash().String()
	p.PasswordResetTokenHash = &hash
	p.PasswordResetExpiresAt = pgconv.PgTimestamp(pr.ExpiresAt())
}

func applyPendingEmailChangeToUpdate(p *db.UpdatePersonParams, ec person.PendingEmailChange) {
	if ec.IsZero() {
		return
	}
	hash := ec.Hash().String()
	newEmail := ec.NewEmail().String()
	p.PendingEmailChangeNewEmail = &newEmail
	p.PendingEmailChangeTokenHash = &hash
	p.PendingEmailChangeExpiresAt = pgconv.PgTimestamp(ec.ExpiresAt())
}

func persistPerson(ctx context.Context, q *db.Queries, p *person.Person) error {
	uid, err := parsePersonID(p.ID())
	if err != nil {
		return err
	}
	stampUUID, err := uuid.Parse(p.SecurityStamp().String())
	if err != nil {
		return fmt.Errorf("person repo: parse security_stamp: %w", err)
	}
	params := db.UpdatePersonParams{
		ID:                     pgconv.PgUUID(uid),
		Email:                  p.Email().String(),
		FirstName:              p.FirstName(),
		LastName:               p.LastName(),
		PasswordHash:           p.PasswordHash().String(),
		SecurityStamp:          pgconv.PgUUID(stampUUID),
		IsActive:               p.IsActive(),
		IsAnonymised:           p.IsAnonymised(),
		AnonymisedAt:           pgconv.PgTimestamp(p.AnonymisedAt()),
		IsGloballySuspended:    p.IsGloballySuspended(),
		GlobalSuspensionReason: p.GlobalSuspensionReason(),
		GloballySuspendedAt:    pgconv.PgTimestamp(p.GloballySuspendedAt()),
		MustChangePassword:     p.MustChangePassword(),
		FailedLoginCount:       int32(p.FailedLoginCount()), //nolint:gosec // bounded by MaxFailedLogins (=10)
		LockedUntil:            pgconv.PgTimestamp(p.LockedUntil()),
		LastFailedLoginAt:      pgconv.PgTimestamp(p.LastFailedLoginAt()),
	}
	applyPendingResetToUpdate(&params, p.PendingPasswordReset())
	applyPendingEmailChangeToUpdate(&params, p.PendingEmailChange())
	if err := q.UpdatePerson(ctx, params); err != nil {
		return fmt.Errorf("person repo: update: %w", err)
	}
	return nil
}

// drainPersonEvents maps Person aggregate events through
// integrationevents.FromDomainEvent and writes the resulting V1
// records to the outbox. Person is global (non-tenant), so the
// outbox row carries uuid.Nil — RLS WITH CHECK passes under
// TxScopePlatform.
//
// When the platform-tenant concept materialises (cross-tenant
// operator audit), swap uuid.Nil for that tenant's UUID.
func drainPersonEvents(ctx context.Context, tx pgx.Tx, p *person.Person) error {
	evs := p.PullEvents()
	if len(evs) == 0 {
		return nil
	}
	asAny := make([]any, len(evs))
	for i, e := range evs {
		asAny[i] = e
	}
	mapped, err := mapDomainEvents(asAny)
	if err != nil {
		return fmt.Errorf("person repo: map events: %w", err)
	}
	return writeOutboxEvents(ctx, tx, uuid.Nil, mapped)
}

func rowToPerson(row db.IdentityPerson) (*person.Person, error) {
	id := person.ID(pgconv.UUIDFromPg(row.ID).String())

	addr, err := email.New(row.Email)
	if err != nil {
		return nil, fmt.Errorf("person repo: hydrate email %q: %w", row.Email, err)
	}
	hash, err := person.NewPasswordHash(row.PasswordHash)
	if err != nil && !row.IsAnonymised {
		// Anonymised rows store empty hash by design; everything else
		// must round-trip a valid hash.
		return nil, fmt.Errorf("person repo: hydrate password_hash: %w", err)
	}
	stamp, err := person.SecurityStampFromString(pgconv.UUIDFromPg(row.SecurityStamp).String())
	if err != nil {
		return nil, fmt.Errorf("person repo: hydrate security_stamp: %w", err)
	}
	snap := person.Snapshot{
		ID:                     id,
		Email:                  addr,
		FirstName:              row.FirstName,
		LastName:               row.LastName,
		PasswordHash:           hash,
		SecurityStamp:          stamp,
		IsActive:               row.IsActive,
		IsAnonymised:           row.IsAnonymised,
		IsGloballySuspended:    row.IsGloballySuspended,
		GlobalSuspensionReason: row.GlobalSuspensionReason,
		GloballySuspendedAt:    pgconv.TimeFromPg(row.GloballySuspendedAt),
		MustChangePassword:     row.MustChangePassword,
		FailedLoginCount:       int(row.FailedLoginCount),
		LockedUntil:            pgconv.TimeFromPg(row.LockedUntil),
		LastFailedLoginAt:      pgconv.TimeFromPg(row.LastFailedLoginAt),
		CreatedAt:              pgconv.TimeFromPg(row.CreatedAt),
		AnonymisedAt:           pgconv.TimeFromPg(row.AnonymisedAt),
	}
	if row.PasswordResetTokenHash != nil {
		resetHash, herr := person.NewPasswordResetTokenHash(*row.PasswordResetTokenHash)
		if herr != nil {
			return nil, fmt.Errorf("person repo: hydrate password_reset_token_hash: %w", herr)
		}
		snap.PasswordResetTokenHash = resetHash
		snap.PasswordResetExpiresAt = pgconv.TimeFromPg(row.PasswordResetExpiresAt)
	}
	if row.PendingEmailChangeTokenHash != nil && row.PendingEmailChangeNewEmail != nil {
		newAddr, eerr := email.New(*row.PendingEmailChangeNewEmail)
		if eerr != nil {
			return nil, fmt.Errorf("person repo: hydrate pending_email_change_new_email: %w", eerr)
		}
		ecHash, herr := person.NewEmailChangeTokenHash(*row.PendingEmailChangeTokenHash)
		if herr != nil {
			return nil, fmt.Errorf("person repo: hydrate pending_email_change_token_hash: %w", herr)
		}
		snap.PendingEmailChangeNewEmail = newAddr
		snap.PendingEmailChangeHash = ecHash
		snap.PendingEmailChangeExpiresAt = pgconv.TimeFromPg(row.PendingEmailChangeExpiresAt)
	}
	return person.UnmarshalFromDB(snap), nil
}

func parsePersonID(id person.ID) (uuid.UUID, error) {
	parsed, err := uuid.Parse(id.String())
	if err != nil {
		return uuid.Nil, fmt.Errorf("person repo: parse id %q: %w", id, err)
	}
	return parsed, nil
}
