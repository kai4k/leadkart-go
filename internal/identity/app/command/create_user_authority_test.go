package command_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/common/slug"
	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/domain/authority"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person/persontest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role/roletest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant/tenanttest"
)

// Owner ruling 2026-11-21: "SuperAdmin and Platform Team, Company Admin and
// Company Owner are allowed to add users only." CreateUser decides on the FRESH
// actor, so a stale grant below the Administrator tier cannot be used.

// createUserPersons answers GetByID for the acting members (synthesised) and
// everything else from the fake repository.
type createUserPersons struct{ *persontest.FakeRepository }

func (c createUserPersons) GetByID(ctx context.Context, id person.ID) (*person.Person, error) {
	if p, err := c.FakeRepository.GetByID(ctx, id); err == nil {
		return p, nil
	}
	return anyPersons{}.GetByID(ctx, id)
}

func (c createUserPersons) GetByIDs(ctx context.Context, ids []person.ID) (map[person.ID]*person.Person, error) {
	out := make(map[person.ID]*person.Person, len(ids))
	for _, id := range ids {
		p, err := c.GetByID(ctx, id)
		if err != nil {
			return nil, err
		}
		out[id] = p
	}
	return out, nil
}

func awTenantRepo(t *testing.T) *tenanttest.FakeRepository {
	t.Helper()
	repo := tenanttest.NewFakeRepository()
	s, err := slug.New("authz-world")
	if err != nil {
		t.Fatalf("slug.New: %v", err)
	}
	admin, err := email.New("admin@authz-world.test")
	if err != nil {
		t.Fatalf("email.New: %v", err)
	}
	tn, err := tenant.New(awTenant, s, "Authz World Pharma Pvt Ltd", "Authz", admin, testNow)
	if err != nil {
		t.Fatalf("tenant.New: %v", err)
	}
	tn.PullEvents()
	if err := repo.Add(t.Context(), tn); err != nil {
		t.Fatalf("tenants.Add: %v", err)
	}
	return repo
}

func (w *authzWorld) createUserHandler(t *testing.T) (command.CreateUserHandler, *persontest.FakeRepository) {
	t.Helper()
	persons := persontest.NewFakeRepository()
	h := command.NewCreateUserHandler(fakeUoW{}, createUserPersons{persons}, w.members, awTenantRepo(t), awClock, testNewPersonID, testNewMembershipID).
		WithAuthority(w.lock, w.roles)
	return h, persons
}

func createUserCmd(t *testing.T, to string, a authority.Actor) command.CreateUserCommand {
	t.Helper()
	addr, err := email.New(to)
	if err != nil {
		t.Fatalf("email.New: %v", err)
	}
	return command.CreateUserCommand{TenantID: awTenant, Email: addr, Password: "Sup3r-Secret-Pass!", FirstName: "New", LastName: "Member", Actor: a, CreatedByMembershipID: a.MembershipID()}
}

// stored members the tokens below name.
func (w *authzWorld) storedAt(t *testing.T, r *role.Role, granted ...string) *membership.Membership {
	t.Helper()
	g := make([]membership.GrantedOverride, 0, len(granted))
	for _, n := range granted {
		g = append(g, membership.GrantedOverride{Permission: permission.FromConstant(n)})
	}
	return w.memberSnap(t, membership.Snapshot{RoleAssignments: []role.ID{r.ID()}, GrantedPermissions: g})
}

func TestCreateUser_OnlyTheAdministratorTierAndOperatorsAddUsers(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	create := p.Users.Create
	type row struct {
		name  string
		actor func(t *testing.T, w *authzWorld) authority.Actor
		want  error // nil = the user is created
	}
	token := func(t *testing.T, m *membership.Membership, rank authority.Rank, perms ...string) authority.Actor {
		return actorAs(t, m.ID(), rank, perms...)
	}
	rows := []row{
		{"company owner", func(t *testing.T, w *authzWorld) authority.Actor {
			return token(t, w.storedAt(t, w.owner), authority.RankOwner, tenantAdminEffective()...)
		}, nil},
		{"administrator", func(t *testing.T, w *authzWorld) authority.Actor {
			return token(t, w.storedAt(t, w.admin), authority.RankAdmin, tenantAdminEffective()...)
		}, nil},
		// the stale-data cases: the permission is held by an override (the way a member
		// below the tier was given it before the ruling), the token and the stored row agree
		{"a manager holding it by override", func(t *testing.T, w *authzWorld) authority.Actor {
			return token(t, w.storedAt(t, w.manager, create), authority.RankManager, create)
		}, authority.ErrAdminTierOnly},
		{"a team lead holding it by override", func(t *testing.T, w *authzWorld) authority.Actor {
			return token(t, w.storedAt(t, w.teamLead, create), authority.RankTeamLead, create)
		}, authority.ErrAdminTierOnly},
		{"an executive holding it by override", func(t *testing.T, w *authzWorld) authority.Actor {
			return token(t, w.storedAt(t, w.exec, create), authority.RankUser, create)
		}, authority.ErrAdminTierOnly},
		{"a member with no role holding it by override", func(t *testing.T, w *authzWorld) authority.Actor {
			return token(t, w.memberSnap(t, membership.Snapshot{GrantedPermissions: []membership.GrantedOverride{{Permission: permission.FromConstant(create)}}}), authority.RankNone, create)
		}, authority.ErrAdminTierOnly},
		// a token that says Administrator over a stored executive
		{"a token that outranks the stored member", func(t *testing.T, w *authzWorld) authority.Actor {
			return token(t, w.storedAt(t, w.exec, create), authority.RankAdmin, tenantAdminEffective()...)
		}, authority.ErrAdminTierOnly},
		{"the zero actor", func(*testing.T, *authzWorld) authority.Actor { return authority.Actor{} }, authority.ErrNoAuthority},
		{"a super user", func(t *testing.T, _ *authzWorld) authority.Actor { return superActor(t) }, nil},
		{"a platform operator with platform.users.create", func(t *testing.T, _ *authzWorld) authority.Actor {
			return operatorActor(t, p.Platform.UsersCreate)
		}, nil},
		{"a platform operator with platform.users.manage", func(t *testing.T, _ *authzWorld) authority.Actor {
			return operatorActor(t, p.Platform.UsersManage)
		}, nil},
		{"a platform operator with neither", func(t *testing.T, _ *authzWorld) authority.Actor {
			return operatorActor(t, p.Platform.TenantsView)
		}, authority.ErrNoAuthority},
		{"an impersonation session", func(t *testing.T, _ *authzWorld) authority.Actor {
			a, err := authority.NewActor(authority.ActorInput{MembershipID: membership.ID(awID(9996)), TenantID: awTenant, IsImpersonation: true, EffectivePermissions: tenantAdminEffective()})
			if err != nil {
				t.Fatalf("NewActor: %v", err)
			}
			return a
		}, nil},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			t.Parallel()
			w := newAuthzWorld(t)
			h, persons := w.createUserHandler(t)
			cmd := createUserCmd(t, "new.member@authz-world.test", r.actor(t, w))
			_, err := h.Handle(t.Context(), cmd)
			switch {
			case r.want == nil && err != nil:
				t.Fatalf("Handle: %v, want the user created", err)
			case r.want != nil && !errors.Is(err, r.want):
				t.Fatalf("Handle: %v, want %v", err, r.want)
			}
			_, perr := persons.GetByEmail(t.Context(), cmd.Email)
			if created := perr == nil; created != (r.want == nil) {
				t.Fatalf("person created = %v, want %v (a refusal writes nothing)", created, r.want == nil)
			}
		})
	}
}

// A refusal answers before the tenant is read, the password hashed, or the email
// looked up: a caller below the tier learns nothing about other companies.
func TestCreateUser_ARefusedCallerLearnsNothingAboutTheEmail(t *testing.T) {
	t.Parallel()
	w := newAuthzWorld(t)
	h, persons := w.createUserHandler(t)
	// the email is already an active member elsewhere
	other := newPersonWithPassword(t, "Sup3r-Secret-Pass!")
	if err := persons.Add(t.Context(), other); err != nil {
		t.Fatalf("persons.Add: %v", err)
	}
	if err := w.members.Add(t.Context(), membership.UnmarshalFromDB(membership.Snapshot{ID: membership.ID(awID(8001)), PersonID: other.ID(),
		TenantID: tenant.ID(awID(8002)), Status: membership.StatusActive, JoinedAt: testNow})); err != nil {
		t.Fatalf("members.Add: %v", err)
	}
	mgr := w.storedAt(t, w.manager, permission.IdentityPermissions.Users.Create)
	cmd := command.CreateUserCommand{TenantID: awTenant, Email: other.Email(), Password: "Sup3r-Secret-Pass!", FirstName: "X", LastName: "Y",
		Actor: actorAs(t, mgr.ID(), authority.RankManager, permission.IdentityPermissions.Users.Create)}
	if _, err := h.Handle(t.Context(), cmd); !errors.Is(err, authority.ErrAdminTierOnly) {
		t.Fatalf("Handle: %v, want ErrAdminTierOnly (not email_has_active_membership)", err)
	}
}

func operatorActor(t *testing.T, perms ...string) authority.Actor {
	t.Helper()
	r := authority.RankNone.Int()
	a, err := authority.NewActor(authority.ActorInput{MembershipID: membership.ID(awID(9995)), TenantID: tenant.ID(awID(9994)), Rank: &r, IsPlatformOperator: true, EffectivePermissions: perms})
	if err != nil {
		t.Fatalf("NewActor: %v", err)
	}
	return a
}

// newCreateUserHandlerT is the handler the tests that are not about authority
// use: wired with a no-op lock and an empty role store (their callers are super
// users, who are not re-read).
func newCreateUserHandlerT(uow pg.UnitOfWork, persons person.Repository, members membership.Repository, tenants tenant.Repository,
	now func() time.Time, newPersonID func() person.ID, newMembershipID func() membership.ID) command.CreateUserHandler {
	return command.NewCreateUserHandler(uow, persons, members, tenants, now, newPersonID, newMembershipID).
		WithAuthority(&fakeAuthorityLock{}, roletest.NewFakeRepository())
}
