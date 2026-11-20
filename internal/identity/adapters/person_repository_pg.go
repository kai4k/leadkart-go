package adapters

import (
	"context"
	"errors"
	"fmt"
	"time"

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

// PersonRepository is the pgx/sqlc-backed [person.Repository]. Person is
// a global aggregate (non-RLS); writes run under platform scope to satisfy
// the outbox INSERT policy.
type PersonRepository struct {
	pool   *pgxpool.Pool
	tx     *pg.Transactor
	q      *db.Queries
	stamps person.StampEvictor
}

// WithStampEvictor makes every committed write that rotates a person's security
// stamp evict the cached stamp synchronously, right after the commit, in this
// process (see [PersonRepository.UpdateByID]). Optional: the outbox subscriber
// that does the same from the worker stays as the backstop.
func (r *PersonRepository) WithStampEvictor(e person.StampEvictor) *PersonRepository {
	r.stamps = e
	return r
}

// evictStampOnCommit schedules the eviction for after the surrounding transaction
// commits (never before: a read in between would refill the cache with the old
// stamp). Best-effort — the TTL and the subscriber bound a failure.
func (r *PersonRepository) evictStampOnCommit(ctx context.Context, id person.ID) {
	if r.stamps == nil {
		return
	}
	pg.OnCommit(ctx, func(ctx context.Context) {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		_ = r.stamps.Invalidate(ctx, id)
	})
}

// NewPersonRepository wires the repository against a pool + transactor.
func NewPersonRepository(pool *pgxpool.Pool, tx *pg.Transactor) *PersonRepository {
	return &PersonRepository{pool: pool, tx: tx, q: db.New(pool)}
}

// Add satisfies [person.Repository]. Joins an ambient UoW tx when present;
// otherwise opens a platform-scoped tx. Drains CreatedEvent into the outbox.
func (r *PersonRepository) Add(ctx context.Context, p *person.Person) error {
	return r.runInTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return r.addOnTx(ctx, tx, p)
	})
}

// addOnTx inserts the person row and drains events. Unexported.
func (r *PersonRepository) addOnTx(ctx context.Context, tx pgx.Tx, p *person.Person) error {
	q := r.q.WithTx(tx)
	if err := insertPersonRow(ctx, q, p); err != nil {
		return err
	}
	return drainPersonEvents(ctx, tx, p)
}

// runInTx joins an ambient UoW tx when present, else opens a platform-scoped
// tx (ADR 0067 Phase-4 UoW-join contract).
func (r *PersonRepository) runInTx(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx) error) error {
	if tx, ok := pg.TxFromContext(ctx); ok {
		return fn(ctx, tx)
	}
	return r.tx.WithinTxPgx(ctx, pg.TxScopePlatform, fn)
}

// UpdateByID satisfies [person.Repository]. TDL UpdateFn pattern.
func (r *PersonRepository) UpdateByID(
	ctx context.Context,
	id person.ID,
	updateFn func(*person.Person) (bool, error),
) error {
	return r.runInTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		p, err := loadPersonForUpdate(ctx, q, id)
		if err != nil {
			return err
		}
		stampBefore := p.SecurityStamp()
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
		if p.SecurityStamp() != stampBefore {
			r.evictStampOnCommit(ctx, id)
		}
		return drainPersonEvents(ctx, tx, p)
	})
}

// reader returns the queries bound to the ambient UoW tx when there is one
// (reads then see the same snapshot as the caller's other facts, ADR 0069),
// else the pool.
func (r *PersonRepository) reader(ctx context.Context) *db.Queries {
	if tx, ok := pg.TxFromContext(ctx); ok {
		return r.q.WithTx(tx)
	}
	return r.q
}

// GetByID satisfies [person.Repository]. Read-only.
func (r *PersonRepository) GetByID(ctx context.Context, id person.ID) (*person.Person, error) {
	return loadPerson(ctx, r.reader(ctx), id)
}

// GetByIDs satisfies [person.Repository]. Batched via WHERE id = ANY($1);
// avoids N round-trips.
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
	rows, err := r.reader(ctx).GetPersonsByIDs(ctx, uids)
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

// UpdateAfterSuccessfulLogin satisfies [person.Repository]: one conditional
// statement (see sql/persons.sql) clears the counters unless a lock is in force.
func (r *PersonRepository) UpdateAfterSuccessfulLogin(ctx context.Context, id person.ID, now, ownLock time.Time) (person.LockoutClear, error) {
	uid, err := parsePersonID(id)
	if err != nil {
		return person.LockoutClear{}, err
	}
	var out person.LockoutClear
	err = r.runInTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		row, err := r.q.WithTx(tx).RegisterPersonSuccessfulLogin(ctx, db.RegisterPersonSuccessfulLoginParams{
			ID:      pgconv.PgUUID(uid),
			Now:     pgconv.PgRequiredTimestamp(now),
			OwnLock: pgconv.PgTimestamp(ownLock),
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return person.ErrNotFound
			}
			return fmt.Errorf("person repo: register successful login: %w", err)
		}
		if !row.Cleared {
			out = person.LockoutClear{LockedUntil: pgconv.TimeFromPg(row.PrevLockedUntil)}
			return nil
		}
		out = person.LockoutClear{Cleared: true}
		// The attempt registered itself (count 1) before it was verified; anything
		// beyond that, or a lock that has expired, is a real unlock.
		if row.PrevFailedCount <= 1 && !row.PrevLockedUntil.Valid {
			return nil
		}
		mapped, err := mapDomainEvents([]any{person.AccountUnlockedEvent{PersonID: id, At: now.UTC()}})
		if err != nil {
			return fmt.Errorf("person repo: map events: %w", err)
		}
		return writeOutboxEvents(ctx, tx, uuid.Nil, mapped)
	})
	if err != nil {
		return person.LockoutClear{}, err
	}
	return out, nil
}

// GetByIDForShare satisfies [person.Repository]: the read takes a SHARE lock, so
// it belongs inside the unit of work whose lifetime the lock should cover.
func (r *PersonRepository) GetByIDForShare(ctx context.Context, id person.ID) (*person.Person, error) {
	uid, err := parsePersonID(id)
	if err != nil {
		return nil, err
	}
	row, err := r.reader(ctx).GetPersonByIDForShare(ctx, pgconv.PgUUID(uid))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, person.ErrNotFound
		}
		return nil, fmt.Errorf("person repo: get by id for share: %w", err)
	}
	return rowToPerson(row)
}

// UpdateAfterFailedLogin satisfies [person.Repository]: one atomic statement counts
// the attempt and applies the lock policy (see sql/persons.sql). The policy
// constants come from the aggregate, so Go and SQL cannot drift apart (an
// integration test replays sequences through both). The attempt that crosses the
// threshold also records [person.AccountLockedEvent] in the same transaction.
func (r *PersonRepository) UpdateAfterFailedLogin(ctx context.Context, id person.ID, now time.Time) (person.LockoutOutcome, error) {
	uid, err := parsePersonID(id)
	if err != nil {
		return person.LockoutOutcome{}, err
	}
	var out person.LockoutOutcome
	err = r.runInTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		row, err := r.q.WithTx(tx).RegisterPersonFailedLogin(ctx, db.RegisterPersonFailedLoginParams{
			ID:         pgconv.PgUUID(uid),
			Now:        pgconv.PgRequiredTimestamp(now),
			WindowSecs: person.LockoutWindow.Seconds(),
			MaxFailed:  person.MaxFailedLogins,
			LockSecs:   person.LockoutDuration.Seconds(),
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return person.ErrNotFound
			}
			return fmt.Errorf("person repo: register failed login: %w", err)
		}
		out = person.LockoutOutcome{
			FailedCount: int(row.FailedLoginCount),
			LockedUntil: pgconv.TimeFromPg(row.LockedUntil),
		}
		prevActive := row.PrevLockedUntil.Valid && row.PrevLockedUntil.Time.After(now)
		out.JustLocked = out.LockedAt(now) && !prevActive
		if !out.JustLocked {
			return nil
		}
		mapped, err := mapDomainEvents([]any{person.AccountLockedEvent{
			PersonID:    id,
			LockedUntil: out.LockedUntil,
			FailedCount: out.FailedCount,
			At:          now,
		}})
		if err != nil {
			return fmt.Errorf("person repo: map events: %w", err)
		}
		return writeOutboxEvents(ctx, tx, uuid.Nil, mapped)
	})
	if err != nil {
		return person.LockoutOutcome{}, err
	}
	return out, nil
}

// GetByEmail satisfies [person.Repository]. Looks up by globally-unique email.
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
// lookup for the confirm-password-reset flow.
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
// lookup for the confirm-email-change flow.
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

// loadPerson reads the person without locking it (read paths).
func loadPerson(ctx context.Context, q *db.Queries, id person.ID) (*person.Person, error) {
	return loadPersonWith(ctx, q, id, false)
}

// loadPersonForUpdate reads the person under the row lock (writers: UpdateByID),
// so every writer of one person serialises and persists a snapshot read after
// the previous writer committed.
func loadPersonForUpdate(ctx context.Context, q *db.Queries, id person.ID) (*person.Person, error) {
	return loadPersonWith(ctx, q, id, true)
}

func loadPersonWith(ctx context.Context, q *db.Queries, id person.ID, forUpdate bool) (*person.Person, error) {
	uid, err := parsePersonID(id)
	if err != nil {
		return nil, err
	}
	var row db.IdentityPerson
	if forUpdate {
		row, err = q.GetPersonByIDForUpdate(ctx, pgconv.PgUUID(uid))
	} else {
		row, err = q.GetPersonByID(ctx, pgconv.PgUUID(uid))
	}
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

// applyPendingResetTo and applyPendingEmailChangeTo project the Person's
// pending sub-states onto the params struct. Zero values map to NULL;
// shared by Insert + Update params so projection rules stay in one place.
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

// drainPersonEvents maps Person aggregate events to V1 integration events
// and writes them to the outbox. Person is global; outbox row carries
// uuid.Nil (RLS passes under TxScopePlatform).
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
		// Anonymised rows store an empty hash by design; all others must round-trip.
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
