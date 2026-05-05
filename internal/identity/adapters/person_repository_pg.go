package adapters

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/platform/pg"
)

// PersonRepository is the pgx/sqlc-backed implementation of
// [person.Repository]. Person is a global aggregate (non-RLS table —
// `database.md` "Identity tenant-scoping rules"); writes still run under
// platform scope so the outbox INSERT policy is satisfied. Reads bypass
// the transactor since persons has no policies attached.
type PersonRepository struct {
	pool *pgxpool.Pool
	tx   *pg.Transactor
	q    *Queries
}

// NewPersonRepository wires the repository against a pool + transactor.
func NewPersonRepository(pool *pgxpool.Pool, tx *pg.Transactor) *PersonRepository {
	return &PersonRepository{pool: pool, tx: tx, q: New(pool)}
}

// Add satisfies [person.Repository] — persists a new Person + drains
// CreatedEvent into the outbox in one tx under platform scope.
func (r *PersonRepository) Add(ctx context.Context, p *person.Person) error {
	return r.tx.WithinTx(ctx, pg.TxScopePlatform, func(ctx context.Context, tx pgx.Tx) error {
		return r.AddInTx(ctx, tx, p)
	})
}

// AddInTx persists a new Person under an EXISTING transaction. See the
// TenantRepository.AddInTx godoc for the orchestrator-composition
// rationale (TDL TransactionProvider escape hatch).
func (r *PersonRepository) AddInTx(ctx context.Context, tx pgx.Tx, p *person.Person) error {
	q := r.q.WithTx(tx)
	if err := insertPersonRow(ctx, q, p); err != nil {
		return err
	}
	return drainPersonEvents(ctx, tx, p)
}

// UpdateByID satisfies [person.Repository] — TDL UpdateFn closure pattern
// for ChangePassword + Anonymise + future mutations.
func (r *PersonRepository) UpdateByID(
	ctx context.Context,
	id person.ID,
	updateFn func(*person.Person) (bool, error),
) error {
	return r.tx.WithinTx(ctx, pg.TxScopePlatform, func(ctx context.Context, tx pgx.Tx) error {
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

// ----- Helpers ---------------------------------------------------------------

func loadPerson(ctx context.Context, q *Queries, id person.ID) (*person.Person, error) {
	uid, err := parsePersonID(id)
	if err != nil {
		return nil, err
	}
	row, err := q.GetPersonByID(ctx, pgUUID(uid))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, person.ErrNotFound
		}
		return nil, fmt.Errorf("person repo: get by id: %w", err)
	}
	return rowToPerson(row)
}

func insertPersonRow(ctx context.Context, q *Queries, p *person.Person) error {
	uid, err := parsePersonID(p.ID())
	if err != nil {
		return err
	}
	stampUUID, err := uuid.Parse(p.SecurityStamp().String())
	if err != nil {
		return fmt.Errorf("person repo: parse security_stamp: %w", err)
	}
	err = q.InsertPerson(ctx, InsertPersonParams{
		ID:            pgUUID(uid),
		Email:         p.Email().String(),
		FirstName:     p.FirstName(),
		LastName:      p.LastName(),
		PasswordHash:  p.PasswordHash().String(),
		SecurityStamp: pgUUID(stampUUID),
		IsActive:      p.IsActive(),
		IsAnonymised:  p.IsAnonymised(),
		CreatedAt:     pgRequiredTimestamp(p.CreatedAt()),
	})
	if err != nil {
		if isUniqueViolation(err) {
			return person.ErrEmailTaken
		}
		return fmt.Errorf("person repo: insert: %w", err)
	}
	return nil
}

func persistPerson(ctx context.Context, q *Queries, p *person.Person) error {
	uid, err := parsePersonID(p.ID())
	if err != nil {
		return err
	}
	stampUUID, err := uuid.Parse(p.SecurityStamp().String())
	if err != nil {
		return fmt.Errorf("person repo: parse security_stamp: %w", err)
	}
	err = q.UpdatePerson(ctx, UpdatePersonParams{
		ID:            pgUUID(uid),
		Email:         p.Email().String(),
		FirstName:     p.FirstName(),
		LastName:      p.LastName(),
		PasswordHash:  p.PasswordHash().String(),
		SecurityStamp: pgUUID(stampUUID),
		IsActive:      p.IsActive(),
		IsAnonymised:  p.IsAnonymised(),
		AnonymisedAt:  pgTimestamp(p.AnonymisedAt()),
	})
	if err != nil {
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

func rowToPerson(row IdentityPerson) (*person.Person, error) {
	id := person.ID(uuidFromPg(row.ID).String())

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
	stamp, err := person.SecurityStampFromString(uuidFromPg(row.SecurityStamp).String())
	if err != nil {
		return nil, fmt.Errorf("person repo: hydrate security_stamp: %w", err)
	}
	return person.UnmarshalFromDB(person.Snapshot{
		ID:            id,
		Email:         addr,
		FirstName:     row.FirstName,
		LastName:      row.LastName,
		PasswordHash:  hash,
		SecurityStamp: stamp,
		IsActive:      row.IsActive,
		IsAnonymised:  row.IsAnonymised,
		CreatedAt:     timeFromPg(row.CreatedAt),
		AnonymisedAt:  timeFromPg(row.AnonymisedAt),
	}), nil
}

func parsePersonID(id person.ID) (uuid.UUID, error) {
	parsed, err := uuid.Parse(id.String())
	if err != nil {
		return uuid.Nil, fmt.Errorf("person repo: parse id %q: %w", id, err)
	}
	return parsed, nil
}
