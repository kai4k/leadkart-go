# LeadKart-Go — Rank hierarchy and grant authority: implementation plan (addendum to 2026-11-18)

**Goal.** Close every finding from the two security reviews of `fix/identity-authz-and-bff-security`, and implement the owner's ruling: *"Owners above admin above manager above team lead above users … admin sits above all; if a manager is not there, admin can see through everything and can approve everything."*

The findings:
1. D4 ("no grant beyond your authority") is enforced only in the HTTP port, outside the transaction.
2. The permission-request approval path ignores D4 and uses `c.IsPlatform`.
3. Removals are unbounded: override `Revoked` lists, role revoke, role-permission revoke and replace. The CompanyOwner role is mutable, and there is no last-owner protection (plan Q6).
4. `PUT /users/{id}/manager` makes someone an approver but only needs `identity.users.update`.
5. Role replace checks the whole new set instead of the delta.
6. The coordinator's re-review:
   - body ids reach the domain uncanonicalised;
   - `PATCH /roles/{id}/parent` has no authority check;
   - operator authority under `X-Tenant-Id`;
   - "set self as manager, then approve";
   - slug-route inline checks and the missing person-route self rule;
   - dead `RequireTenantContext` and the byte-equal `sameTenant` fallback;
   - the actor's tenant must come from claims.

**Architecture.**
- **One value object.** `authority.Actor` (new pure domain package `internal/identity/domain/authority`) is built **once** in the port by `authn.AuthorityFrom(ctx)` from verified **claims**. It is carried in every authority-changing command struct (tdl_canon §4/§5/§11). The zero value denies every decision.
- **One policy.** Pure methods on `Actor` plus two guard functions decide:
  - may A act on member T;
  - may A confer or remove permission set P;
  - may A assign or revoke role R;
  - may A create or edit role R (level, delta, parent);
  - may A set or clear T's manager;
  - may A approve or deny request Q;
  - the last-owner and owner-holds-`tenant.admin` invariants.
- **Where the checks run.** Each command evaluates the policy inside its transaction, after taking a **per-tenant transaction-scoped advisory lock** (`AuthorityLock`). The facts the decision reads cannot change before the write commits. This closes every check-then-act race, including last-owner.
- **Errors.** Typed domain sentinels are mapped once in the port (`writeAuthorityError`).
- **What goes away.** The port-level `authority.go` checks and the `IsPlatformOperator bool` command fields are deleted. There is no duplicate enforcement.
- **Seed and migration.** The catalogue gets the tier ladder plus the Manager/Team Lead daily-work grants. One goose data migration updates existing tenants' **pristine** seed roles only.

**Tech stack.** Same as the parent plan: Go 1.26.4, stdlib ServeMux, pgx/sqlc (`sqlc` is installed at `~/go/bin/sqlc`), goose (`~/go/bin/goose`), testcontainers Postgres 17 (`//go:build integration`), fakes not mocks (ADR 0062).

## Global constraints

1. **Prerequisite.** Execute after the parent plan `docs/plans/2026-11-18-identity-authz-and-bff-security.md` has landed at least through **Task 8**:
   - `authn` is being edited for Tasks 6–8;
   - Task 7 adds `MustChangePassword` to `jwt.Claims`, `IssueArgs`, `login.go` and `refresh.go`;
   - Task 8 adds `RequireSelfService` on the permission-request mutations.
   It also requires the coordinator's small "canonicalise body ids in handlers" fix. Run `git log --oneline -15` and confirm. If Task 6 (X-Tenant-Id) has not landed, tasks A3 and A4 still work: the rules below never read the context tenant for the actor. Plan against **behaviour**, not line numbers.
2. **Doctrine, verbatim from the parent plan's Global constraints 1:**
   - Before touching `domain/`, `app/`, `ports/` or `adapters/`, read `CLAUDE.md` and `docs/doctrine/tdl_canon.md` and invoke the **tdl-discipline** skill. Before every commit, invoke **verify-green**.
   - Interfaces live with their consumer.
   - No cross-module imports, except the existing shared kernel `internal/identity/ports/authn` (allow-listed in `eda_arch_test.go`).
   - `app/` never calls `tenancy.FromContext`; tenant and actor values travel in the command struct.
   - Aggregates expose behaviour only. No public setters, no `Save()`, no business-verb repo methods.
   - Fakes, not mocks.
   - No `time.Sleep` in tests.
   - OpenAPI `api/openapi.yaml` is updated for every contract change (ADR 0046/0050). The identity route↔spec gate `TestArch_RouteHasSpecOperation` must stay green.
   - When a gate goes red, fix the code. Never weaken the gate or add an allowlist entry to silence it.
3. **Docker is available.** Docker Desktop with the linux engine. Integration tests use testcontainers and need serial package execution, `-p 1`.
4. **TDD per step.** Write the failing test, run it and see it fail, write the minimum code, run it and see it pass. **One commit per task.**
5. **Per-task gate, verbatim from the parent plan.** Run in Git Bash from `D:/Development/leadkart-go`, read every exit code, never infer one:
   ```
   G1  gofmt -l $(git diff --name-only HEAD -- '*.go')            # no output
   G2  go build ./... ; echo BUILD=$? ; go build -tags=integration ./... ; echo BUILDINT=$?   # 0 / 0
   G3  go vet ./... ; echo VET=$?                                  # 0
   G4  task test ; echo UNIT=$?                                    # 0, no "--- FAIL"
   G5  go clean -testcache ; task test:arch ; echo ARCH=$?         # 0
   G6  task test:int:compile ; echo INTC=$?                        # 0
   G7  go test -tags=integration -p 1 -count=1 -timeout=15m <focused pkgs> ; echo INT=$?   # 0
   ```
   - `task lint` and `task ci:openapi` are optional: run them if the tool is installed and report the result.
   - The full `task test:int` runs after **A5** and after **A8**.
6. **Commits.**
   - Stage explicit paths with `git add <paths>`, never `-A`.
   - The gate run and the commit are separate steps.
   - The message states the verified markers.
   - Use a conventional subject and end the message with: `Co-Authored-By: Claude Fable 5.2 <noreply@anthropic.com>`
   - **Never push.** Branch: `fix/identity-authz-and-bff-security`.
7. **Ids.** The policy compares only ids of **loaded aggregates** (canonical, from the DB) and claims ids (minted from the DB). It never compares raw request strings. Body/path ids are canonicalised with `membership.ParseID` / `role.ParseID` (A1) before they reach a command.

## Decisions

The owner's H1–H6 are refined below. Every refinement is marked **[R]**; the owner should confirm them.

- **H1 – Rank and tiers.** A member's **rank** is the numerically lowest `HierarchyLevel` among their **live** roles. With no live role the rank is `RankNone = 99`.
  - Rank comes from roles only. Overrides never change rank. ADR 0036 keeps permissions-first; rank is a separate seniority axis.
  - Tiers are bands, not exact values: `owner` = 0, `admin` = 1–10, `manager` = 11–20, `team_lead` = 21–30, `user` = 31–99. BRD §6.7 says "gaps of 10 let custom roles slot in".
  - Seeded levels (BRD §6.7, reconciled with the ruling):

    | Role | Level | Tier |
    |---|---|---|
    | Company Owner | 0 | owner |
    | Administrator | 10 | admin |
    | Office Administrator | 10 | admin |
    | Senior Manager | **15** | manager (outranks department managers) |
    | Sales / Purchase / Dispatch / HR Manager | **20** | manager |
    | **Team Lead** (new seeded role) | **30** | team_lead |
    | All Executives | 50 | user |

  - **[R1]** Senior Manager moves 20→15 so it outranks department managers. The BRD has managers at 30 and executives at 40; the ruling's ladder overrides that. A new seeded "Team Lead" role brings the catalogue to 14 roles, a deviation from the .NET 13.
  - **[R2]** Level 0 is reserved to system-default roles; it is enforced in the `role` aggregate (H6). So Owner tier ≡ holder of the CompanyOwner system role.
- **H2 – Act only downward.** Assign/revoke role, overrides, deactivate/reactivate and set/clear manager need `actor.rank < target.rank` (strict). Exceptions:
  - an owner may act on another owner;
  - super users are exempt;
  - **exempt platform operators** are exempt.
  - **[R3]** *Exempt platform operator* means:
    - slug-anchored and non-impersonation (`isPlatformOperator`);
    - **and** holds `platform.users.manage` or `platform.roles.manage` in their own claims.
    The actor's tenant and effective permissions come from **claims** (the caller's own tenant), never from the re-bound context tenant (finding 7). An operator acting in tenant B via `X-Tenant-Id` therefore has no tenant-B rank; they are rank-exempt instead. Their writes are audited by the D7 scope log line plus the mutation's outbox events (ADR 0027/0039).
  - Targets holding an `is_super_admin` role are changeable only by super users.
  - Self-target is denied for non-super actors. This is D3: the policy re-states it so the domain is total; the route `DenySelfTarget` stays as the cheap early gate. **[R4]**
  - **Impersonation rank = Admin-equivalent (10).** NewActor forces it whatever the claim says. ADR 0045 gives the operator `tenant.admin` *reach*, but a support session must not change tenant *ownership*. At rank 10 it:
    - cannot act on owners;
    - cannot assign Administrator or CompanyOwner;
    - cannot confer `tenant.admin`;
    - can never remove the last owner.
    Ownership repair goes through the exempt operator path, which is non-impersonation and audited.
- **H3 – Grant ceiling (D4, now symmetric and diff-based).**
  - An actor may **confer or remove** only permissions it holds, and only the **delta** is checked:
    - role replace: `Delta(current, new)`;
    - override replace: `Delta(grantedBefore, grantedAfter) ∪ Delta(revokedBefore, revokedAfter)`, where "granted before" means live (unexpired) grants;
    - role grant or revoke of P: `[P]`, only if it changes anything;
    - role assign/revoke: the role's whole permission set;
    - role delete: the role's whole set, because it is removed from the holders.
  - Role assign/revoke also needs `role.level > actor.rank`; owners may assign any tenant role, including CompanyOwner. Only super users assign or revoke `is_super_admin` roles.
  - Editing a role (rename, level, permissions, parent, delete) needs `actor.rank < role.level`. A new level must satisfy `actor.rank < newLevel`. Creating a role needs `actor.rank < level`.
  - **[R5]** `tenant.admin` is conferrable or removable **only by Owner-tier actors** (or super users / exempt operators). It *defines* the Admin tier, mirroring "only owners appoint Administrators". Without this rule an Admin could make a Manager admin-equivalent through an override.
  - Platform-reserved permissions (`permission.IsPlatformReserved`) are conferrable only by super users.
  - **[R6]** Exempt operators' ceiling is `tenant.admin` ∪ everything `tenant.admin` implies (finding 3). Without it an operator could never repair a locked-out tenant's roles.
- **H4 – Approvals (supersede ADR 0055 §4/§8 approver rule).** A request is **approvable** by any of:
  - **(a)** the requester's current manager (`reports_to`), when the manager outranks the requester, holds the requested permission **and** holds `identity.roles.approve` **[R7]**. Grounds: ADR 0055 §4 already designates `roles.approve` as the approver capability. With it, a manager link alone confers no approval power; the capability itself is D4-conferred. This closes finding 4.
  - **(b)** any member holding `tenant.admin` (Admin or Owner tier) who outranks the requester and holds the permission. Admins always do: `tenant.admin` implies everything requestable.
  - **(c)** a super user or an exempt platform operator.
  - **Never**:
    - impersonation sessions (`impersonation_not_allowed`, both at the route per parent Task 8 and in the policy);
    - the requester themselves.
  - **Deny** uses the same eligibility minus "holds the permission".
  - **Requestable set** = `permission.ImpliedByTenantAdmin(name)`, which excludes `tenant.admin` and every platform-reserved name. `permissionrequest.New` rejects anything else.
  - **Approver queue** (`GET /permission-requests?role=approver`): pending requests whose requester the actor may *deny*. This fixes a real bug: the old query filtered on `approver_membership_id`, which is NULL while a request is pending, so the queue was always empty.
  - **Setting a manager** requires all of:
    - the actor may act on the subordinate (outranks them);
    - the manager is an active member of the same tenant, strictly more senior than the subordinate (no lateral or upward links);
    - the actor is **at least as senior as the proposed manager** (`actor.rank ≤ manager.rank`), so self-appointment is allowed.
  - **Clearing or replacing a manager** needs `actor.rank ≤ currentManager.rank`.
  - **Why `≤` and not `<`:** a manager organising their own team is normal. Self-appointment cannot grow authority, because approval additionally needs `roles.approve` and is capped by the approver's own permissions. The sock-puppet chain yields at most what the attacker already holds.
  - **Cycles:** rank ordering alone cannot prevent them, because ranks change after links exist. A1 adds a canonical-id self check, a DB `CHECK (reports_to <> id)`, and a bounded chain walk in the command (A4). Approval also re-checks "manager outranks requester", so stale upward links are inert.
- **H5 – Manager-level daily-work grants.** These are added to existing grants. Every non-owner role also gets `identity.meta.request_permission_elevation`. The catalogue godoc already says it is held by every regular user, but no role held it.

  | Role | Added grants |
  |---|---|
  | Administrator, Office Administrator | `tenant.admin` (new tenants only; see Q2) |
  | Senior Manager | `identity.users.view`, `identity.roles.view`, `identity.roles.approve`, `crm.leads.{read,read_all,assign,manage}`, `inventory.{catalog,stock}.read`, `dispatch.consignment_notes.read`, orders read set |
  | Sales Manager | users.view, roles.view, roles.approve, `crm.leads.{read,read_all,assign,manage}` (BRD §3.2 "views all CRM data", §4.9 "Sales Manager or above can reassign") |
  | Purchase Manager | users.view, roles.view, roles.approve |
  | Dispatch Manager | users.view, roles.view, roles.approve, `orders.orders.read`, `inventory.stock.read` |
  | HR Manager | users.view, roles.view, roles.approve, `identity.users.update` (BRD "Users + org structure": reporting lines) |
  | Team Lead (new) | users.view, roles.view, roles.approve, `crm.leads.{read,manage}`, `tasks.work_items.{read,manage,reassign}`, inbox |
  | Sales Executive | `crm.leads.{read,manage}` (BRD "manages assigned leads, logs calls") |

  - Directory views: Manager and Team Lead tiers get `identity.users.view` + `identity.roles.view`. The team picker, the approval UI and the role names in the directory need them now that directory reads are gated.
  - Team Lead deliberately gets **no** `*.read_all`: today `read_all` is tenant-wide (see the follow-up feature).
  - **Visibility.** `tenant.admin` already implies every tenant permission, including every `read_all`, so admins see everything. The approver queue and request detail are opened to `tenant.admin` holders (A6). Nothing else is needed.
- **H6 – Protected invariants (domain).**
  - **(i)** System-default roles are **fully immutable**: rename, level, delete and now **permission grant/revoke/replace** are all refused (`role.ErrSystemDefault`). This matches the existing docstring, "tenants must clone them to mutate". So the CompanyOwner role always holds `tenant.admin` at level 0. The seed creates it via `role.NewWithPermissions`.
  - **(ii)** Level 0 is accepted only for system-default roles (`role.New`, `ChangeHierarchyLevel`).
  - **(iii)** **Last owner:** deactivation, or revoking the CompanyOwner role, is refused when no *other* active CompanyOwner holder remains. This applies to every actor, including super users.
  - **(iv)** An override set that revokes `tenant.admin` from a CompanyOwner holder is refused (`owner_protected`).
  - **(v)** Other seeded roles (Administrator … Team Lead … executives) stay ordinary tenant roles. Owners and other seniors can rename, re-level, re-permission or delete them within H3. **[R8]** The ladder cannot be broken *from below*.
  - **(vi)** Role-parent edges (ADR 0058) are **org-chart metadata only**:
    - rank is derived from `HierarchyLevel` alone;
    - the approver chain is the membership `reports_to` link, never the role edge;
    - so an edge can never escalate.
    To keep the two from disagreeing, an edge is accepted only when the parent's level is strictly more senior than the child's, and only by an actor who may edit the child (finding 2).
- **Error codes.**

  | Code | Status | Meaning |
  |---|---|---|
  | `insufficient_rank` | 403 | new |
  | `grant_exceeds_authority` | 403 | existing |
  | `self_modification_forbidden` | 403 | existing |
  | `forbidden` | 403 | zero or cross-tenant actor |
  | `impersonation_not_allowed` | 403 | |
  | `permission_request_forbidden` | 403 | existing; not an eligible approver |
  | `last_owner_protected` | 409 | |
  | `owner_protected` | 409 | |
  | `role_system_default` | 409 | was an unmapped 500 |
  | `manager_not_eligible` | 422 | |
  | `role_parent_not_eligible` | 422 | |
  | `permission_not_requestable` | 422 | |

  - **[R9]** The 409/422 codes are state or input errors, not authorization errors, so they deviate from the brief's "403 codes".
- **Rank delivery.**
  - The actor's rank travels as a new JWT claim `rank`. It is minted by `permissions.Resolver.ResolveAuth` from the same role load as the permission claims, at login and refresh.
  - It is therefore exactly as fresh as `effective_permissions`: at most `AccessTokenTTL` (10 min) stale. Role and permission changes do not rotate the security stamp (ADR 0028); see Q4. The per-request cost is zero.
  - Tokens without the claim (pre-deploy) get `RankNone`, so they fail closed and expire within 10 min.
  - The **target's** rank, roles and the owner set are always loaded fresh inside the locked transaction.
  - Capabilities exposes `rank` (int, lower = more senior) and `tier` (string), computed from the same `AuthorityFrom`.

## Open questions (each with a default)

- **Q1.** Is the [R1]–[R9] refinement set accepted? **Default: yes.**
- **Q2.** Should the migration add `tenant.admin` to existing tenants' pristine Administrator / Office Administrator roles? That would silently escalate their holders. **Default: no.** New tenants get it from the seed; existing owners opt in with `POST /roles/{id}/permissions/grant`.
- **Q3.** Should Office Administrator be Admin tier with `tenant.admin`? BRD §6.7 puts it at 10. **Default: yes for new tenants.**
- **Q4.** Should privileged commands re-resolve the *actor* fresh instead of trusting the 10-minute-stale rank and permission claims? **Default: no.** Claims stay the single source; a demoted actor can still act only below their old rank and never on owners. Follow-up: rotate a per-membership authz version on role/override changes.
- **Q5.** Slug lookup (`GET /tenants?slug=`, `/tenants/by-slug/{slug}`) now requires `identity.tenants.view` in the resolved tenant, so ordinary members lose own-slug lookup. This is consistent with parent Q1. **Default: gate it.**
- **Q6.** Should `CreateUser` carry an Actor? **Default: no change.** A new membership holds no role (rank 99) and confers nothing; all authority over it flows through gated commands.

---

## Task A0 — Preflight (no commit)

1. Run `git status` and expect a clean tree. Run `git branch --show-current` and expect `fix/identity-authz-and-bff-security`.
2. Run `git log --oneline -20`. Confirm parent Tasks 5–8 and the "canonicalise body ids" fix are present. If they are not, **stop and ask.**
3. Run `ls migrations | sort | tail -1`. Expect `20260605000007_notifications_init.sql`, or any version < `20261119000001`. If a later migration exists, renumber A2/A4 above it.
4. Run the baseline G2–G6 and record the markers. A red baseline is a finding; report it and do not fix it here.

---

## Task A1 — Domain: `authority` package, ids, role helpers (H1–H4, finding 1)

**Files:**
- Create `internal/identity/domain/authority/{errors.go,rank.go,member.go,actor.go,policy.go,authority_test.go}`
- Modify `internal/identity/domain/role/role.go`: tier constants, `IsCompanyOwner`, `ParseID`
- Modify `internal/identity/domain/role/system_roles.go`: `TeamLead`
- Modify `internal/identity/domain/membership/membership.go`: `ParseID`; canonical ids in `AssignRole`, `RevokeRole`, `AssignManager`
- Modify `internal/identity/domain/membership/membership_test.go`

**Interfaces produced** (every later task consumes them):
```go
// role
const HierarchyLevelOwner = 0; HierarchyLevelAdmin = 10; HierarchyLevelSeniorManager = 15
const HierarchyLevelManager = 20; HierarchyLevelTeamLead = 30   // Default 50, Max/NoRole 99 unchanged
func ParseID(raw string) (ID, error)
func (r *Role) IsCompanyOwner() bool
// SystemRoles.Tenant.TeamLead = "Team Lead"
// membership
func ParseID(raw string) (ID, error)
// authority
type Rank int; const RankOwner, RankAdmin, RankManager, RankTeamLead, RankUser, RankNone Rank
type Tier string; const TierOwner, TierAdmin, TierManager, TierTeamLead, TierUser Tier
func (r Rank) Tier() Tier; func (r Rank) Int() int; func ClampRank(n int) Rank; func RankOf([]*role.Role) Rank
type Member struct{ /* unexported */ }; func MemberOf(*membership.Membership, []*role.Role) Member
  // ID() TenantID() Rank() IsOwner() IsSuperAdmin() IsActive() ReportsTo()
type ActorInput struct{ MembershipID membership.ID; TenantID tenant.ID; Rank Rank; EffectivePermissions []string; IsSuperUser, IsPlatformOperator, IsImpersonation bool }
type Actor struct{ /* unexported */ }; func NewActor(ActorInput) (Actor, error)
  // Valid() MembershipID() TenantID() Rank() IsSuperUser() IsPlatformOperator() IsImpersonation() Holds(string) bool
  // MayActOn(Member) error; MayChangePermissions([]string) error
  // MayAssignRole(Member,*role.Role) error; MayRevokeRole(Member,*role.Role) error
  // MayCreateRole(level int) error; MayEditRole(*role.Role, newLevel int, delta []string) error
  // MaySetRoleParent(child, parent *role.Role) error   // parent nil = clear
  // MaySetManager(sub, mgr Member) error; MayUnlinkManager(sub, current Member) error
  // MayApprove(requester Member, perm string) error; MayDeny(requester Member) error
const NoLevelChange = -1
func GuardLastOwner(target Member, activeOwners []membership.ID) error
func GuardOwnerOverrides(target Member, revoked []string) error
func Delta(before, after []string) []string
func PermissionNames([]*permission.Permission) []string
var ErrNoAuthority, ErrInsufficientRank, ErrGrantExceedsAuthority, ErrSelfTarget, ErrImpersonationNotAllowed,
    ErrNotApprover, ErrLastOwnerProtected, ErrOwnerProtected, ErrManagerNotEligible, ErrRoleParentNotEligible
```

### Steps

**1. Write the failing test** `internal/identity/domain/authority/authority_test.go`:
```go
package authority_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/authority"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

const (
	tA = tenant.ID("0190a000-0000-7000-8000-00000000000a")
	tB = tenant.ID("0190a000-0000-7000-8000-00000000000b")
	tP = tenant.ID("0190a000-0000-7000-8000-00000000000f")
)

var t0 = time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)

func uid(n int) string { return fmt.Sprintf("0190a000-0000-7000-8000-%012d", n) }

func mkRole(n int, tid tenant.ID, name string, sys bool, level int, super bool, perms ...string) *role.Role {
	ps := make([]*permission.Permission, 0, len(perms))
	for _, p := range perms {
		ps = append(ps, permission.FromConstant(p))
	}
	return role.UnmarshalFromDB(role.Snapshot{ID: role.ID(uid(n)), TenantID: tid, Name: name,
		IsSystemDefault: sys, IsSuperAdmin: super, HierarchyLevel: level, Permissions: ps, CreatedAt: t0})
}

func mkMember(n int, tid tenant.ID, active bool, reportsTo membership.ID, roles ...*role.Role) authority.Member {
	ids := make([]role.ID, 0, len(roles))
	for _, r := range roles {
		ids = append(ids, r.ID())
	}
	status := membership.StatusActive
	if !active {
		status = membership.StatusInactive
	}
	m := membership.UnmarshalFromDB(membership.Snapshot{ID: membership.ID(uid(n)), PersonID: person.ID(uid(n + 500)),
		TenantID: tid, Status: status, JoinedAt: t0, RoleAssignments: ids, ReportsTo: reportsTo})
	return authority.MemberOf(m, roles)
}

func adminEffective() []string {
	out := []string{permission.IdentityPermissions.Meta.TenantAdmin}
	for _, p := range permission.All() {
		if permission.ImpliedByTenantAdmin(p.Name()) {
			out = append(out, p.Name())
		}
	}
	return out
}

func mkActor(t *testing.T, in authority.ActorInput) authority.Actor {
	t.Helper()
	a, err := authority.NewActor(in)
	if err != nil {
		t.Fatalf("NewActor: %v", err)
	}
	return a
}

func TestPolicy_DecisionTable(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	ownerR := mkRole(1, tA, role.SystemRoles.Tenant.CompanyOwner, true, 0, false, p.Meta.TenantAdmin)
	adminR := mkRole(2, tA, role.SystemRoles.Tenant.Administrator, false, 10, false, p.Meta.TenantAdmin)
	mgrR := mkRole(3, tA, role.SystemRoles.Tenant.SalesManager, false, 20, false, p.Crm.Leads.Read, p.Crm.Leads.ReadAll, p.Roles.Approve)
	tlR := mkRole(4, tA, role.SystemRoles.Tenant.TeamLead, false, 30, false, p.Crm.Leads.Read, p.Roles.Approve)
	userR := mkRole(5, tA, role.SystemRoles.Tenant.SalesExecutive, false, 50, false, p.Crm.Leads.Read)
	assignR := mkRole(6, tA, "Lead Router", false, 40, false, p.Crm.Leads.Assign)
	superR := mkRole(7, tA, "SuperAdmin", true, 0, true)

	owner1 := mkMember(11, tA, true, "", ownerR)
	owner2 := mkMember(12, tA, true, "", ownerR)
	admin := mkMember(13, tA, true, "", adminR)
	admin2 := mkMember(14, tA, true, "", adminR)
	mgr := mkMember(15, tA, true, "", mgrR)
	mgr2 := mkMember(16, tA, true, "", mgrR)
	tl := mkMember(17, tA, true, mgr.ID(), tlR)
	user := mkMember(18, tA, true, tl.ID(), userR)
	puppet := mkMember(19, tA, true, mgr.ID())
	staleAdmin := mkMember(20, tA, true, mgr.ID(), adminR) // upward link left over from an old rank
	inactiveMgr := mkMember(21, tA, false, "", mgrR)
	foreign := mkMember(22, tB, true, "", userR)
	superMember := mkMember(23, tA, true, "", superR)

	aOwner := mkActor(t, authority.ActorInput{MembershipID: owner1.ID(), TenantID: tA, Rank: authority.RankOwner, EffectivePermissions: adminEffective()})
	aAdmin := mkActor(t, authority.ActorInput{MembershipID: admin.ID(), TenantID: tA, Rank: authority.RankAdmin, EffectivePermissions: adminEffective()})
	aMgr := mkActor(t, authority.ActorInput{MembershipID: mgr.ID(), TenantID: tA, Rank: authority.RankManager, EffectivePermissions: []string{p.Crm.Leads.Read, p.Crm.Leads.ReadAll, p.Roles.Approve}})
	aMgrNoApprove := mkActor(t, authority.ActorInput{MembershipID: mgr.ID(), TenantID: tA, Rank: authority.RankManager, EffectivePermissions: []string{p.Crm.Leads.Read, p.Crm.Leads.ReadAll}})
	aTL := mkActor(t, authority.ActorInput{MembershipID: tl.ID(), TenantID: tA, Rank: authority.RankTeamLead, EffectivePermissions: []string{p.Crm.Leads.Read, p.Roles.Approve}})
	aUser := mkActor(t, authority.ActorInput{MembershipID: user.ID(), TenantID: tA, Rank: authority.RankUser, EffectivePermissions: []string{p.Crm.Leads.Read}})
	aSuper := mkActor(t, authority.ActorInput{MembershipID: membership.ID(uid(90)), TenantID: tP, Rank: authority.RankOwner, IsSuperUser: true})
	aOp := mkActor(t, authority.ActorInput{MembershipID: membership.ID(uid(91)), TenantID: tP, Rank: authority.RankNone, IsPlatformOperator: true, EffectivePermissions: []string{p.Platform.UsersManage}})
	aOpNoManage := mkActor(t, authority.ActorInput{MembershipID: membership.ID(uid(92)), TenantID: tP, Rank: authority.RankNone, IsPlatformOperator: true, EffectivePermissions: []string{p.Platform.TenantsView}})
	aImp := mkActor(t, authority.ActorInput{MembershipID: membership.ID(uid(93)), TenantID: tA, Rank: authority.RankOwner, IsImpersonation: true, EffectivePermissions: adminEffective()})
	var zero authority.Actor

	cases := []struct {
		name string
		got  error
		want error
	}{
		// H2 — act only downward
		{"zero actor", zero.MayActOn(user), authority.ErrNoAuthority},
		{"owner on owner", aOwner.MayActOn(owner2), nil},
		{"owner on admin", aOwner.MayActOn(admin), nil},
		{"admin on owner", aAdmin.MayActOn(owner1), authority.ErrInsufficientRank},
		{"admin on admin (lateral)", aAdmin.MayActOn(admin2), authority.ErrInsufficientRank},
		{"admin on manager", aAdmin.MayActOn(mgr), nil},
		{"team lead on user", aTL.MayActOn(user), nil},
		{"user on team lead (upward)", aUser.MayActOn(tl), authority.ErrInsufficientRank},
		{"self", aAdmin.MayActOn(admin), authority.ErrSelfTarget},
		{"cross tenant", aAdmin.MayActOn(foreign), authority.ErrNoAuthority},
		{"super-admin target", aOwner.MayActOn(superMember), authority.ErrInsufficientRank},
		{"super on super-admin", aSuper.MayActOn(superMember), nil},
		{"exempt operator on owner", aOp.MayActOn(owner1), nil},
		{"operator without manage", aOpNoManage.MayActOn(user), authority.ErrNoAuthority},
		{"impersonation on owner", aImp.MayActOn(owner1), authority.ErrInsufficientRank},
		{"impersonation on manager", aImp.MayActOn(mgr), nil},
		// H3 — grant ceiling
		{"owner confers tenant.admin", aOwner.MayChangePermissions([]string{p.Meta.TenantAdmin}), nil},
		{"admin confers tenant.admin", aAdmin.MayChangePermissions([]string{p.Meta.TenantAdmin}), authority.ErrGrantExceedsAuthority},
		{"admin confers read_all", aAdmin.MayChangePermissions([]string{p.Crm.Leads.ReadAll}), nil},
		{"manager confers unheld", aMgr.MayChangePermissions([]string{p.Crm.Leads.Assign}), authority.ErrGrantExceedsAuthority},
		{"manager confers held", aMgr.MayChangePermissions([]string{p.Crm.Leads.ReadAll}), nil},
		{"owner confers reserved", aOwner.MayChangePermissions([]string{p.Platform.TenantsManage}), authority.ErrGrantExceedsAuthority},
		{"super confers reserved", aSuper.MayChangePermissions([]string{p.Platform.TenantsManage}), nil},
		{"operator confers tenant.admin", aOp.MayChangePermissions([]string{p.Meta.TenantAdmin}), nil},
		{"operator confers anonymise", aOp.MayChangePermissions([]string{p.Users.Anonymise}), authority.ErrGrantExceedsAuthority},
		{"impersonation confers tenant.admin", aImp.MayChangePermissions([]string{p.Meta.TenantAdmin}), authority.ErrGrantExceedsAuthority},
		{"impersonation confers crm", aImp.MayChangePermissions([]string{p.Crm.Leads.Read}), nil},
		{"zero confers", zero.MayChangePermissions(nil), authority.ErrNoAuthority},
		// H3 — roles
		{"owner assigns owner role", aOwner.MayAssignRole(admin, ownerR), nil},
		{"admin assigns admin role", aAdmin.MayAssignRole(mgr, adminR), authority.ErrInsufficientRank},
		{"admin assigns manager role", aAdmin.MayAssignRole(user, mgrR), nil},
		{"manager assigns team lead role", aMgr.MayAssignRole(user, tlR), nil},
		{"manager assigns manager role", aMgr.MayAssignRole(user, mgrR), authority.ErrInsufficientRank},
		{"manager assigns role with unheld perm", aMgr.MayAssignRole(user, assignR), authority.ErrGrantExceedsAuthority},
		{"admin assigns super role", aAdmin.MayAssignRole(user, superR), authority.ErrGrantExceedsAuthority},
		{"impersonation assigns owner role", aImp.MayAssignRole(mgr, ownerR), authority.ErrInsufficientRank},
		{"operator assigns owner role", aOp.MayAssignRole(user, ownerR), nil},
		{"admin revokes admin role from admin", aAdmin.MayRevokeRole(admin2, adminR), authority.ErrInsufficientRank},
		{"admin creates level 11", aAdmin.MayCreateRole(11), nil},
		{"admin creates level 10", aAdmin.MayCreateRole(10), authority.ErrInsufficientRank},
		{"manager creates level 20", aMgr.MayCreateRole(20), authority.ErrInsufficientRank},
		{"admin relevels manager role to 15", aAdmin.MayEditRole(mgrR, 15, nil), nil},
		{"admin relevels manager role to 5", aAdmin.MayEditRole(mgrR, 5, nil), authority.ErrInsufficientRank},
		{"admin edits admin role", aAdmin.MayEditRole(adminR, authority.NoLevelChange, nil), authority.ErrInsufficientRank},
		{"owner edits owner role", aOwner.MayEditRole(ownerR, authority.NoLevelChange, nil), authority.ErrInsufficientRank},
		{"manager adds held perm to TL role", aMgr.MayEditRole(tlR, authority.NoLevelChange, []string{p.Crm.Leads.ReadAll}), nil},
		{"manager adds unheld perm to TL role", aMgr.MayEditRole(tlR, authority.NoLevelChange, []string{p.Crm.Leads.Assign}), authority.ErrGrantExceedsAuthority},
		{"admin adds tenant.admin to TL role", aAdmin.MayEditRole(tlR, authority.NoLevelChange, []string{p.Meta.TenantAdmin}), authority.ErrGrantExceedsAuthority},
		{"parent more senior", aAdmin.MaySetRoleParent(tlR, mgrR), nil},
		{"parent not more senior", aAdmin.MaySetRoleParent(mgrR, tlR), authority.ErrRoleParentNotEligible},
		{"clear parent", aAdmin.MaySetRoleParent(tlR, nil), nil},
		{"junior edits parent", aTL.MaySetRoleParent(mgrR, adminR), authority.ErrInsufficientRank},
		// H4 — managers
		{"admin links user to manager", aAdmin.MaySetManager(user, mgr), nil},
		{"upward link", aAdmin.MaySetManager(mgr, user), authority.ErrManagerNotEligible},
		{"lateral link", aAdmin.MaySetManager(mgr, mgr2), authority.ErrManagerNotEligible},
		{"inactive manager", aAdmin.MaySetManager(user, inactiveMgr), authority.ErrManagerNotEligible},
		{"manager appoints self", aMgr.MaySetManager(puppet, mgr), nil},
		{"team lead appoints senior", aTL.MaySetManager(user, mgr), authority.ErrInsufficientRank},
		{"admin unlinks from manager", aAdmin.MayUnlinkManager(tl, mgr), nil},
		{"team lead unlinks own report", aTL.MayUnlinkManager(user, tl), nil},
		{"team lead unlinks senior's report", aTL.MayUnlinkManager(user, mgr), authority.ErrInsufficientRank},
		// H4 — approvals
		{"manager approves report held perm", aMgr.MayApprove(tl, p.Crm.Leads.ReadAll), nil},
		{"manager approves unheld perm", aMgr.MayApprove(tl, p.Crm.Leads.Assign), authority.ErrGrantExceedsAuthority},
		{"manager approves puppet", aMgr.MayApprove(puppet, p.Crm.Leads.Read), nil},
		{"manager without roles.approve", aMgrNoApprove.MayApprove(tl, p.Crm.Leads.Read), authority.ErrNotApprover},
		{"manager not the requester's manager", aMgr.MayApprove(user, p.Crm.Leads.Read), authority.ErrNotApprover},
		{"stale upward link", aMgr.MayApprove(staleAdmin, p.Crm.Leads.Read), authority.ErrInsufficientRank},
		{"admin approves anyone below", aAdmin.MayApprove(user, p.Crm.Leads.Assign), nil},
		{"admin approves owner", aAdmin.MayApprove(owner1, p.Crm.Leads.Read), authority.ErrInsufficientRank},
		{"tenant.admin not requestable", aOwner.MayApprove(user, p.Meta.TenantAdmin), authority.ErrGrantExceedsAuthority},
		{"self approval", aUser.MayApprove(user, p.Crm.Leads.Read), authority.ErrSelfTarget},
		{"impersonation approves", aImp.MayApprove(user, p.Crm.Leads.Read), authority.ErrImpersonationNotAllowed},
		{"operator approves", aOp.MayApprove(user, p.Crm.Leads.Assign), nil},
		{"manager denies report", aMgr.MayDeny(tl), nil},
		{"manager denies non-report", aMgr.MayDeny(user), authority.ErrNotApprover},
		{"admin denies", aAdmin.MayDeny(user), nil},
		// H6 — guards
		{"last owner", authority.GuardLastOwner(owner1, []membership.ID{owner1.ID()}), authority.ErrLastOwnerProtected},
		{"second owner remains", authority.GuardLastOwner(owner1, []membership.ID{owner1.ID(), owner2.ID()}), nil},
		{"non-owner target", authority.GuardLastOwner(admin, []membership.ID{owner1.ID()}), nil},
		{"owner keeps tenant.admin", authority.GuardOwnerOverrides(owner2, []string{p.Meta.TenantAdmin}), authority.ErrOwnerProtected},
		{"owner other revoke ok", authority.GuardOwnerOverrides(owner2, []string{p.Crm.Leads.Read}), nil},
		{"admin may lose tenant.admin override", authority.GuardOwnerOverrides(admin, []string{p.Meta.TenantAdmin}), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if tc.want == nil && tc.got != nil {
				t.Fatalf("got %v, want nil", tc.got)
			}
			if tc.want != nil && !errors.Is(tc.got, tc.want) {
				t.Fatalf("got %v, want %v", tc.got, tc.want)
			}
		})
	}
}

func TestRank_TierClampAndRankOf(t *testing.T) {
	t.Parallel()
	for lvl, want := range map[int]authority.Tier{0: authority.TierOwner, 5: authority.TierAdmin, 10: authority.TierAdmin,
		15: authority.TierManager, 20: authority.TierManager, 25: authority.TierTeamLead, 30: authority.TierTeamLead, 50: authority.TierUser, 99: authority.TierUser} {
		if got := authority.Rank(lvl).Tier(); got != want {
			t.Errorf("Rank(%d).Tier() = %s, want %s", lvl, got, want)
		}
	}
	if authority.ClampRank(-1) != authority.RankNone || authority.ClampRank(150) != authority.RankNone || authority.ClampRank(20) != authority.RankManager {
		t.Fatal("ClampRank must fail closed outside [0,99]")
	}
	deleted := role.UnmarshalFromDB(role.Snapshot{ID: role.ID(uid(1)), TenantID: tA, Name: "Gone", HierarchyLevel: 1, IsDeleted: true})
	if got := authority.RankOf([]*role.Role{mkRole(2, tA, "M", false, 20, false), mkRole(3, tA, "U", false, 50, false), deleted}); got != authority.RankManager {
		t.Fatalf("RankOf = %d, want 20 (deleted roles ignored)", got)
	}
	if authority.RankOf(nil) != authority.RankNone {
		t.Fatal("no roles → RankNone")
	}
}

func TestNewActor_ValidationAndImpersonationRank(t *testing.T) {
	t.Parallel()
	if _, err := authority.NewActor(authority.ActorInput{TenantID: tA}); !errors.Is(err, authority.ErrNoAuthority) {
		t.Fatalf("missing membership: %v", err)
	}
	if _, err := authority.NewActor(authority.ActorInput{MembershipID: membership.ID(uid(1)), TenantID: tA, IsImpersonation: true, IsPlatformOperator: true}); !errors.Is(err, authority.ErrNoAuthority) {
		t.Fatalf("impersonating operator: %v", err)
	}
	imp := mkActor(t, authority.ActorInput{MembershipID: membership.ID(uid(1)), TenantID: tA, Rank: authority.RankOwner, IsImpersonation: true,
		EffectivePermissions: []string{"not.a.permission", permission.IdentityPermissions.Crm.Leads.Read}})
	if imp.Rank() != authority.RankAdmin {
		t.Fatalf("impersonation rank = %d, want Admin", imp.Rank())
	}
	if imp.Holds("not.a.permission") || !imp.Holds(permission.IdentityPermissions.Crm.Leads.Read) {
		t.Fatal("unknown names dropped, known kept")
	}
	var zero authority.Actor
	if zero.Valid() || zero.Rank() != authority.RankNone || zero.Holds(permission.IdentityPermissions.Crm.Leads.Read) {
		t.Fatal("zero Actor must hold nothing and rank RankNone")
	}
}

func TestDelta(t *testing.T) {
	t.Parallel()
	got := authority.Delta([]string{"a", "b", "b"}, []string{"b", "c"})
	if fmt.Sprint(got) != "[a c]" {
		t.Fatalf("Delta = %v, want [a c]", got)
	}
}
```

**2. Add membership id tests** to `membership_test.go`:
```go
func TestMembership_CanonicalIDs(t *testing.T) {
	t.Parallel()
	const raw = "0190A000-0000-7000-8000-0000000000AA"
	got, err := membership.ParseID("urn:uuid:" + raw)
	if err != nil || got != membership.ID(strings.ToLower(raw)) {
		t.Fatalf("ParseID = %q, %v", got, err)
	}
	if _, err := membership.ParseID("nope"); !errors.Is(err, membership.ErrInvalid) {
		t.Fatalf("non-UUID: %v", err)
	}
	m := membership.UnmarshalFromDB(membership.Snapshot{ID: membership.ID("0190a000-0000-7000-8000-000000000001"),
		PersonID: "0190a000-0000-7000-8000-000000000002", TenantID: "0190a000-0000-7000-8000-000000000003", Status: membership.StatusActive})
	if err := m.AssignManager(membership.ID(strings.ToUpper(string(m.ID()))), time.Now()); !errors.Is(err, membership.ErrInvalid) {
		t.Fatalf("uppercase self-manager must be rejected: %v", err)
	}
	rid := "0190a000-0000-7000-8000-0000000000bb"
	_ = m.AssignRole(role.ID(rid), time.Now())                  // arch-test:ignore-err - test fixture setup
	_ = m.AssignRole(role.ID(strings.ToUpper(rid)), time.Now()) // arch-test:ignore-err - test fixture setup
	if got := m.RoleAssignments(); len(got) != 1 || got[0] != role.ID(rid) {
		t.Fatalf("assignments = %v, want one canonical id", got)
	}
	if n := len(m.PullEvents()); n != 1 {
		t.Fatalf("events = %d, want 1 (spelling must not defeat dedup)", n)
	}
}
```
Add the `strings`, `errors`, `time` and `role` imports if they are missing.

**3.** Run `go test ./internal/identity/domain/...`. Expect a compile FAIL on undefined `authority`, `ParseID`, `TeamLead`.

**4. Implement** `internal/identity/domain/authority/errors.go`:
```go
package authority

import "github.com/leadkart/leadkart-go/internal/common/errs"

// Sentinels (ADR 0069). The HTTP port maps each to one wire code. Wrap with
// fmt.Errorf("%w: detail", ErrX); errors.Is is identity-based.
var (
	ErrNoAuthority             = errs.New(errs.KindPermissionDenied, "authority", "no authority for this action")
	ErrInsufficientRank        = errs.New(errs.KindPermissionDenied, "authority", "insufficient rank")
	ErrGrantExceedsAuthority   = errs.New(errs.KindPermissionDenied, "authority", "grant exceeds the caller's authority")
	ErrSelfTarget              = errs.New(errs.KindPermissionDenied, "authority", "callers cannot change their own roles, permissions, status or manager")
	ErrImpersonationNotAllowed = errs.New(errs.KindPermissionDenied, "authority", "not available during an impersonation session")
	ErrNotApprover             = errs.New(errs.KindPermissionDenied, "authority", "caller is not an eligible approver for this request")
	ErrLastOwnerProtected      = errs.New(errs.KindConflict, "authority", "the tenant's last active owner cannot be deactivated or demoted")
	ErrOwnerProtected          = errs.New(errs.KindConflict, "authority", "tenant.admin cannot be revoked from an owner")
	ErrManagerNotEligible      = errs.New(errs.KindInvalidInput, "authority", "manager must be an active member more senior than the subordinate")
	ErrRoleParentNotEligible   = errs.New(errs.KindInvalidInput, "authority", "a parent role must be more senior than its child")
)
```

`rank.go`:
```go
package authority

import "github.com/leadkart/leadkart-go/internal/identity/domain/role"

// Rank is a member's seniority (ADR 0069 H1): the numerically lowest
// HierarchyLevel among their live roles. Lower = more senior.
type Rank int

const (
	RankOwner    Rank = role.HierarchyLevelOwner
	RankAdmin    Rank = role.HierarchyLevelAdmin
	RankManager  Rank = role.HierarchyLevelManager
	RankTeamLead Rank = role.HierarchyLevelTeamLead
	RankUser     Rank = role.HierarchyLevelDefault
	// RankNone: no live role, or a token minted before the rank claim (fail closed).
	RankNone Rank = role.HierarchyLevelNoRole
)

// Tier is the wire label of a rank band.
type Tier string

const (
	TierOwner    Tier = "owner"
	TierAdmin    Tier = "admin"
	TierManager  Tier = "manager"
	TierTeamLead Tier = "team_lead"
	TierUser     Tier = "user"
)

// Tier bands: 0 owner; 1–10 admin; 11–20 manager; 21–30 team lead; 31–99 user.
func (r Rank) Tier() Tier {
	switch {
	case r <= RankOwner:
		return TierOwner
	case r <= RankAdmin:
		return TierAdmin
	case r <= RankManager:
		return TierManager
	case r <= RankTeamLead:
		return TierTeamLead
	default:
		return TierUser
	}
}

// Int is the numeric (JWT / wire) form.
func (r Rank) Int() int { return int(r) }

// ClampRank maps an untrusted integer (a JWT claim) into [RankOwner, RankNone];
// out-of-range values fail closed to RankNone.
func ClampRank(n int) Rank {
	if n < int(RankOwner) || n > int(RankNone) {
		return RankNone
	}
	return Rank(n)
}

// RankOf returns the most senior level among live roles; RankNone when none.
func RankOf(roles []*role.Role) Rank {
	best := RankNone
	for _, r := range roles {
		if r == nil || r.IsDeleted() {
			continue
		}
		if lvl := Rank(r.HierarchyLevel()); lvl < best {
			best = lvl
		}
	}
	return best
}
```

`member.go`:
```go
package authority

import (
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// Member is the authority-relevant projection of a membership (ADR 0069).
type Member struct {
	id         membership.ID
	tenantID   tenant.ID
	rank       Rank
	owner      bool
	superAdmin bool
	active     bool
	reportsTo  membership.ID
}

// MemberOf projects m with its live roles (callers load them via
// role.Repository.GetByIDs(m.RoleAssignments()) inside their transaction).
func MemberOf(m *membership.Membership, roles []*role.Role) Member {
	out := Member{id: m.ID(), tenantID: m.TenantID(), rank: RankOf(roles),
		active: m.Status() == membership.StatusActive, reportsTo: m.ReportsTo()}
	for _, r := range roles {
		if r == nil || r.IsDeleted() {
			continue
		}
		out.owner = out.owner || r.IsCompanyOwner()
		out.superAdmin = out.superAdmin || r.IsSuperAdmin()
	}
	return out
}

func (m Member) ID() membership.ID        { return m.id }
func (m Member) TenantID() tenant.ID      { return m.tenantID }
func (m Member) Rank() Rank               { return m.rank }
func (m Member) IsOwner() bool            { return m.owner }
func (m Member) IsSuperAdmin() bool       { return m.superAdmin }
func (m Member) IsActive() bool           { return m.active }
func (m Member) ReportsTo() membership.ID { return m.reportsTo }
```

`actor.go`:
```go
// Package authority is the identity module's single authorization policy for
// authority-changing writes (ADR 0069): rank (who may act on whom), the grant
// ceiling (what may be conferred or removed), approval eligibility, and the
// owner invariants. Pure: no ctx, no I/O, no HTTP. Commands load the facts
// inside their locked transaction and ask; the HTTP port builds the Actor once.
package authority

import (
	"fmt"

	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// ActorInput carries the verified facts NewActor validates. TenantID is the
// caller's OWN (claims) tenant — never the request's re-bound scope tenant.
type ActorInput struct {
	MembershipID         membership.ID
	TenantID             tenant.ID
	Rank                 Rank
	EffectivePermissions []string // satisfied in the caller's own tenant (tenant.admin expanded)
	IsSuperUser          bool
	IsPlatformOperator   bool // slug-anchored, non-impersonation
	IsImpersonation      bool
}

// Actor is the authority of the caller performing a write. Carried
// explicitly in every authority-changing command (tdl_canon §4). The zero
// Actor holds no authority: every decision denies.
type Actor struct {
	valid            bool
	membershipID     membership.ID
	tenantID         tenant.ID
	rank             Rank
	effective        map[string]struct{}
	superUser        bool
	platformOperator bool
	impersonation    bool
}

// NewActor validates in. An impersonation session is Admin-equivalent
// whatever rank it claims (ADR 0069 H2) and never platform or super.
func NewActor(in ActorInput) (Actor, error) {
	if in.MembershipID.IsZero() {
		return Actor{}, fmt.Errorf("%w: membership id required", ErrNoAuthority)
	}
	if in.TenantID.IsZero() {
		return Actor{}, fmt.Errorf("%w: tenant id required", ErrNoAuthority)
	}
	if in.IsImpersonation && (in.IsPlatformOperator || in.IsSuperUser) {
		return Actor{}, fmt.Errorf("%w: an impersonation session carries no platform or super-user authority", ErrNoAuthority)
	}
	rank := ClampRank(int(in.Rank))
	if in.IsImpersonation {
		rank = RankAdmin
	}
	eff := make(map[string]struct{}, len(in.EffectivePermissions))
	for _, n := range in.EffectivePermissions {
		if permission.IsKnown(n) {
			eff[n] = struct{}{}
		}
	}
	return Actor{valid: true, membershipID: in.MembershipID, tenantID: in.TenantID, rank: rank, effective: eff,
		superUser: in.IsSuperUser, platformOperator: in.IsPlatformOperator, impersonation: in.IsImpersonation}, nil
}

func (a Actor) Valid() bool                  { return a.valid }
func (a Actor) MembershipID() membership.ID  { return a.membershipID }
func (a Actor) TenantID() tenant.ID          { return a.tenantID }
func (a Actor) IsSuperUser() bool            { return a.valid && a.superUser }
func (a Actor) IsPlatformOperator() bool     { return a.valid && a.platformOperator }
func (a Actor) IsImpersonation() bool        { return a.valid && a.impersonation }

// Rank is RankNone for the zero Actor (never "owner").
func (a Actor) Rank() Rank {
	if !a.valid {
		return RankNone
	}
	return a.rank
}

// Holds reports whether a may confer, remove or approve name in a tenant it
// acts on: super users hold everything; exempt operators hold tenant.admin and
// everything it implies (never platform-reserved names); everyone else holds
// their own effective set.
func (a Actor) Holds(name string) bool {
	switch {
	case !a.valid || !permission.IsKnown(name):
		return false
	case a.superUser:
		return true
	case a.operatorExempt():
		return name == tenantAdminName() || permission.ImpliedByTenantAdmin(name)
	}
	return a.hasOwn(name)
}

func (a Actor) hasOwn(name string) bool { _, ok := a.effective[name]; return ok }
```

`policy.go`:
```go
package authority

import (
	"fmt"
	"slices"

	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
)

// NoLevelChange is MayEditRole's "level unchanged" sentinel (matches
// command.UpdateRoleCommand's -1).
const NoLevelChange = -1

func tenantAdminName() string { return permission.IdentityPermissions.Meta.TenantAdmin }

// operatorExempt: slug-anchored non-impersonation operator holding
// platform.users.manage or platform.roles.manage (ADR 0069 R3).
func (a Actor) operatorExempt() bool {
	if !a.valid || !a.platformOperator {
		return false
	}
	pp := permission.IdentityPermissions.Platform
	return a.hasOwn(pp.UsersManage) || a.hasOwn(pp.RolesManage)
}

func (a Actor) exempt() bool         { return a.valid && (a.superUser || a.operatorExempt()) }
func (a Actor) outranks(r Rank) bool { return a.rank < r }

// MayActOn — H2: may a change member t (roles, overrides, status, manager)?
func (a Actor) MayActOn(t Member) error {
	switch {
	case !a.valid:
		return ErrNoAuthority
	case a.superUser:
		return nil
	case t.superAdmin:
		return fmt.Errorf("%w: only a super user may change a super-admin member", ErrInsufficientRank)
	case t.id == a.membershipID:
		return ErrSelfTarget
	case a.operatorExempt():
		return nil
	case t.tenantID != a.tenantID:
		return ErrNoAuthority
	case a.rank == RankOwner && t.rank == RankOwner:
		return nil
	case a.outranks(t.rank):
		return nil
	}
	return fmt.Errorf("%w: rank %d may not change a member of rank %d", ErrInsufficientRank, a.rank, t.rank)
}

// MayChangePermissions — H3: may a confer or remove every name in names?
// tenant.admin is owner-only (R5); platform-reserved names are super-only.
func (a Actor) MayChangePermissions(names []string) error {
	if !a.valid {
		return ErrNoAuthority
	}
	if a.superUser {
		return nil
	}
	for _, n := range names {
		switch {
		case n == tenantAdminName():
			if a.operatorExempt() || (a.rank == RankOwner && a.hasOwn(n)) {
				continue
			}
			return fmt.Errorf("%w: only an owner may confer or remove %s", ErrGrantExceedsAuthority, n)
		case !permission.ImpliedByTenantAdmin(n):
			return fmt.Errorf("%w: %q is platform-reserved or unknown", ErrGrantExceedsAuthority, n)
		case !a.Holds(n):
			return fmt.Errorf("%w: caller does not hold %s", ErrGrantExceedsAuthority, n)
		}
	}
	return nil
}

// MayAssignRole — H3: role level strictly below the actor (owners: any
// tenant role), super-admin roles super-only, and the role's whole permission
// set within the ceiling. Revocation is symmetric.
func (a Actor) MayAssignRole(t Member, r *role.Role) error {
	if err := a.MayActOn(t); err != nil {
		return err
	}
	if r == nil {
		return fmt.Errorf("%w: role required", ErrNoAuthority)
	}
	if r.IsSuperAdmin() && !a.superUser {
		return fmt.Errorf("%w: only a super user may assign or revoke a super-admin role", ErrGrantExceedsAuthority)
	}
	if !a.exempt() {
		if r.TenantID() != t.tenantID {
			return ErrNoAuthority
		}
		if a.rank != RankOwner && !a.outranks(Rank(r.HierarchyLevel())) {
			return fmt.Errorf("%w: role level %d is not below rank %d", ErrInsufficientRank, r.HierarchyLevel(), a.rank)
		}
	}
	return a.MayChangePermissions(PermissionNames(r.Permissions()))
}

// MayRevokeRole is symmetric with MayAssignRole (H3).
func (a Actor) MayRevokeRole(t Member, r *role.Role) error { return a.MayAssignRole(t, r) }

// MayCreateRole — H3: a new role's level must be strictly below the actor.
func (a Actor) MayCreateRole(level int) error {
	if !a.valid {
		return ErrNoAuthority
	}
	if a.exempt() || a.outranks(Rank(level)) {
		return nil
	}
	return fmt.Errorf("%w: a new role's level %d must be below rank %d", ErrInsufficientRank, level, a.rank)
}

// MayEditRole — H3: actor strictly more senior than the role's current level
// and (if changing) its new level; delta (added ∪ removed permissions) within
// the ceiling. Covers rename, re-level, permission edits, parent and delete.
func (a Actor) MayEditRole(r *role.Role, newLevel int, delta []string) error {
	if !a.valid {
		return ErrNoAuthority
	}
	if r == nil {
		return fmt.Errorf("%w: role required", ErrNoAuthority)
	}
	if a.superUser {
		return nil
	}
	if r.IsSuperAdmin() {
		return fmt.Errorf("%w: only a super user may edit a super-admin role", ErrInsufficientRank)
	}
	if !a.operatorExempt() {
		if r.TenantID() != a.tenantID {
			return ErrNoAuthority
		}
		if !a.outranks(Rank(r.HierarchyLevel())) {
			return fmt.Errorf("%w: role level %d is not below rank %d", ErrInsufficientRank, r.HierarchyLevel(), a.rank)
		}
		if newLevel != NoLevelChange && !a.outranks(Rank(newLevel)) {
			return fmt.Errorf("%w: new level %d is not below rank %d", ErrInsufficientRank, newLevel, a.rank)
		}
	}
	return a.MayChangePermissions(delta)
}

// MaySetRoleParent — ADR 0058 edges carry no authority (rank is level-only;
// the approver chain is reports_to) but must agree with the ladder: the
// parent is strictly more senior than the child. parent nil = clear.
func (a Actor) MaySetRoleParent(child, parent *role.Role) error {
	if err := a.MayEditRole(child, NoLevelChange, nil); err != nil {
		return err
	}
	if parent == nil {
		return nil
	}
	if parent.TenantID() != child.TenantID() {
		return ErrNoAuthority
	}
	if parent.HierarchyLevel() >= child.HierarchyLevel() {
		return fmt.Errorf("%w: parent level %d must be below child level %d", ErrRoleParentNotEligible, parent.HierarchyLevel(), child.HierarchyLevel())
	}
	return nil
}

// MaySetManager — H4: actor outranks the subordinate; the manager is an active
// same-tenant member strictly more senior than the subordinate (no lateral or
// upward links); the actor is at least as senior as the manager.
func (a Actor) MaySetManager(sub, mgr Member) error {
	if err := a.MayActOn(sub); err != nil {
		return err
	}
	switch {
	case mgr.tenantID != sub.tenantID:
		return ErrNoAuthority
	case !mgr.active:
		return fmt.Errorf("%w: manager is not active", ErrManagerNotEligible)
	case mgr.rank >= sub.rank:
		return fmt.Errorf("%w: manager rank %d must be below member rank %d", ErrManagerNotEligible, mgr.rank, sub.rank)
	case a.exempt(), mgr.id == a.membershipID, mgr.rank >= a.rank:
		return nil
	}
	return fmt.Errorf("%w: cannot appoint a manager more senior than yourself", ErrInsufficientRank)
}

// MayUnlinkManager — H4: removing or replacing current's authority over sub
// needs the actor at least as senior as current.
func (a Actor) MayUnlinkManager(sub, current Member) error {
	if err := a.MayActOn(sub); err != nil {
		return err
	}
	if a.exempt() || current.id == a.membershipID || current.rank >= a.rank {
		return nil
	}
	return fmt.Errorf("%w: the current manager is more senior than you", ErrInsufficientRank)
}

// MayApprove — H4: (a) qualified manager, (b) tenant.admin holder, (c) super
// or exempt operator; never impersonation, never self; perm must be requestable
// and held by the approver.
func (a Actor) MayApprove(requester Member, perm string) error { return a.mayDecide(requester, perm, true) }

// MayDeny — H4 eligibility without "holds the permission".
func (a Actor) MayDeny(requester Member) error { return a.mayDecide(requester, "", false) }

func (a Actor) mayDecide(req Member, perm string, approving bool) error {
	switch {
	case !a.valid:
		return ErrNoAuthority
	case a.impersonation:
		return ErrImpersonationNotAllowed
	case req.id == a.membershipID:
		return ErrSelfTarget
	case approving && !permission.IsRequestable(perm):
		return fmt.Errorf("%w: %q is not requestable", ErrGrantExceedsAuthority, perm)
	case a.exempt():
		return nil
	case req.tenantID != a.tenantID:
		return ErrNoAuthority
	case !a.outranks(req.rank):
		return fmt.Errorf("%w: approver rank %d does not outrank requester rank %d", ErrInsufficientRank, a.rank, req.rank)
	case approving && !a.Holds(perm):
		return fmt.Errorf("%w: approver does not hold %s", ErrGrantExceedsAuthority, perm)
	case a.hasOwn(tenantAdminName()):
		return nil
	case req.reportsTo == a.membershipID && a.hasOwn(permission.IdentityPermissions.Roles.Approve):
		return nil
	}
	return ErrNotApprover
}

// GuardLastOwner — H6: refuse removing target's ownership (deactivation,
// CompanyOwner revocation) when no OTHER active owner remains. activeOwners is
// loaded inside the same locked transaction.
func GuardLastOwner(target Member, activeOwners []membership.ID) error {
	if !target.owner || !target.active {
		return nil
	}
	for _, id := range activeOwners {
		if id != target.id {
			return nil
		}
	}
	return ErrLastOwnerProtected
}

// GuardOwnerOverrides — H6: an owner's effective set always includes tenant.admin.
func GuardOwnerOverrides(target Member, revoked []string) error {
	if target.owner && slices.Contains(revoked, tenantAdminName()) {
		return ErrOwnerProtected
	}
	return nil
}

// Delta is the sorted symmetric difference — what a replace confers or removes.
func Delta(before, after []string) []string {
	set := func(xs []string) map[string]struct{} {
		m := make(map[string]struct{}, len(xs))
		for _, x := range xs {
			m[x] = struct{}{}
		}
		return m
	}
	b, af := set(before), set(after)
	out := []string{}
	for n := range b {
		if _, ok := af[n]; !ok {
			out = append(out, n)
		}
	}
	for n := range af {
		if _, ok := b[n]; !ok {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return out
}

// PermissionNames maps permissions to their names (nil entries dropped).
func PermissionNames(ps []*permission.Permission) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		if p != nil {
			out = append(out, p.Name())
		}
	}
	return out
}
```

`permission` gains, in `implication.go`:
```go
// IsRequestable reports whether name may be the subject of a permission-
// elevation request (ADR 0069 H4): exactly the permissions tenant.admin implies.
// tenant.admin itself and every platform-reserved name are conferred only by role.
func IsRequestable(name string) bool { return ImpliedByTenantAdmin(name) }
```

**5. `role/role.go`.**
- Add the tier constants beside `HierarchyLevelDefault`: `HierarchyLevelOwner = 0`, `HierarchyLevelAdmin = 10`, `HierarchyLevelSeniorManager = 15`, `HierarchyLevelManager = 20`, `HierarchyLevelTeamLead = 30`. Each gets a one-line godoc citing ADR 0069 H1.
- Add:
```go
// IsCompanyOwner reports whether r is the tenant's CompanyOwner system role —
// the Owner tier anchor (ADR 0069 H1/H6).
func (r *Role) IsCompanyOwner() bool {
	return r.isSystemDefault && !r.isSuperAdmin && r.name == SystemRoles.Tenant.CompanyOwner
}

// ParseID canonicalises an untrusted role id (any RFC 4122 spelling) to the
// lowercase hyphenated form repositories store (ADR 0069, finding 1).
func ParseID(raw string) (ID, error) {
	u, err := uuid.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("%w: role id %q is not a UUID", ErrInvalid, raw)
	}
	return ID(u.String()), nil
}
```
- Add the `github.com/google/uuid` import. Parsing is allowed in the domain; only generation is forbidden (`TestArch_NoUUIDGenerationInDomain`).
- In `system_roles.go`, add `TeamLead string` to both struct literals, with `TeamLead: "Team Lead",`. Add `Tenant.TeamLead — 30` to the hierarchy comment and update the comment's levels per H1.

**6. `membership/membership.go`.**
- Add `ParseID`, identical in shape to the role one and using `membership.ErrInvalid`.
- In `AssignRole` and `RevokeRole`, replace the `roleID.IsZero()` guard with:
```go
	canonical, err := role.ParseID(string(roleID))
	if err != nil {
		return fmt.Errorf("%w: roleID: %w", ErrInvalid, err)
	}
	roleID = canonical
```
- In `AssignManager`, before the self check:
```go
	if !managerID.IsZero() {
		canonical, err := ParseID(string(managerID))
		if err != nil {
			return err
		}
		managerID = canonical
	}
	self, err := ParseID(string(m.id))
	if err != nil {
		return err
	}
	if managerID == self {
		return fmt.Errorf("%w: cannot report to self", ErrInvalid)
	}
```
- Add the `uuid` and `strings` imports for `ParseID`.

**7.** Run `go test ./internal/identity/...`. Expect `ok`.
- Some existing fixtures may pass non-UUID ids to `AssignRole`, `RevokeRole` or `AssignManager`. Replace each with a UUID literal; that is the durable guarantee working, not the gate. Do not relax `ParseID`.

**8.** Run G1–G6. Commit: `feat(identity): authority policy — rank, grant ceiling, approval eligibility, owner guards; canonical ids (ADR 0069)`.

---

## Task A2 — Seed tiers and daily-work grants, plus the goose migration (H1, H5)

**Files:**
- Modify `internal/identity/app/seed/role_catalog.go` and `role_catalog_test.go`
- Create `migrations/20261119000001_identity_role_tiers.sql`
- Create `internal/common/pg/identity_role_tiers_migration_integration_test.go`

**Depends on** A1 (role constants, `SystemRoles.Tenant.TeamLead`).

**1. Failing seed tests.** In `role_catalog_test.go`, change `13` to `14` in `TestDefaultRoleCatalog_ContainsExpectedNames` (comment: "13 .NET roles + Team Lead, ADR 0069"), add `role.SystemRoles.Tenant.TeamLead` to the expected names, and add:
```go
func TestDefaultRoleCatalog_TierLevels(t *testing.T) {
	t.Parallel()
	r := role.SystemRoles.Tenant
	want := map[string]int{r.CompanyOwner: 0, r.Administrator: 10, r.OfficeAdministrator: 10, r.SeniorManager: 15,
		r.SalesManager: 20, r.PurchaseManager: 20, r.DispatchManager: 20, r.HrManager: 20, r.TeamLead: 30,
		r.SalesExecutive: 50, r.PurchaseExecutive: 50, r.DispatchExecutive: 50, r.HrExecutive: 50, r.OfficeExecutive: 50}
	for _, s := range seed.DefaultRoleCatalog() {
		if s.HierarchyLevel != want[s.Name] {
			t.Errorf("%s level = %d, want %d", s.Name, s.HierarchyLevel, want[s.Name])
		}
	}
}

func TestDefaultRoleCatalog_DailyWorkGrants(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	r := role.SystemRoles.Tenant
	must := map[string][]string{
		r.Administrator:  {p.Meta.TenantAdmin},
		r.SalesManager:   {p.Users.View, p.Roles.View, p.Roles.Approve, p.Crm.Leads.ReadAll, p.Crm.Leads.Assign, p.Crm.Leads.Manage},
		r.TeamLead:       {p.Users.View, p.Roles.Approve, p.Crm.Leads.Read, p.Tasks.WorkItems.Reassign},
		r.SalesExecutive: {p.Crm.Leads.Read, p.Crm.Leads.Manage, p.Meta.RequestPermissionElevation},
		r.HrManager:      {p.Users.Update, p.Roles.Approve},
	}
	mustNot := map[string][]string{r.TeamLead: {p.Tasks.WorkItems.ReadAll, p.Crm.Leads.ReadAll}, r.SalesExecutive: {p.Users.View}}
	for _, s := range seed.DefaultRoleCatalog() {
		for _, n := range must[s.Name] {
			if !slices.Contains(s.Permissions, n) {
				t.Errorf("%s missing %s", s.Name, n)
			}
		}
		for _, n := range mustNot[s.Name] {
			if slices.Contains(s.Permissions, n) {
				t.Errorf("%s must not hold %s", s.Name, n)
			}
		}
	}
}
```
Add the `slices` import. Run `go test ./internal/identity/app/seed/` and expect FAIL.

**2. Replace the `specs` literal and the post-loop in `DefaultRoleCatalog`.** Keep the existing bundle variables and add these bundles:
```go
	elevation := []string{p.Meta.RequestPermissionElevation}
	directory := []string{p.Users.View, p.Roles.View}
	approver := []string{p.Roles.Approve}
	crmManager := []string{p.Crm.Leads.Read, p.Crm.Leads.ReadAll, p.Crm.Leads.Assign, p.Crm.Leads.Manage}
	crmExec := []string{p.Crm.Leads.Read, p.Crm.Leads.Manage}
	invRead := []string{p.Inventory.Catalog.Read, p.Inventory.Stock.Read}

	specs := []RoleSpec{
		{Name: role.SystemRoles.Tenant.CompanyOwner, IsSystemDefault: true, HierarchyLevel: role.HierarchyLevelOwner,
			Permissions: slices.Concat([]string{p.Meta.TenantAdmin}, tasksManager, dispatchOps, ordersFull)},
		{Name: role.SystemRoles.Tenant.Administrator, HierarchyLevel: role.HierarchyLevelAdmin,
			Permissions: slices.Concat([]string{p.Meta.TenantAdmin}, tasksManager)},
		{Name: role.SystemRoles.Tenant.OfficeAdministrator, HierarchyLevel: role.HierarchyLevelAdmin,
			Permissions: slices.Concat([]string{p.Meta.TenantAdmin}, invRead, tasksManager, ordersRead)},
		{Name: role.SystemRoles.Tenant.SeniorManager, HierarchyLevel: role.HierarchyLevelSeniorManager,
			Permissions: slices.Concat(tasksManager, directory, approver, crmManager, invRead,
				[]string{p.Dispatch.ConsignmentNotes.Read}, ordersRead)},
		{Name: role.SystemRoles.Tenant.SalesManager, HierarchyLevel: role.HierarchyLevelManager,
			Permissions: slices.Concat(invRead, tasksManager, ordersFull, directory, approver, crmManager)},
		{Name: role.SystemRoles.Tenant.PurchaseManager, HierarchyLevel: role.HierarchyLevelManager,
			Permissions: slices.Concat([]string{p.Inventory.Catalog.Manage, p.Inventory.Stock.Manage}, tasksManager, directory, approver)},
		{Name: role.SystemRoles.Tenant.DispatchManager, HierarchyLevel: role.HierarchyLevelManager,
			Permissions: slices.Concat(tasksManager, dispatchOps, directory, approver, []string{p.Orders.Orders.Read, p.Inventory.Stock.Read})},
		{Name: role.SystemRoles.Tenant.HrManager, HierarchyLevel: role.HierarchyLevelManager,
			Permissions: slices.Concat(tasksManager, directory, approver, []string{p.Users.Update})},
		{Name: role.SystemRoles.Tenant.TeamLead, HierarchyLevel: role.HierarchyLevelTeamLead,
			Permissions: slices.Concat(directory, approver, crmExec,
				[]string{p.Tasks.WorkItems.Read, p.Tasks.WorkItems.Manage, p.Tasks.WorkItems.Reassign})},
		{Name: role.SystemRoles.Tenant.OfficeExecutive, HierarchyLevel: role.HierarchyLevelDefault, Permissions: tasksMember},
		{Name: role.SystemRoles.Tenant.SalesExecutive, HierarchyLevel: role.HierarchyLevelDefault,
			Permissions: slices.Concat(tasksMember, ordersExec, crmExec)},
		{Name: role.SystemRoles.Tenant.PurchaseExecutive, HierarchyLevel: role.HierarchyLevelDefault, Permissions: tasksMember},
		{Name: role.SystemRoles.Tenant.DispatchExecutive, HierarchyLevel: role.HierarchyLevelDefault,
			Permissions: slices.Concat(tasksMember, dispatchOps)},
		{Name: role.SystemRoles.Tenant.HrExecutive, HierarchyLevel: role.HierarchyLevelDefault, Permissions: tasksMember},
	}
	for i := range specs {
		specs[i].Permissions = slices.Concat(specs[i].Permissions, notificationsInbox)
		if specs[i].Name != role.SystemRoles.Tenant.CompanyOwner { // implied by tenant.admin
			specs[i].Permissions = slices.Concat(specs[i].Permissions, elevation)
		}
	}
	return specs
```
- Rewrite the godoc grant policy to match the H5 table and cite ADR 0069 and BRD §3.2/§6.7.
- Duplicates are harmless: `GrantPermission` is idempotent.
- Run the seed tests. Expect `ok`.

**3. Migration** `migrations/20261119000001_identity_role_tiers.sql`:
```sql
-- ADR 0069 (H1/H5) — rank tiers + manager-tier daily-work grants for EXISTING
-- tenants' seeded roles. New tenants receive the same shape from
-- seed.DefaultRoleCatalog at registration.
--
-- Pristine-seed detection (never overwrite a tenant's customisation):
--   (1) the name is a DefaultRoleCatalog name, NOT is_deleted, NOT is_system_default;
--   (2) created_at equals the tenant's system-default 'Company Owner' role's
--       created_at — ApplyDefaultRoles stamps the whole batch with one `now`,
--       so a tenant-created homonym (or a renamed-then-recreated role) is newer;
--   (3a) hierarchy_level changes only while it still equals the old seed level;
--   (3b) permissions change only while the SET still equals the old seed set
--        (jsonb containment both ways). Each test is independent.
-- tenant.admin is deliberately NOT added to existing Administrator / Office
-- Administrator roles (ADR 0069 Q2) — owners opt in. Data-only migration: no
-- outbox/audit rows are written (recorded in ADR 0069).

-- +goose Up
-- +goose StatementBegin
SELECT set_config('app.is_platform', 'true', true);

WITH spec(name, old_level, new_level, old_perms, add_perms) AS (
    VALUES
    ('Administrator', 10, 10,
     '["tasks.work_items.read","tasks.work_items.read_all","tasks.work_items.manage","tasks.work_items.reassign","notifications.inbox.read","notifications.inbox.manage"]'::jsonb,
     '["identity.meta.request_permission_elevation"]'::jsonb),
    ('Senior Manager', 20, 15,
     '["tasks.work_items.read","tasks.work_items.read_all","tasks.work_items.manage","tasks.work_items.reassign","notifications.inbox.read","notifications.inbox.manage"]'::jsonb,
     '["identity.users.view","identity.roles.view","identity.roles.approve","crm.leads.read","crm.leads.read_all","crm.leads.assign","crm.leads.manage","inventory.catalog.read","inventory.stock.read","dispatch.consignment_notes.read","orders.quotations.read","orders.orders.read","orders.payments.read","orders.invoices.read","identity.meta.request_permission_elevation"]'::jsonb),
    ('Office Administrator', 50, 10,
     '["inventory.catalog.read","inventory.stock.read","tasks.work_items.read","tasks.work_items.read_all","tasks.work_items.manage","tasks.work_items.reassign","orders.quotations.read","orders.orders.read","orders.payments.read","orders.invoices.read","notifications.inbox.read","notifications.inbox.manage"]'::jsonb,
     '["identity.meta.request_permission_elevation"]'::jsonb),
    ('Office Executive', 50, 50,
     '["tasks.work_items.read","tasks.work_items.manage","notifications.inbox.read","notifications.inbox.manage"]'::jsonb,
     '["identity.meta.request_permission_elevation"]'::jsonb),
    ('Sales Manager', 50, 20,
     '["inventory.catalog.read","inventory.stock.read","tasks.work_items.read","tasks.work_items.read_all","tasks.work_items.manage","tasks.work_items.reassign","orders.quotations.read","orders.quotations.manage","orders.orders.read","orders.orders.manage","orders.payments.read","orders.payments.record","orders.invoices.read","notifications.inbox.read","notifications.inbox.manage"]'::jsonb,
     '["identity.users.view","identity.roles.view","identity.roles.approve","crm.leads.read","crm.leads.read_all","crm.leads.assign","crm.leads.manage","identity.meta.request_permission_elevation"]'::jsonb),
    ('Sales Executive', 50, 50,
     '["tasks.work_items.read","tasks.work_items.manage","orders.quotations.read","orders.quotations.manage","orders.orders.read","orders.payments.read","orders.payments.record","orders.invoices.read","notifications.inbox.read","notifications.inbox.manage"]'::jsonb,
     '["crm.leads.read","crm.leads.manage","identity.meta.request_permission_elevation"]'::jsonb),
    ('Purchase Manager', 50, 20,
     '["inventory.catalog.manage","inventory.stock.manage","tasks.work_items.read","tasks.work_items.read_all","tasks.work_items.manage","tasks.work_items.reassign","notifications.inbox.read","notifications.inbox.manage"]'::jsonb,
     '["identity.users.view","identity.roles.view","identity.roles.approve","identity.meta.request_permission_elevation"]'::jsonb),
    ('Purchase Executive', 50, 50,
     '["tasks.work_items.read","tasks.work_items.manage","notifications.inbox.read","notifications.inbox.manage"]'::jsonb,
     '["identity.meta.request_permission_elevation"]'::jsonb),
    ('Dispatch Manager', 50, 20,
     '["tasks.work_items.read","tasks.work_items.read_all","tasks.work_items.manage","tasks.work_items.reassign","dispatch.consignment_notes.read","dispatch.consignment_notes.manage","notifications.inbox.read","notifications.inbox.manage"]'::jsonb,
     '["identity.users.view","identity.roles.view","identity.roles.approve","orders.orders.read","inventory.stock.read","identity.meta.request_permission_elevation"]'::jsonb),
    ('Dispatch Executive', 50, 50,
     '["tasks.work_items.read","tasks.work_items.manage","dispatch.consignment_notes.read","dispatch.consignment_notes.manage","notifications.inbox.read","notifications.inbox.manage"]'::jsonb,
     '["identity.meta.request_permission_elevation"]'::jsonb),
    ('HR Manager', 50, 20,
     '["tasks.work_items.read","tasks.work_items.read_all","tasks.work_items.manage","tasks.work_items.reassign","notifications.inbox.read","notifications.inbox.manage"]'::jsonb,
     '["identity.users.view","identity.roles.view","identity.roles.approve","identity.users.update","identity.meta.request_permission_elevation"]'::jsonb),
    ('HR Executive', 50, 50,
     '["tasks.work_items.read","tasks.work_items.manage","notifications.inbox.read","notifications.inbox.manage"]'::jsonb,
     '["identity.meta.request_permission_elevation"]'::jsonb)
),
owner_batch AS (
    SELECT tenant_id, created_at
    FROM   identity.roles
    WHERE  name = 'Company Owner' AND is_system_default AND NOT is_deleted
)
UPDATE identity.roles r
SET    hierarchy_level = CASE WHEN r.hierarchy_level = s.old_level THEN s.new_level ELSE r.hierarchy_level END,
       permissions     = CASE WHEN r.permissions @> s.old_perms AND s.old_perms @> r.permissions
                              THEN (SELECT jsonb_agg(DISTINCT e.v ORDER BY e.v)
                                    FROM jsonb_array_elements_text(r.permissions || s.add_perms) AS e(v))
                              ELSE r.permissions END
FROM   spec s, owner_batch ob
WHERE  r.name = s.name
  AND  r.tenant_id = ob.tenant_id
  AND  r.created_at = ob.created_at
  AND  NOT r.is_deleted
  AND  NOT r.is_system_default;

INSERT INTO identity.roles (id, tenant_id, name, is_system_default, is_super_admin, hierarchy_level, permissions, created_at)
SELECT gen_random_uuid(), ob.tenant_id, 'Team Lead', false, false, 30,
       '["identity.users.view","identity.roles.view","identity.roles.approve","crm.leads.read","crm.leads.manage","tasks.work_items.read","tasks.work_items.manage","tasks.work_items.reassign","notifications.inbox.read","notifications.inbox.manage","identity.meta.request_permission_elevation"]'::jsonb,
       ob.created_at
FROM   (SELECT tenant_id, created_at FROM identity.roles
        WHERE name = 'Company Owner' AND is_system_default AND NOT is_deleted) ob
WHERE  NOT EXISTS (SELECT 1 FROM identity.roles x
                   WHERE x.tenant_id = ob.tenant_id AND x.name = 'Team Lead' AND NOT x.is_deleted);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
SELECT set_config('app.is_platform', 'true', true);

DELETE FROM identity.roles r
USING  (SELECT tenant_id, created_at FROM identity.roles
        WHERE name = 'Company Owner' AND is_system_default AND NOT is_deleted) ob
WHERE  r.name = 'Team Lead' AND r.tenant_id = ob.tenant_id AND r.created_at = ob.created_at
  AND  r.hierarchy_level = 30
  AND  NOT EXISTS (SELECT 1 FROM identity.role_assignments ra WHERE ra.role_id = r.id)
  AND  NOT EXISTS (SELECT 1 FROM identity.role_hierarchy_edges e WHERE e.child_role_id = r.id OR e.parent_role_id = r.id);

-- Reverse: same spec rows as Up (copy the VALUES block verbatim), then:
-- UPDATE identity.roles r
-- SET hierarchy_level = CASE WHEN r.hierarchy_level = s.new_level THEN s.old_level ELSE r.hierarchy_level END,
--     permissions     = CASE WHEN r.permissions @> (s.old_perms || s.add_perms) AND (s.old_perms || s.add_perms) @> r.permissions
--                            THEN s.old_perms ELSE r.permissions END
-- FROM spec s, owner_batch ob WHERE <same predicate as Up>;
-- +goose StatementEnd
```
- Write the Down `UPDATE` out in full. The comment above shows its exact shape: the same `WITH spec … owner_batch …` header as Up, then the reversed `SET`.
- `TestArch_DataLiftHasRollbackPath` needs an `UPDATE` in Down, and this provides it.

**4. Integration test** `internal/common/pg/identity_role_tiers_migration_integration_test.go`:
```go
//go:build integration

// arch-test:raw-sql-justified — goose data-migration test: seeds the PRE-migration role rows (old seed levels and permission sets) that no current typed helper can produce, then asserts the post-Up and post-Down rows.
package pg_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pressly/goose/v3"

	"github.com/leadkart/leadkart-go/internal/common/pg"
)

const roleTiersMigration = 20261119000001

const oldSalesManagerPerms = `["inventory.catalog.read","inventory.stock.read","tasks.work_items.read","tasks.work_items.read_all","tasks.work_items.manage","tasks.work_items.reassign","orders.quotations.read","orders.quotations.manage","orders.orders.read","orders.orders.manage","orders.payments.read","orders.payments.record","orders.invoices.read","notifications.inbox.read","notifications.inbox.manage"]`

func TestMigration_IdentityRoleTiers_UpgradesPristineSeedsOnly(t *testing.T) {
	dsn := startPostgres(t)
	db, err := goose.OpenDBWithDriver("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := pg.EnsureGooseDialect(); err != nil {
		t.Fatalf("dialect: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	if err := goose.UpToContext(ctx, db, migrationsDir(t), roleTiersMigration-1); err != nil {
		t.Fatalf("up to previous: %v", err)
	}
	batch := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	pristine, custom := seedTenant(ctx, t, db, "tiers-a", batch), seedTenant(ctx, t, db, "tiers-b", batch)
	insertRole(ctx, t, db, pristine, "Sales Manager", 50, oldSalesManagerPerms, batch)
	insertRole(ctx, t, db, custom, "Sales Manager", 40, oldSalesManagerPerms, batch)            // level customised
	insertRole(ctx, t, db, custom, "HR Manager", 50, `["tasks.work_items.read"]`, batch)       // perms customised
	insertRole(ctx, t, db, custom, "Purchase Manager", 50, `[]`, batch.Add(time.Hour))           // tenant-created homonym

	if err := goose.UpToContext(ctx, db, migrationsDir(t), roleTiersMigration); err != nil {
		t.Fatalf("up: %v", err)
	}
	assertRole(ctx, t, db, pristine, "Sales Manager", 20, "crm.leads.read_all", true)
	assertRole(ctx, t, db, custom, "Sales Manager", 40, "crm.leads.read_all", true) // perms still pristine
	assertRole(ctx, t, db, custom, "HR Manager", 20, "identity.roles.approve", false)
	assertRole(ctx, t, db, custom, "Purchase Manager", 50, "identity.roles.approve", false)
	assertRole(ctx, t, db, pristine, "Team Lead", 30, "tasks.work_items.reassign", true)
	assertRole(ctx, t, db, pristine, "Company Owner", 0, "tenant.admin", true)

	if err := goose.DownToContext(ctx, db, migrationsDir(t), roleTiersMigration-1); err != nil {
		t.Fatalf("down: %v", err)
	}
	assertRole(ctx, t, db, pristine, "Sales Manager", 50, "crm.leads.read_all", false)
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM identity.roles WHERE tenant_id = $1 AND name = 'Team Lead'`, pristine).Scan(&n); err != nil || n != 0 {
		t.Fatalf("Team Lead after down: n=%d err=%v", n, err)
	}
}

func seedTenant(ctx context.Context, t *testing.T, db *sql.DB, slug string, batch time.Time) string {
	t.Helper()
	id := uuid.NewString()
	if _, err := db.ExecContext(ctx, `INSERT INTO identity.tenants (id, slug, legal_name, display_name, status, created_at, activated_at)
		VALUES ($1, $2, $3, $3, 'active', $4, $4)`, id, slug, slug+" Pvt Ltd", batch); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO identity.roles (id, tenant_id, name, is_system_default, is_super_admin, hierarchy_level, permissions, created_at)
		VALUES ($1, $2, 'Company Owner', true, false, 0, '["tenant.admin"]'::jsonb, $3)`, uuid.NewString(), id, batch); err != nil {
		t.Fatalf("seed owner role: %v", err)
	}
	return id
}

func insertRole(ctx context.Context, t *testing.T, db *sql.DB, tenantID, name string, level int, perms string, at time.Time) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `INSERT INTO identity.roles (id, tenant_id, name, is_system_default, is_super_admin, hierarchy_level, permissions, created_at)
		VALUES ($1, $2, $3, false, false, $4, $5::jsonb, $6)`, uuid.NewString(), tenantID, name, level, perms, at); err != nil {
		t.Fatalf("insert role %s: %v", name, err)
	}
}

func assertRole(ctx context.Context, t *testing.T, db *sql.DB, tenantID, name string, wantLevel int, perm string, wantPerm bool) {
	t.Helper()
	var level int
	var has bool
	if err := db.QueryRowContext(ctx, `SELECT hierarchy_level, permissions ? $3 FROM identity.roles
		WHERE tenant_id = $1 AND name = $2 AND NOT is_deleted`, tenantID, name, perm).Scan(&level, &has); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if level != wantLevel || has != wantPerm {
		t.Fatalf("%s: level=%d has(%s)=%v, want %d/%v", name, level, perm, has, wantLevel, wantPerm)
	}
}
```
- If the tenants table has more NOT NULL columns without defaults, add them as `cmd/bootstrap/main.go`'s insert does.
- `?` here is the jsonb key-exists operator. If the pgx stdlib driver mis-parses it, use `jsonb_exists(permissions, $3)`.

**5.** Run G1–G6. For G7:
```
go test -tags=integration -p 1 -count=1 -run 'TestMigration' ./internal/common/pg/...
go test -tags=integration -p 1 -count=1 ./internal/identity/app/seed/... ./internal/identity/app/command/...
```

**6.** Commit: `feat(identity): rank-tier seed levels, Team Lead, manager daily-work grants + pristine-seed data migration (ADR 0069 H1/H5)`.

---

## Task A3 — Rank claim, `authn.AuthorityFrom`, capabilities rank/tier, authn cleanups (findings 5–7)

**Files:**
- Modify `internal/identity/app/jwt/jwt.go` (+ `jwt_test.go`)
- Modify `internal/identity/app/permissions/resolver.go` (+ test)
- Modify `internal/identity/app/command/login.go`, `refresh.go`
- Create `internal/identity/app/command/rank_claim.go`
- Create `internal/identity/ports/authn/authority.go` (+ `authority_test.go`)
- Modify `internal/identity/ports/authn/authorize.go`: `sameTenant`, `SatisfiesInTenant`, `DenySelfPerson`
- Modify `internal/identity/ports/authn/authn.go`: delete `RequireTenantContext`
- Modify `authn_test.go`, `authorize_test.go`, `authorize_decision_test.go`
- Modify `internal/identity/ports/tenant_http.go` (slug handlers)
- Modify `internal/identity/ports/http.go` (person routes)
- Modify `internal/identity/ports/dto.go`, `me_http.go`
- Modify `internal/identity/ports/route_authz_test.go`
- Modify `api/openapi.yaml`

**Interfaces produced:**
```go
// jwt
type Claims struct{ ...; Rank *int `json:"rank,omitzero"` }   // IssueArgs.Rank *int
// permissions
type AuthClaims struct{ Permissions []*permission.Permission; IsSuperUser bool; Rank authority.Rank }
// authn
func AuthorityFrom(ctx context.Context) authority.Actor
func SatisfiesInTenant(ctx context.Context, tenantID, permName string) bool
func DenySelfPerson(pathVar string) func(http.Handler) http.Handler   // inner; 403 self_modification_forbidden
// ports DTO: CapabilitiesDto.Rank int `json:"rank"`; CapabilitiesDto.Tier string `json:"tier"`
```

### Steps

**1. Failing tests.**
- **jwt_test:** `Issue(IssueArgs{..., Rank: &twenty})` → `Verify` → `*c.Rank == 20`. Issuing without `Rank` gives `c.Rank == nil`.
- **resolver_test:** a membership holding level-50 and level-20 roles gives `ResolveAuth(...).Rank == authority.RankManager`. No roles gives `RankNone`.
- **`authn/authority_test.go`:**
```go
func TestAuthorityFrom(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	rank := func(n int) *int { return &n }
	if authn.AuthorityFrom(t.Context()).Valid() {
		t.Fatal("no claims → zero Actor")
	}
	base := &jwt.Claims{TenantID: tenantA, MembershipID: selfMem, Rank: rank(20), Permissions: []string{p.Crm.Leads.Read}}
	a := authn.AuthorityFrom(authn.WithClaims(t.Context(), base))
	if !a.Valid() || a.Rank() != authority.RankManager || !a.Holds(p.Crm.Leads.Read) || string(a.TenantID()) != tenantA {
		t.Fatalf("member actor = %+v", a)
	}
	// caller tenant comes from CLAIMS even when ctx tenancy is re-bound (finding 7)
	scoped := tenancy.WithID(authn.WithClaims(t.Context(), base), tenancy.ID(tenantB))
	if got := authn.AuthorityFrom(scoped).TenantID(); string(got) != tenantA {
		t.Fatalf("TenantID = %s, want claims tenant", got)
	}
	if got := authn.AuthorityFrom(authn.WithClaims(t.Context(), &jwt.Claims{TenantID: tenantA, MembershipID: selfMem})).Rank(); got != authority.RankNone {
		t.Fatalf("missing rank claim → RankNone, got %d", got)
	}
	imp := &jwt.Claims{TenantID: tenantA, MembershipID: selfMem, Rank: rank(0), Permissions: []string{p.Meta.TenantAdmin}, Act: &jwt.ActClaim{Sub: "op"}}
	if got := authn.AuthorityFrom(authn.WithClaims(t.Context(), imp)); got.Rank() != authority.RankAdmin || !got.IsImpersonation() {
		t.Fatalf("impersonation actor = %+v", got)
	}
	op := &jwt.Claims{TenantID: tenantA, TenantSlug: authn.PlatformTenantSlug, IsPlatform: true, MembershipID: selfMem, Permissions: []string{p.Platform.UsersManage}}
	if !authn.AuthorityFrom(authn.WithClaims(t.Context(), op)).IsPlatformOperator() {
		t.Fatal("slug-anchored operator flag")
	}
}
```
- **`DenySelfPerson`:** path person id equal to `c.Subject` in uppercase → 403 `self_modification_forbidden`. Another person → 200. Super user on self → 200.
- **`SatisfiesInTenant`:** a member with `identity.tenants.view` in their own tenant → true; the same member in another tenant → false; an operator holding `platform.tenants.view` → true in any tenant.
- **Capabilities** (`route_authz_test.go`): `azClaims(p.Meta.TenantAdmin)` with `c.Rank = &ten` → JSON `rank == 10`, `tier == "admin"`. Without a rank → `rank == 99`, `tier == "user"`.

**2. Implement.**
- **jwt:** add the `Rank` field to `Claims` (godoc: "ADR 0069 seniority; nil on tokens minted before the claim existed ⇒ RankNone, fail closed") and to `IssueArgs`. In `Issue`, add `Rank: args.Rank,`.
- **resolver:** add `Rank authority.Rank` to `AuthClaims`. In `ResolveAuth`, return `Rank: authority.RankOf(roles)`. The roles were already loaded once, so rank and permissions come from the same snapshot.
- **`rank_claim.go`:**
```go
package command

import "github.com/leadkart/leadkart-go/internal/identity/domain/authority"

// rankClaim renders the ADR 0069 rank for the JWT `rank` claim.
func rankClaim(r authority.Rank) *int { n := r.Int(); return &n }
```
- **login.go / refresh.go:** add `Rank: rankClaim(authClaims.Rank),` to the `IssueArgs` literal. Impersonation minting stays unchanged; `NewActor` forces Admin.
- **`authn/authority.go`:**
```go
package authn

import (
	"context"

	"github.com/leadkart/leadkart-go/internal/identity/domain/authority"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// AuthorityFrom builds the caller's authority.Actor ONCE from the verified
// CLAIMS on ctx (ADR 0069). It never reads the tenancy context: under
// X-Tenant-Id / path re-binding the context tenant is the SCOPE tenant, while
// the actor's tenant, rank and effective permissions are the caller's own.
// Returns the zero Actor (denies everything) without claims.
func AuthorityFrom(ctx context.Context) authority.Actor {
	c, ok := ClaimsFromContext(ctx)
	if !ok || c == nil {
		return authority.Actor{}
	}
	rank := authority.RankNone
	if c.Rank != nil {
		rank = authority.ClampRank(*c.Rank)
	}
	a, err := authority.NewActor(authority.ActorInput{
		MembershipID:         membership.ID(c.MembershipID),
		TenantID:             tenant.ID(c.TenantID),
		Rank:                 rank,
		EffectivePermissions: EffectivePermissions(c),
		IsSuperUser:          c.IsSuperUser,
		IsPlatformOperator:   isPlatformOperator(c),
		IsImpersonation:      c.IsImpersonation(),
	})
	if err != nil {
		return authority.Actor{}
	}
	return a
}
```
- **`authorize.go`:**
  - `sameTenant` becomes `return sameID(a, b)` (finding 6). Delete the byte-equal fallback, and change the `authn_test.go` fixtures `"tenant-test"` and `"tenant-1"` to the UUID constants `tenantA` / `tenantB`.
  - Add:
```go
// SatisfiesInTenant evaluates permName in an explicit tenant (slug lookups
// resolve the tenant from the DB, so the path/ctx tenant is not it).
func SatisfiesInTenant(ctx context.Context, tenantID, permName string) bool {
	c, ok := ClaimsFromContext(ctx)
	return ok && satisfiesIn(c, tenancy.ID(tenantID), permName)
}

// DenySelfPerson refuses /platform/persons/{pathVar}/... mutations on the
// caller's OWN person (ids compared as UUID values). Super users exempt.
// INNER middleware: compose inside RequirePlatform.
func DenySelfPerson(pathVar string) func(http.Handler) http.Handler {
	if strings.TrimSpace(pathVar) == "" {
		panic("authn: DenySelfPerson pathVar required")
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, ok := ClaimsFromContext(r.Context())
			if !ok {
				writeError(w, http.StatusUnauthorized, errCodeUnauthenticated, "missing claims")
				return
			}
			if !c.IsSuperUser && sameID(r.PathValue(pathVar), c.Subject) {
				writeError(w, http.StatusForbidden, errCodeSelfModificationForbidden, "operators cannot change their own person")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
```
- **`authn.go`:** delete `RequireTenantContext` (it has no production caller), its tests in `authorize_test.go` / `authorize_decision_test.go`, and comment references in `tenant_http.go`, `audit_http.go`, `http.go` and `cross_tenant_e2e_test.go`. Do not touch arch-test regexes.
- **`tenant_http.go`, both slug handlers:** replace the two-line inline `operator := …; if !operator && c.TenantID != view.ID` with:
```go
		if !authn.SatisfiesInTenant(r.Context(), view.ID, permission.IdentityPermissions.Tenants.View) {
```
  Keep each branch's existing response (empty list / 404) and add the `permission` import.
- **`http.go`:** wrap the five mutating person routes in `notSelfPerson := authn.DenySelfPerson("personId")`, for example `platform(notSelfPerson(handleGlobalSuspendPerson(log, a)))`:
  - `PATCH …/profile`
  - `POST …/global-suspend`
  - `POST …/lift-global-suspension`
  - `POST …/anonymise`
  - `POST /api/v1/users/{userId}/anonymise` stays as is; it is membership-scoped, and D3 covers it.
- **`dto.go`:** add to `CapabilitiesDto`:
```go
	// Rank / Tier — ADR 0069 seniority (lower = more senior) and its band
	// (owner|admin|manager|team_lead|user). UI hides actions on members whose
	// rank is not strictly greater than this (owners: also on owners).
	Rank int    `json:"rank"`
	Tier string `json:"tier"`
```
- **`me_http.go`:** `actor := authn.AuthorityFrom(r.Context())`, then add `Rank: actor.Rank().Int(), Tier: string(actor.Rank().Tier()),` to the dto literal.
- **OpenAPI:**
  - `CapabilitiesDto`: add `rank: {type: integer, minimum: 0, maximum: 99}` and `tier: {type: string, enum: [owner, admin, manager, team_lead, user]}`, both required.
  - Slug routes: add the `identity.tenants.view` requirement (no-access → empty list / 404).
  - Person mutations: add `403 self_modification_forbidden`.

**3.** Run `go test ./internal/identity/...`. Expect `ok`.

**4.** Run G1–G6. For G7: `go test -tags=integration -p 1 -count=1 -run 'TestE2E_' ./cmd/api/...`. If an existing E2E reads its own tenant by slug as a plain member, update it to use the owner token (Q5).

**5.** Commit: `feat(authn): rank claim + AuthorityFrom (claims-only actor), capabilities rank/tier, slug reads via single decision, person self rule; drop RequireTenantContext (ADR 0069)`.

---

## Task A4 — Membership commands enforce authority inside a locked transaction (H2, H3, H4 links, H6 guards; findings 1, 3, 4)

**Files:**
- Create `internal/identity/adapters/sql/authority_lock.sql` and run `sqlc generate` (→ `adapters/db/authority_lock.sql.go`)
- Create `internal/identity/adapters/authority_lock_pg.go` (+ `authority_lock_pg_integration_test.go`)
- Modify `internal/identity/adapters/role_repository_pg.go`: reads join the ambient UoW
- Create `migrations/20261119000002_membership_reports_to_not_self.sql`
- Create `internal/identity/app/command/authority_support.go`
- Modify `internal/identity/app/command/user_authz.go`, `user_management.go`
- Create `internal/identity/app/command/authority_commands_test.go`
- Delete `user_authz_test.go`; modify `user_management_test.go`
- Modify `internal/identity/ports/users_http.go`
- Create `internal/identity/ports/authority_errors.go`
- Modify `internal/identity/ports/errcodes.go`, `route_authz_test.go`
- Modify `cmd/api/main.go`, `api/openapi.yaml`

**Interfaces produced** (A5 and A6 consume them):
```go
// command
type AuthorityLock interface{ LockTenantAuthority(ctx context.Context, tenantID tenant.ID) error }
func NewAssignUserRoleHandler(uow pg.UnitOfWork, lock AuthorityLock, m membership.Repository, r role.Repository, now func() time.Time) AssignUserRoleHandler
// identical new signature for: NewRevokeUserRoleHandler, NewReplaceUserPermissionOverridesHandler,
// NewAssignUserManagerHandler, NewRemoveUserManagerHandler, NewDeactivateUserHandler, NewReactivateUserHandler
// every command above gains `Actor authority.Actor`
// unexported helpers reused by A5/A6: authorityFacts{memberOf, activeOwnerIDs, mayUnlink, rejectManagerCycle}, translateAuthorityWrite
// adapters
func NewAuthorityLockPG() *AuthorityLockPG
// ports
func writeAuthorityError(w http.ResponseWriter, err error) bool
```

### Steps

**1. SQL and adapter.** `internal/identity/adapters/sql/authority_lock.sql`:
```sql
-- name: LockTenantAuthority :exec
-- ADR 0069: transaction-scoped advisory lock serialising every authority-
-- changing write within one tenant (role/override/status/manager/approval).
SELECT pg_advisory_xact_lock(hashtextextended('identity.authority:' || sqlc.arg(tenant_id)::uuid::text, 0));
```
Run `sqlc generate`; expect no output. `internal/identity/adapters/authority_lock_pg.go`:
```go
package adapters

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/common/pgconv"
	"github.com/leadkart/leadkart-go/internal/identity/adapters/db"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// AuthorityLockPG implements command.AuthorityLock (ADR 0069) with
// pg_advisory_xact_lock: released automatically at commit/rollback. It MUST
// run inside a pg.UnitOfWork transaction — a lock outside one would release
// immediately and protect nothing.
type AuthorityLockPG struct{}

// NewAuthorityLockPG wires the lock (stateless).
func NewAuthorityLockPG() *AuthorityLockPG { return &AuthorityLockPG{} }

// LockTenantAuthority blocks until the tenant's authority lock is held by the ambient tx.
func (*AuthorityLockPG) LockTenantAuthority(ctx context.Context, tenantID tenant.ID) error {
	tx, ok := pg.TxFromContext(ctx)
	if !ok {
		return errors.New("authority lock: requires an ambient UnitOfWork transaction")
	}
	u, err := uuid.Parse(tenantID.String())
	if err != nil {
		return fmt.Errorf("authority lock: parse tenant id %q: %w", tenantID, err)
	}
	if err := db.New(tx).LockTenantAuthority(ctx, pgconv.PgUUID(u)); err != nil {
		return fmt.Errorf("authority lock: %w", err)
	}
	return nil
}
```
Check the generated parameter type; it is likely `pgtype.UUID`.

**2. Lock integration test** `authority_lock_pg_integration_test.go`. Use `//go:build integration`, `repoFixture(t)` from `fixture_integration_test.go`, and a `// SQL contract: advisory xact lock serialises same-tenant writers` marker on the test:
- Inside `tx.WithinTx(tenancy.WithID(ctx, tA), pg.TxScopeTenant, fn)`, where `fn` takes the lock:
  - a second `WithinTx` started from the **outer** ctx (no tx in it) with `context.WithTimeout(…, 300*time.Millisecond)` that takes the tenant-A lock must return an error;
  - the same attempt for tenant B must succeed.
- After the first tx returns, the tenant-A attempt must succeed.
- Calling `LockTenantAuthority` outside a UoW must return an error.
- Use `defer cancel()`. No sleeps.

**3. Role repository joins the UoW.** In `role_repository_pg.go`, add:
```go
// runInTxTenant joins an ambient UoW tx when present, else opens a tenant tx
// (ADR 0067 UoW-join contract) — so authority checks read under the lock's tx.
func (r *RoleRepository) runInTxTenant(ctx context.Context, tenantID tenant.ID, fn func(ctx context.Context, tx pgx.Tx) error) error {
	if tx, ok := pg.TxFromContext(ctx); ok {
		return fn(ctx, tx)
	}
	return r.tx.WithinTxPgxTenant(ctx, tenantID.String(), fn)
}
```
Replace `r.tx.WithinTxPgxTenant(ctx, tenantID.String(), func` with `r.runInTxTenant(ctx, tenantID, func` in `GetByID`, `GetByTenantAndName`, `GetByIDs` and `ListByTenant`.

**4. Migration** `migrations/20261119000002_membership_reports_to_not_self.sql`:
```sql
-- ADR 0069 finding 1: a membership can never report to itself (durable
-- backstop for the canonical-id fix; reports_to is uuid so any spelling of the
-- own id collapses to the same value).
-- +goose Up
-- +goose StatementBegin
SELECT set_config('app.is_platform', 'true', true);
UPDATE identity.tenant_memberships SET reports_to = NULL WHERE reports_to = id;
ALTER TABLE identity.tenant_memberships
    ADD CONSTRAINT chk_memberships_reports_to_not_self CHECK (reports_to IS NULL OR reports_to <> id);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE identity.tenant_memberships DROP CONSTRAINT IF EXISTS chk_memberships_reports_to_not_self; -- ADR 0069
-- +goose StatementEnd
```
- Cycle policy: **cycles are prevented in the command**, by a bounded chain walk under the authority lock. Rank ordering makes a new link non-cycling only when ranks are unchanged, and ranks change. The DB CHECK covers the self-loop only. A recursive-CTE trigger, like ADR 0058's for edges, is follow-up.
- `TestMigrationsApplyCleanly` covers this migration.

**5. Failing command tests** `internal/identity/app/command/authority_commands_test.go`:
```go
package command_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/domain/authority"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership/membershiptest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role/roletest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

const awTenant = tenant.ID("33333333-3333-3333-3333-333333333333")

func awID(n int) string { return fmt.Sprintf("0190b000-0000-7000-8000-%012d", n) }
func awClock() time.Time { return testNow }

type fakeAuthorityLock struct{ calls int }

func (l *fakeAuthorityLock) LockTenantAuthority(_ context.Context, _ tenant.ID) error { l.calls++; return nil }

type authzWorld struct {
	members                                *membershiptest.FakeRepository
	roles                                  *roletest.FakeRepository
	lock                                   *fakeAuthorityLock
	owner, admin, manager, teamLead, exec  *role.Role
	next                                   int
}

func newAuthzWorld(t *testing.T) *authzWorld {
	t.Helper()
	p := permission.IdentityPermissions
	w := &authzWorld{members: membershiptest.NewFakeRepository(), roles: roletest.NewFakeRepository(), lock: &fakeAuthorityLock{}, next: 100}
	w.owner = w.addRole(t, 1, role.SystemRoles.Tenant.CompanyOwner, true, role.HierarchyLevelOwner, p.Meta.TenantAdmin)
	w.admin = w.addRole(t, 2, role.SystemRoles.Tenant.Administrator, false, role.HierarchyLevelAdmin, p.Meta.TenantAdmin)
	w.manager = w.addRole(t, 3, role.SystemRoles.Tenant.SalesManager, false, role.HierarchyLevelManager, p.Crm.Leads.Read, p.Crm.Leads.ReadAll, p.Roles.Approve)
	w.teamLead = w.addRole(t, 4, role.SystemRoles.Tenant.TeamLead, false, role.HierarchyLevelTeamLead, p.Crm.Leads.Read, p.Roles.Approve)
	w.exec = w.addRole(t, 5, role.SystemRoles.Tenant.SalesExecutive, false, role.HierarchyLevelDefault, p.Crm.Leads.Read)
	return w
}

func (w *authzWorld) addRole(t *testing.T, n int, name string, sys bool, level int, perms ...string) *role.Role {
	t.Helper()
	ps := make([]*permission.Permission, 0, len(perms))
	for _, name := range perms {
		ps = append(ps, permission.FromConstant(name))
	}
	r := role.UnmarshalFromDB(role.Snapshot{ID: role.ID(awID(n)), TenantID: awTenant, Name: name, IsSystemDefault: sys,
		HierarchyLevel: level, Permissions: ps, CreatedAt: testNow})
	if err := w.roles.Add(t.Context(), r); err != nil {
		t.Fatalf("add role %s: %v", name, err)
	}
	return r
}

func (w *authzWorld) memberReporting(t *testing.T, reportsTo membership.ID, roles ...*role.Role) *membership.Membership {
	t.Helper()
	w.next++
	ids := make([]role.ID, 0, len(roles))
	for _, r := range roles {
		ids = append(ids, r.ID())
	}
	m := membership.UnmarshalFromDB(membership.Snapshot{ID: membership.ID(awID(w.next)), PersonID: person.ID(awID(w.next + 5000)),
		TenantID: awTenant, Status: membership.StatusActive, JoinedAt: testNow, RoleAssignments: ids, ReportsTo: reportsTo})
	if err := w.members.Add(t.Context(), m); err != nil {
		t.Fatalf("add member: %v", err)
	}
	return m
}

func (w *authzWorld) member(t *testing.T, roles ...*role.Role) *membership.Membership {
	return w.memberReporting(t, "", roles...)
}

func tenantAdminEffective() []string {
	out := []string{permission.IdentityPermissions.Meta.TenantAdmin}
	for _, p := range permission.All() {
		if permission.ImpliedByTenantAdmin(p.Name()) {
			out = append(out, p.Name())
		}
	}
	return out
}

func actorAs(t *testing.T, id membership.ID, rank authority.Rank, perms ...string) authority.Actor {
	t.Helper()
	a, err := authority.NewActor(authority.ActorInput{MembershipID: id, TenantID: awTenant, Rank: rank, EffectivePermissions: perms})
	if err != nil {
		t.Fatalf("NewActor: %v", err)
	}
	return a
}

func superActor(t *testing.T) authority.Actor {
	t.Helper()
	a, err := authority.NewActor(authority.ActorInput{MembershipID: membership.ID(awID(9999)), TenantID: tenant.ID(awID(9998)), Rank: authority.RankOwner, IsSuperUser: true})
	if err != nil {
		t.Fatalf("NewActor: %v", err)
	}
	return a
}

// testAdminActor is a rank-Admin actor for tests whose targets hold no role.
func testAdminActor(t *testing.T) authority.Actor {
	return actorAs(t, membership.ID(awID(9997)), authority.RankAdmin, tenantAdminEffective()...)
}

func TestAuthorityCommands_ZeroActorDeniedBeforeLock(t *testing.T) {
	t.Parallel()
	w := newAuthzWorld(t)
	target := w.member(t)
	h := command.NewAssignUserRoleHandler(fakeUoW{}, w.lock, w.members, w.roles, awClock)
	err := h.Handle(t.Context(), command.AssignUserRoleCommand{TenantID: awTenant, MembershipID: target.ID(), RoleID: w.exec.ID()})
	if !errors.Is(err, authority.ErrNoAuthority) || w.lock.calls != 0 {
		t.Fatalf("err=%v lock=%d, want ErrNoAuthority and no lock", err, w.lock.calls)
	}
}

func TestAssignUserRole_RankAndCeiling(t *testing.T) {
	t.Parallel()
	w := newAuthzWorld(t)
	adminM := w.member(t, w.admin)
	target := w.member(t)
	h := command.NewAssignUserRoleHandler(fakeUoW{}, w.lock, w.members, w.roles, awClock)
	admin := actorAs(t, adminM.ID(), authority.RankAdmin, tenantAdminEffective()...)
	if err := h.Handle(t.Context(), command.AssignUserRoleCommand{TenantID: awTenant, MembershipID: target.ID(), RoleID: w.manager.ID(), Actor: admin}); err != nil {
		t.Fatalf("admin assigns manager role: %v", err)
	}
	if w.lock.calls != 1 {
		t.Fatalf("lock calls = %d, want 1", w.lock.calls)
	}
	if err := h.Handle(t.Context(), command.AssignUserRoleCommand{TenantID: awTenant, MembershipID: target.ID(), RoleID: w.admin.ID(), Actor: admin}); !errors.Is(err, authority.ErrInsufficientRank) {
		t.Fatalf("admin assigns admin role: %v, want ErrInsufficientRank", err)
	}
	router := w.addRole(t, 6, "Lead Router", false, 40, permission.IdentityPermissions.Crm.Leads.Assign)
	mgrM := w.member(t, w.manager)
	mgr := actorAs(t, mgrM.ID(), authority.RankManager, permission.IdentityPermissions.Crm.Leads.Read)
	exec := w.member(t)
	if err := h.Handle(t.Context(), command.AssignUserRoleCommand{TenantID: awTenant, MembershipID: exec.ID(), RoleID: router.ID(), Actor: mgr}); !errors.Is(err, authority.ErrGrantExceedsAuthority) {
		t.Fatalf("manager confers unheld via role: %v", err)
	}
	if err := h.Handle(t.Context(), command.AssignUserRoleCommand{TenantID: awTenant, MembershipID: exec.ID(), RoleID: role.ID(awID(777)), Actor: admin}); !errors.Is(err, command.ErrRoleNotFound) {
		t.Fatalf("unknown role: %v", err)
	}
}

func TestRevokeUserRole_LastOwnerProtected(t *testing.T) {
	t.Parallel()
	w := newAuthzWorld(t)
	owner1 := w.member(t, w.owner)
	h := command.NewRevokeUserRoleHandler(fakeUoW{}, w.lock, w.members, w.roles, awClock)
	cmd := command.RevokeUserRoleCommand{TenantID: awTenant, MembershipID: owner1.ID(), RoleID: w.owner.ID(), Actor: superActor(t)}
	if err := h.Handle(t.Context(), cmd); !errors.Is(err, authority.ErrLastOwnerProtected) {
		t.Fatalf("last owner: %v", err)
	}
	w.member(t, w.owner)
	if err := h.Handle(t.Context(), cmd); err != nil {
		t.Fatalf("with a second owner: %v", err)
	}
}

func TestDeactivateUser_LastOwnerProtected(t *testing.T) {
	t.Parallel()
	w := newAuthzWorld(t)
	owner1 := w.member(t, w.owner)
	h := command.NewDeactivateUserHandler(fakeUoW{}, w.lock, w.members, w.roles, awClock)
	err := h.Handle(t.Context(), command.DeactivateUserCommand{TenantID: awTenant, MembershipID: owner1.ID(), Reason: "offboarding test", Actor: superActor(t)})
	if !errors.Is(err, authority.ErrLastOwnerProtected) {
		t.Fatalf("err = %v", err)
	}
}

func TestReplaceOverrides_OwnerProtectedAndDeltaOnly(t *testing.T) {
	t.Parallel()
	w := newAuthzWorld(t)
	p := permission.IdentityPermissions
	owner1, owner2 := w.member(t, w.owner), w.member(t, w.owner)
	h := command.NewReplaceUserPermissionOverridesHandler(fakeUoW{}, w.lock, w.members, w.roles, awClock)
	ownerActor := actorAs(t, owner1.ID(), authority.RankOwner, tenantAdminEffective()...)
	if err := h.Handle(t.Context(), command.ReplaceUserPermissionOverridesCommand{TenantID: awTenant, MembershipID: owner2.ID(),
		RevokedNames: []string{p.Meta.TenantAdmin}, Actor: ownerActor}); !errors.Is(err, authority.ErrOwnerProtected) {
		t.Fatalf("strip owner: %v", err)
	}
	mgrM := w.member(t, w.manager)
	exec := w.member(t, w.exec)
	_ = w.members.UpdateByID(t.Context(), awTenant, exec.ID(), func(m *membership.Membership) (bool, error) { // arch-test:ignore-err - test fixture setup
		return true, m.GrantPermission(permission.FromConstant(p.Crm.Leads.Assign), time.Time{}, testNow)
	})
	mgr := actorAs(t, mgrM.ID(), authority.RankManager, p.Crm.Leads.Read, p.Crm.Leads.ReadAll, p.Roles.Approve)
	if err := h.Handle(t.Context(), command.ReplaceUserPermissionOverridesCommand{TenantID: awTenant, MembershipID: exec.ID(),
		GrantedNames: []string{p.Crm.Leads.Assign, p.Crm.Leads.ReadAll}, Actor: mgr}); err != nil {
		t.Fatalf("unchanged unheld grant must not be checked: %v", err)
	}
	if err := h.Handle(t.Context(), command.ReplaceUserPermissionOverridesCommand{TenantID: awTenant, MembershipID: exec.ID(),
		GrantedNames: []string{p.Crm.Leads.ReadAll}, Actor: mgr}); !errors.Is(err, authority.ErrGrantExceedsAuthority) {
		t.Fatalf("removing an unheld grant: %v", err)
	}
}

func TestAssignUserManager_Rules(t *testing.T) {
	t.Parallel()
	w := newAuthzWorld(t)
	adminM := w.member(t, w.admin)
	admin := actorAs(t, adminM.ID(), authority.RankAdmin, tenantAdminEffective()...)
	mgr1, mgr2 := w.member(t, w.manager), w.member(t, w.manager)
	tlM := w.member(t, w.teamLead)
	exec := w.member(t, w.exec)
	h := command.NewAssignUserManagerHandler(fakeUoW{}, w.lock, w.members, w.roles, awClock)
	run := func(a authority.Actor, sub, mgr membership.ID) error {
		return h.Handle(t.Context(), command.AssignUserManagerCommand{TenantID: awTenant, MembershipID: sub, ManagerID: mgr, Actor: a})
	}
	if err := run(admin, exec.ID(), mgr1.ID()); err != nil {
		t.Fatalf("admin links exec→manager: %v", err)
	}
	if err := run(admin, mgr1.ID(), mgr2.ID()); !errors.Is(err, authority.ErrManagerNotEligible) {
		t.Fatalf("lateral: %v", err)
	}
	if err := run(admin, mgr1.ID(), exec.ID()); !errors.Is(err, authority.ErrManagerNotEligible) {
		t.Fatalf("upward: %v", err)
	}
	tl := actorAs(t, tlM.ID(), authority.RankTeamLead, permission.IdentityPermissions.Users.Update)
	if err := run(tl, exec.ID(), mgr2.ID()); !errors.Is(err, authority.ErrInsufficientRank) {
		t.Fatalf("team lead appoints senior / unlinks senior: %v", err)
	}
	if err := run(admin, exec.ID(), membership.ID(strings.ToUpper(string(exec.ID())))); !errors.Is(err, membership.ErrInvalid) {
		t.Fatalf("uppercase self link: %v", err)
	}
	// stale upward link mgrStale→tlStale, then tlStale→mgrStale would close a cycle
	tlStale := w.member(t, w.teamLead)
	mgrStale := w.memberReporting(t, tlStale.ID(), w.manager)
	if err := run(admin, tlStale.ID(), mgrStale.ID()); !errors.Is(err, authority.ErrManagerNotEligible) {
		t.Fatalf("cycle: %v", err)
	}
}

func TestRemoveUserManager_JuniorCannotUnlinkSenior(t *testing.T) {
	t.Parallel()
	w := newAuthzWorld(t)
	mgrM := w.member(t, w.manager)
	tlM := w.member(t, w.teamLead)
	exec := w.memberReporting(t, mgrM.ID(), w.exec)
	h := command.NewRemoveUserManagerHandler(fakeUoW{}, w.lock, w.members, w.roles, awClock)
	err := h.Handle(t.Context(), command.RemoveUserManagerCommand{TenantID: awTenant, MembershipID: exec.ID(),
		Actor: actorAs(t, tlM.ID(), authority.RankTeamLead, permission.IdentityPermissions.Users.Update)})
	if !errors.Is(err, authority.ErrInsufficientRank) {
		t.Fatalf("err = %v", err)
	}
}
```
- Port the input-rejection and not-found coverage from the old `user_authz_test.go` here as `TestAuthzHandlers_InputRejections` and `TestAuthzHandlers_NotFound`, using the new constructors and `Actor: testAdminActor(t)`. Then delete `user_authz_test.go`.
- In `user_management_test.go`:
  - change `NewDeactivateUserHandler(repo, clock)` / `NewReactivateUserHandler(repo, clock)` to `(fakeUoW{}, &fakeAuthorityLock{}, repo, roletest.NewFakeRepository(), clock)`;
  - add `Actor: testAdminActor(t)` to each command literal (`newMembership` uses tenant `33333333-…`, which is `awTenant`);
  - keep `TestNewHandlers_PanicOnNilRepo` on the nil repo.
- Run `go test ./internal/identity/app/command/`. Expect a compile FAIL.

**6. Implement** `internal/identity/app/command/authority_support.go`:
```go
package command

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/identity/domain/authority"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// AuthorityLock serialises every authority-changing write within one tenant
// (ADR 0069 H6): taken FIRST inside the UnitOfWork so the facts a policy
// decision reads (target roles, role permission sets, owner set, manager
// chain) cannot change before the write commits. Consumer-side interface;
// adapters.AuthorityLockPG implements it.
type AuthorityLock interface {
	LockTenantAuthority(ctx context.Context, tenantID tenant.ID) error
}

// maxManagerChainDepth bounds the reports-to walk (cycle prevention).
const maxManagerChainDepth = 64

// authorityFacts loads the authority-relevant projection of memberships.
type authorityFacts struct {
	memberships membership.Repository
	roles       role.Repository
}

func (f authorityFacts) memberOf(ctx context.Context, tenantID tenant.ID, m *membership.Membership) (authority.Member, error) {
	ids := m.RoleAssignments()
	if len(ids) == 0 {
		return authority.MemberOf(m, nil), nil
	}
	roles, err := f.roles.GetByIDs(ctx, tenantID, ids)
	if err != nil {
		return authority.Member{}, fmt.Errorf("authority facts: load roles: %w", err)
	}
	return authority.MemberOf(m, roles), nil
}

// activeOwnerIDs lists every active holder of the tenant's CompanyOwner role.
func (f authorityFacts) activeOwnerIDs(ctx context.Context, tenantID tenant.ID) ([]membership.ID, error) {
	owner, err := f.roles.GetByTenantAndName(ctx, tenantID, role.SystemRoles.Tenant.CompanyOwner)
	if errors.Is(err, role.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("authority facts: owner role: %w", err)
	}
	if !owner.IsCompanyOwner() {
		return nil, nil
	}
	all, err := f.memberships.ListForTenant(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("authority facts: members: %w", err)
	}
	var out []membership.ID
	for _, m := range all {
		if m.Status() == membership.StatusActive && slices.Contains(m.RoleAssignments(), owner.ID()) {
			out = append(out, m.ID())
		}
	}
	return out, nil
}

// mayUnlink authorises removing current's authority over target (H4).
func (f authorityFacts) mayUnlink(ctx context.Context, tenantID tenant.ID, a authority.Actor, target authority.Member, current membership.ID) error {
	cm, err := f.memberships.GetByID(ctx, tenantID, current)
	if errors.Is(err, membership.ErrNotFound) {
		return a.MayActOn(target)
	}
	if err != nil {
		return fmt.Errorf("authority facts: current manager: %w", err)
	}
	cur, err := f.memberOf(ctx, tenantID, cm)
	if err != nil {
		return err
	}
	return a.MayUnlinkManager(target, cur)
}

// rejectManagerCycle walks the proposed manager's chain upward; reaching the
// subordinate means the new link would close a cycle.
func (f authorityFacts) rejectManagerCycle(ctx context.Context, tenantID tenant.ID, manager *membership.Membership, subordinate membership.ID) error {
	cur := manager.ReportsTo()
	for range maxManagerChainDepth {
		if cur.IsZero() {
			return nil
		}
		if cur == subordinate {
			return fmt.Errorf("%w: the reporting line would form a cycle", authority.ErrManagerNotEligible)
		}
		next, err := f.memberships.GetByID(ctx, tenantID, cur)
		if errors.Is(err, membership.ErrNotFound) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("authority facts: manager chain: %w", err)
		}
		cur = next.ReportsTo()
	}
	return fmt.Errorf("%w: reporting line deeper than %d levels", authority.ErrManagerNotEligible, maxManagerChainDepth)
}

// authorityWriter bundles one UoW tx + the tenant authority lock + facts.
type authorityWriter struct {
	uow   pg.UnitOfWork
	lock  AuthorityLock
	facts authorityFacts
	now   func() time.Time
}

func newAuthorityWriter(ctor string, uow pg.UnitOfWork, lock AuthorityLock, m membership.Repository, r role.Repository, now func() time.Time) authorityWriter {
	if uow == nil || lock == nil || m == nil || r == nil {
		panic("command: " + ctor + " uow, lock, memberships and roles repositories required")
	}
	if now == nil {
		now = time.Now
	}
	return authorityWriter{uow: uow, lock: lock, facts: authorityFacts{memberships: m, roles: r}, now: now}
}

// locked runs fn in one tenant tx after taking the tenant's authority lock.
func (w authorityWriter) locked(ctx context.Context, tenantID tenant.ID, fn func(ctx context.Context) error) error {
	return w.uow.WithinTx(ctx, pg.TxScopeTenant, func(ctx context.Context) error {
		if err := w.lock.LockTenantAuthority(ctx, tenantID); err != nil {
			return fmt.Errorf("authority lock: %w", err)
		}
		return fn(ctx)
	})
}

// updateMember loads the target under the lock, projects its facts, and lets
// decide authorise + mutate it.
func (w authorityWriter) updateMember(ctx context.Context, tenantID tenant.ID, id membership.ID,
	decide func(ctx context.Context, m *membership.Membership, target authority.Member) error) error {
	return w.locked(ctx, tenantID, func(ctx context.Context) error {
		return w.facts.memberships.UpdateByID(ctx, tenantID, id, func(m *membership.Membership) (bool, error) {
			target, err := w.facts.memberOf(ctx, tenantID, m)
			if err != nil {
				return false, err
			}
			if err := decide(ctx, m, target); err != nil {
				return false, err
			}
			return true, nil
		})
	})
}

// translateAuthorityWrite maps repository sentinels to command sentinels;
// authority errors pass through wrapped (errors.Is still matches).
func translateAuthorityWrite(op string, err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, membership.ErrNotFound):
		return ErrUserNotFound
	case errors.Is(err, role.ErrNotFound):
		return ErrRoleNotFound
	}
	return fmt.Errorf("%s: %w", op, err)
}

// liveGrantedNames lists unexpired overlay grants (expired ones confer nothing).
func liveGrantedNames(m *membership.Membership, now time.Time) []string {
	var out []string
	for _, g := range m.GrantedPermissions() {
		if g.ExpiresAt.IsZero() || now.Before(g.ExpiresAt) {
			out = append(out, g.Permission.Name())
		}
	}
	return out
}
```

**7. Rewrite the handlers in `user_authz.go` and `user_management.go`.**

Every command struct listed gains `Actor authority.Actor` (godoc: "built once by the port via authn.AuthorityFrom; the zero value denies"). Every handler struct becomes `struct{ w authorityWriter }`, and every `New…Handler` uses the A4 signature with `newAuthorityWriter("<CtorName>", uow, lock, m, r, now)`. After the existing id-presence checks, each `Handle` does `if !cmd.Actor.Valid() { return authority.ErrNoAuthority }`. The bodies:

```go
// AssignUserRoleHandler.Handle
	now := h.w.now()
	err := h.w.locked(ctx, cmd.TenantID, func(ctx context.Context) error {
		r, err := h.w.facts.roles.GetByID(ctx, cmd.TenantID, cmd.RoleID)
		if err != nil {
			return err
		}
		return h.w.facts.memberships.UpdateByID(ctx, cmd.TenantID, cmd.MembershipID, func(m *membership.Membership) (bool, error) {
			target, err := h.w.facts.memberOf(ctx, cmd.TenantID, m)
			if err != nil {
				return false, err
			}
			if err := cmd.Actor.MayAssignRole(target, r); err != nil {
				return false, err
			}
			return true, m.AssignRole(cmd.RoleID, now)
		})
	})
	return translateAuthorityWrite("assign_user_role", err)

// RevokeUserRoleHandler.Handle
	now := h.w.now()
	err := h.w.locked(ctx, cmd.TenantID, func(ctx context.Context) error {
		r, err := h.w.facts.roles.GetByID(ctx, cmd.TenantID, cmd.RoleID)
		if errors.Is(err, role.ErrNotFound) {
			r = nil // a soft-deleted role confers nothing: only rank applies
		} else if err != nil {
			return err
		}
		return h.w.facts.memberships.UpdateByID(ctx, cmd.TenantID, cmd.MembershipID, func(m *membership.Membership) (bool, error) {
			target, err := h.w.facts.memberOf(ctx, cmd.TenantID, m)
			if err != nil {
				return false, err
			}
			if r == nil {
				err = cmd.Actor.MayActOn(target)
			} else {
				err = cmd.Actor.MayRevokeRole(target, r)
			}
			if err != nil {
				return false, err
			}
			if r != nil && r.IsCompanyOwner() {
				owners, err := h.w.facts.activeOwnerIDs(ctx, cmd.TenantID)
				if err != nil {
					return false, err
				}
				if err := authority.GuardLastOwner(target, owners); err != nil {
					return false, err
				}
			}
			return true, m.RevokeRole(cmd.RoleID, now)
		})
	})
	return translateAuthorityWrite("revoke_user_role", err)

// ReplaceUserPermissionOverridesHandler.Handle (after resolvePermissions as today)
	now := h.w.now()
	err = h.w.updateMember(ctx, cmd.TenantID, cmd.MembershipID, func(_ context.Context, m *membership.Membership, target authority.Member) error {
		if err := cmd.Actor.MayActOn(target); err != nil {
			return err
		}
		grantedNames, revokedNames := authority.PermissionNames(granted), authority.PermissionNames(revoked)
		delta := slices.Concat(
			authority.Delta(liveGrantedNames(m, now), grantedNames),
			authority.Delta(authority.PermissionNames(m.RevokedPermissions()), revokedNames))
		if err := cmd.Actor.MayChangePermissions(delta); err != nil {
			return err
		}
		if err := authority.GuardOwnerOverrides(target, revokedNames); err != nil {
			return err
		}
		return m.ReplacePermissionOverlays(granted, revoked, now)
	})
	return translateAuthorityWrite("replace_user_permission_overrides", err)

// AssignUserManagerHandler.Handle (after presence checks + Actor check)
	mgrID, err := membership.ParseID(string(cmd.ManagerID))
	if err != nil {
		return err
	}
	subID, err := membership.ParseID(string(cmd.MembershipID))
	if err != nil {
		return err
	}
	if mgrID == subID {
		return fmt.Errorf("%w: cannot report to self", membership.ErrInvalid)
	}
	now := h.w.now()
	err = h.w.locked(ctx, cmd.TenantID, func(ctx context.Context) error {
		mgrM, err := h.w.facts.memberships.GetByID(ctx, cmd.TenantID, mgrID)
		if errors.Is(err, membership.ErrNotFound) {
			return fmt.Errorf("%w: manager not found in tenant", authority.ErrManagerNotEligible)
		}
		if err != nil {
			return err
		}
		mgr, err := h.w.facts.memberOf(ctx, cmd.TenantID, mgrM)
		if err != nil {
			return err
		}
		if err := h.w.facts.rejectManagerCycle(ctx, cmd.TenantID, mgrM, subID); err != nil {
			return err
		}
		return h.w.facts.memberships.UpdateByID(ctx, cmd.TenantID, subID, func(m *membership.Membership) (bool, error) {
			target, err := h.w.facts.memberOf(ctx, cmd.TenantID, m)
			if err != nil {
				return false, err
			}
			if cur := m.ReportsTo(); !cur.IsZero() && cur != mgrID {
				if err := h.w.facts.mayUnlink(ctx, cmd.TenantID, cmd.Actor, target, cur); err != nil {
					return false, err
				}
			}
			if err := cmd.Actor.MaySetManager(target, mgr); err != nil {
				return false, err
			}
			return true, m.AssignManager(mgrID, now)
		})
	})
	return translateAuthorityWrite("assign_user_manager", err)

// RemoveUserManagerHandler.Handle
	now := h.w.now()
	err := h.w.updateMember(ctx, cmd.TenantID, cmd.MembershipID, func(ctx context.Context, m *membership.Membership, target authority.Member) error {
		cur := m.ReportsTo()
		if cur.IsZero() {
			return cmd.Actor.MayActOn(target) // idempotent clear is still authorised
		}
		if err := h.w.facts.mayUnlink(ctx, cmd.TenantID, cmd.Actor, target, cur); err != nil {
			return err
		}
		return m.RemoveManager(now)
	})
	return translateAuthorityWrite("remove_user_manager", err)

// DeactivateUserHandler.Handle
	now := h.w.now()
	err := h.w.updateMember(ctx, cmd.TenantID, cmd.MembershipID, func(ctx context.Context, m *membership.Membership, target authority.Member) error {
		if err := cmd.Actor.MayActOn(target); err != nil {
			return err
		}
		if target.IsOwner() {
			owners, err := h.w.facts.activeOwnerIDs(ctx, cmd.TenantID)
			if err != nil {
				return err
			}
			if err := authority.GuardLastOwner(target, owners); err != nil {
				return err
			}
		}
		return m.Deactivate(cmd.Reason, now)
	})
	return translateAuthorityWrite("deactivate_user", err)

// ReactivateUserHandler.Handle
	now := h.w.now()
	err := h.w.updateMember(ctx, cmd.TenantID, cmd.MembershipID, func(_ context.Context, m *membership.Membership, target authority.Member) error {
		if err := cmd.Actor.MayActOn(target); err != nil {
			return err
		}
		return m.Reactivate(now)
	})
	return translateAuthorityWrite("reactivate_user", err)
```
Update the imports (`pg`, `authority`, `slices`). `UpdateUserProfile` and `CreateUser` are unchanged (Q6).

**8. Ports.**
- `errcodes.go` gains (reuse any constant a parent task already added):
```go
ErrCodeForbidden                 = "forbidden"
ErrCodeSelfModificationForbidden = "self_modification_forbidden"
ErrCodeInsufficientRank          = "insufficient_rank"          // ADR 0069 H2/H3 — 403
ErrCodeImpersonationNotAllowed   = "impersonation_not_allowed"
ErrCodeLastOwnerProtected        = "last_owner_protected"       // ADR 0069 H6 — 409
ErrCodeOwnerProtected            = "owner_protected"            // ADR 0069 H6 — 409
ErrCodeManagerNotEligible        = "manager_not_eligible"       // ADR 0069 H4 — 422
ErrCodeRoleSystemDefault         = "role_system_default"        // ADR 0069 H6 — 409
ErrCodeRoleParentNotEligible     = "role_parent_not_eligible"   // ADR 0069 H6 — 422
ErrCodePermissionNotRequestable  = "permission_not_requestable" // ADR 0069 H4 — 422
```
- `authority_errors.go`:
```go
package ports

import (
	"errors"
	"net/http"

	"github.com/leadkart/leadkart-go/internal/identity/domain/authority"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
)

// writeAuthorityError maps ADR 0069 policy errors to the wire. Returns false
// when err is none of them (caller continues its own mapping).
func writeAuthorityError(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, authority.ErrSelfTarget):
		writeError(w, http.StatusForbidden, ErrCodeSelfModificationForbidden, "callers cannot change their own roles, permissions, status or manager")
	case errors.Is(err, authority.ErrInsufficientRank):
		writeError(w, http.StatusForbidden, ErrCodeInsufficientRank, err.Error())
	case errors.Is(err, authority.ErrGrantExceedsAuthority):
		writeError(w, http.StatusForbidden, ErrCodeGrantExceedsAuthority, err.Error())
	case errors.Is(err, authority.ErrImpersonationNotAllowed):
		writeError(w, http.StatusForbidden, ErrCodeImpersonationNotAllowed, "")
	case errors.Is(err, authority.ErrNoAuthority):
		writeError(w, http.StatusForbidden, ErrCodeForbidden, "")
	case errors.Is(err, authority.ErrLastOwnerProtected):
		writeError(w, http.StatusConflict, ErrCodeLastOwnerProtected, "the tenant's last active owner cannot be deactivated or demoted")
	case errors.Is(err, authority.ErrOwnerProtected):
		writeError(w, http.StatusConflict, ErrCodeOwnerProtected, "tenant.admin cannot be revoked from an owner")
	case errors.Is(err, authority.ErrManagerNotEligible):
		writeError(w, http.StatusUnprocessableEntity, ErrCodeManagerNotEligible, err.Error())
	case errors.Is(err, authority.ErrRoleParentNotEligible):
		writeError(w, http.StatusUnprocessableEntity, ErrCodeRoleParentNotEligible, err.Error())
	case errors.Is(err, role.ErrSystemDefault):
		writeError(w, http.StatusConflict, ErrCodeRoleSystemDefault, "system-default roles cannot be changed")
	default:
		return false
	}
	return true
}
```
- `users_http.go`:
  - In `writeUserMutationResult`, insert after `case err == nil:` a `case writeAuthorityError(w, err):` (empty body), and add `case errors.Is(err, command.ErrRoleNotFound): writeError(w, http.StatusNotFound, ErrCodeRoleNotFound, "")`.
  - In every user-authority handler, add `Actor: authn.AuthorityFrom(r.Context()),` to the command literal.
  - In `handleAssignUserRole`: **delete** the `GetRole` preload, the super-admin block and `requireGrantWithinAuthority`. Parse the body id with `rid, err := role.ParseID(req.RoleID)` → 400 `invalid_role_id`.
  - In `handleReplaceUserPermissionOverrides`: delete `requireGrantWithinAuthority`.
  - In `handleAssignUserManager`: `mid, err := membership.ParseID(req.ManagerID)` → 400 `invalid_manager_id`. Use `ParseID` wherever the coordinator's fix used `authn.CanonicalID` for body ids, so there is one canonical parser per type.
  - Drop the now-unused `query` import.
  - `authority.go` keeps `requireGrantWithinAuthority` until A5 (roles still use it).
- `route_authz_test.go`: in `TestRouteAuthz_GrantBeyondAuthorityRejected`, delete the override-escalation block only. The command test and the A8 E2E cover it now.
- `cmd/api/main.go`:
  - add `authorityLock := adapters.NewAuthorityLockPG()` beside the repositories;
  - change the seven constructors to `command.NewXHandler(tx, authorityLock, memberships, roles, now)`;
  - grep for other callers with `grep -rn "NewAssignUserRoleHandler\|NewRevokeUserRoleHandler\|NewReplaceUserPermissionOverridesHandler\|NewAssignUserManagerHandler\|NewRemoveUserManagerHandler\|NewDeactivateUserHandler\|NewReactivateUserHandler" --include=*.go .` and update every one.
- OpenAPI, on these seven operations: add `insufficient_rank`, `grant_exceeds_authority` and `forbidden` to the 403 description. Add `409 last_owner_protected` (deactivate, revoke role), `409 owner_protected` (overrides), `422 manager_not_eligible` (manager PUT/DELETE) and `404 role_not_found` (assign).

**9.** Run `go test ./internal/identity/...`. Expect `ok`. Run G1–G6. For G7:
```
go test -tags=integration -p 1 -count=1 ./internal/identity/... ; go test -tags=integration -p 1 -count=1 -run 'TestE2E_' ./cmd/api/...
```

**10.** Commit: `fix(identity): membership authority enforced in-command under a per-tenant lock — rank, symmetric diff ceiling, manager rules, last-owner guard (ADR 0069)`.

---

## Task A5 — Role commands and protected system roles (H3, H6; findings 2, 5)

**Files:**
- Modify `internal/identity/domain/role/role.go` (+ `role_test.go`)
- Modify `internal/identity/app/seed/role_catalog.go`
- Modify `internal/identity/app/command/role_management.go` (+ `role_management_test.go`)
- Modify `internal/identity/ports/roles_http.go`
- Delete `internal/identity/ports/authority.go`
- Modify `route_authz_test.go`, `cmd/api/main.go`, `api/openapi.yaml`

**Interfaces produced:**
```go
func role.NewWithPermissions(id ID, tenantID tenant.ID, name string, isSystemDefault bool, hierarchyLevel int, isSuperAdmin bool, perms []*permission.Permission, now time.Time) (*Role, error)
func NewCreateRoleHandler(r role.Repository, edges rolehierarchy.Repository, uow pg.UnitOfWork, now func() time.Time, newRoleID func() role.ID, newEdgeID func() rolehierarchy.ID) // unchanged signature
func NewUpdateRoleHandler(uow pg.UnitOfWork, lock AuthorityLock, r role.Repository, now func() time.Time)
// same new signature for NewDeleteRoleHandler, NewReplaceRolePermissionsHandler, NewGrantRolePermissionHandler, NewRevokeRolePermissionHandler
func NewSetRoleParentHandler(edges rolehierarchy.Repository, roles role.Repository, uow pg.UnitOfWork, lock AuthorityLock, now func() time.Time, newEdgeID func() rolehierarchy.ID)
// CreateRoleCommand / SetRoleParentCommand: ActorMembershipID REPLACED by Actor authority.Actor;
// UpdateRole/DeleteRole/ReplaceRolePermissions/GrantRolePermission/RevokeRolePermission commands gain Actor.
```

### Steps

**1. Domain tests** (`role_test.go`, failing):
```go
func TestRole_SystemDefaultPermissionsImmutable(t *testing.T) {
	t.Parallel()
	ta := permission.IdentityPermissions.Meta.TenantAdmin
	r, err := role.NewWithPermissions("0190a000-0000-7000-8000-000000000001", "0190a000-0000-7000-8000-000000000002",
		role.SystemRoles.Tenant.CompanyOwner, true, 0, false, []*permission.Permission{permission.FromConstant(ta)}, time.Now())
	if err != nil {
		t.Fatalf("NewWithPermissions: %v", err)
	}
	for name, mut := range map[string]error{
		"revoke":  r.RevokePermission(permission.FromConstant(ta), time.Now()),
		"replace": r.ReplacePermissions(nil, time.Now()),
		"grant":   r.GrantPermission(permission.FromConstant(permission.IdentityPermissions.Crm.Leads.Read), time.Now()),
	} {
		if !errors.Is(mut, role.ErrSystemDefault) {
			t.Errorf("%s: %v, want ErrSystemDefault", name, mut)
		}
	}
	if !r.HasPermission(permission.FromConstant(ta)) || r.HierarchyLevel() != 0 {
		t.Fatal("CompanyOwner must keep tenant.admin at level 0")
	}
}

func TestRole_LevelZeroReservedForSystemRoles(t *testing.T) {
	t.Parallel()
	if _, err := role.New("0190a000-0000-7000-8000-000000000003", "0190a000-0000-7000-8000-000000000002", "Shadow Owner", false, 0, false, time.Now()); !errors.Is(err, role.ErrInvalid) {
		t.Fatalf("custom level 0: %v", err)
	}
	r, _ := role.New("0190a000-0000-7000-8000-000000000004", "0190a000-0000-7000-8000-000000000002", "Ops", false, 30, false, time.Now()) // arch-test:ignore-err - test fixture setup
	if err := r.ChangeHierarchyLevel(0); !errors.Is(err, role.ErrInvalid) {
		t.Fatalf("relevel to 0: %v", err)
	}
}
```

**2. Implement in `role.go`.**
- Rename the body of `New` into `NewWithPermissions`. After the level-bounds check, add:
```go
	if hierarchyLevel == HierarchyLevelOwner && !isSystemDefault {
		return nil, fmt.Errorf("%w: hierarchyLevel 0 is reserved for system-default roles (ADR 0069 H6)", ErrInvalid)
	}
```
  After recording `CreatedEvent`, apply each non-nil perm with dedup (append to `r.permissions` and `recordEvent(PermissionGrantedEvent{…})`). That is the same narrative the seed produced through `GrantPermission`.
- `New(...)` becomes `return NewWithPermissions(id, tenantID, name, isSystemDefault, hierarchyLevel, isSuperAdmin, nil, now)`.
- In `ChangeHierarchyLevel`, add the same level-0 guard after the bounds check.
- In `GrantPermission`, `RevokePermission` and `ReplacePermissions`, after `ensureMutable`, add:
```go
	if r.isSystemDefault {
		return fmt.Errorf("%w: %s", ErrSystemDefault, r.name)
	}
```
- Fix the doc comments: "system defaults refuse rename, level, permission changes and delete".
- Fix any existing `role_test.go` cases that mutate system-default roles: assert `ErrSystemDefault`, or switch them to custom roles.

**3. Seed.** In `ApplyDefaultRoles`, resolve `spec.Permissions` through `TryFromConstant` into a slice first, then call `role.NewWithPermissions(..., perms, now)`. Delete the per-permission `GrantPermission` loop. The seed tests stay green.

**4. Role commands** (`role_management.go`). Add:
```go
// roleWriter: one UoW tx + tenant authority lock + roles repo (ADR 0069).
type roleWriter struct {
	uow   pg.UnitOfWork
	lock  AuthorityLock
	roles role.Repository
	now   func() time.Time
}

func newRoleWriter(ctor string, uow pg.UnitOfWork, lock AuthorityLock, r role.Repository, now func() time.Time) roleWriter {
	if uow == nil || lock == nil || r == nil {
		panic("command: " + ctor + " uow, lock and roles repository required")
	}
	if now == nil {
		now = time.Now
	}
	return roleWriter{uow: uow, lock: lock, roles: r, now: now}
}

// updateRole runs decide against the role under the tenant authority lock.
func (w roleWriter) updateRole(ctx context.Context, tenantID tenant.ID, id role.ID, decide func(r *role.Role) (bool, error)) error {
	return w.uow.WithinTx(ctx, pg.TxScopeTenant, func(ctx context.Context) error {
		if err := w.lock.LockTenantAuthority(ctx, tenantID); err != nil {
			return fmt.Errorf("authority lock: %w", err)
		}
		return w.roles.UpdateByID(ctx, tenantID, id, decide)
	})
}

func translateRoleWrite(op string, err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, role.ErrNotFound):
		return ErrRoleNotFound
	case errors.Is(err, role.ErrNameTaken):
		return ErrRoleNameTaken
	}
	return fmt.Errorf("%s: %w", op, err)
}
```
Each handler, after its presence checks and `if !cmd.Actor.Valid() { return authority.ErrNoAuthority }`:
- **CreateRole:** before `role.New`, `if err := cmd.Actor.MayCreateRole(cmd.HierarchyLevel); err != nil { return CreateRoleResult{}, err }`. The edge actor is `cmd.Actor.MembershipID()`. Remove the `ActorMembershipID` field. No lock is needed: a new role has no holders.
- **UpdateRole:** `h.w.updateRole(ctx, tid, rid, func(r) { if err := cmd.Actor.MayEditRole(r, cmd.HierarchyLevel, nil); err != nil {return false, err}; …existing rename/level body… })`, then `translateRoleWrite("update_role", err)`. The `-1` sentinel equals `authority.NoLevelChange`.
- **ReplaceRolePermissions:** inside the closure, `delta := authority.Delta(authority.PermissionNames(r.Permissions()), authority.PermissionNames(target))`; then `MayEditRole(r, authority.NoLevelChange, delta)`; then `r.ReplacePermissions(target, now)`. This closes finding 5: unchanged permissions the caller lacks are not checked.
- **GrantRolePermission:** `var delta []string; if !r.HasPermission(p) { delta = []string{p.Name()} }`; `MayEditRole(r, authority.NoLevelChange, delta)`; `r.GrantPermission`.
- **RevokeRolePermission:** the same, with `if r.HasPermission(p)`.
- **DeleteRole:** `MayEditRole(r, authority.NoLevelChange, authority.PermissionNames(r.Permissions()))`, then `r.Delete`.
- **SetRoleParent:** replace `Handle` and `clearParent` with:
```go
	err := h.uow.WithinTx(ctx, pg.TxScopeTenant, func(ctx context.Context) error {
		if err := h.lock.LockTenantAuthority(ctx, cmd.TenantID); err != nil {
			return fmt.Errorf("authority lock: %w", err)
		}
		child, err := h.roles.GetByID(ctx, cmd.TenantID, cmd.RoleID)
		if err != nil {
			return err
		}
		var parent *role.Role
		if !cmd.NewParentID.IsZero() {
			if parent, err = h.roles.GetByID(ctx, cmd.TenantID, cmd.NewParentID); err != nil {
				return err
			}
		}
		if err := cmd.Actor.MaySetRoleParent(child, parent); err != nil {
			return err
		}
		if err := h.removeExistingEdge(ctx, cmd); err != nil {
			return err
		}
		if parent == nil {
			return nil
		}
		edge, err := rolehierarchy.New(h.newEdgeID(), cmd.TenantID, cmd.RoleID, cmd.NewParentID, cmd.Actor.MembershipID(), cmd.Reason, h.now())
		if err != nil {
			return err
		}
		return h.edges.Add(ctx, edge)
	})
	return translateRoleWrite("set_role_parent", err)
```
  `removeExistingEdge` uses `cmd.Actor.MembershipID()` in `e.Remove`. Removing an edge with no parent remains an idempotent no-op.

**5. Tests** (`role_management_test.go`):
- Mechanically update the constructors (`fakeUoW{}, &fakeAuthorityLock{}` and the role repo). Add `Actor: testOwnerActor(t)` to every command:
```go
func testOwnerActor(t *testing.T) authority.Actor { return actorAs(t, membership.ID(awID(9996)), authority.RankOwner, tenantAdminEffective()...) }
```
  Put it in `authority_commands_test.go`. The roles' tenant must be `awTenant`; use it in `newCustomRole`, or have the actor use the tenant the file uses.
- For SetRoleParent tests, give the parent a more senior level than the child: add `newCustomRoleAt(t, repo, name, level)` and give parents level 40 and children 50. Pass the role repo to the new constructor.
- Add new tests:
  - admin edits the Administrator role → `ErrInsufficientRank`;
  - replace keeps an unheld unchanged permission → ok;
  - replace adds an unheld permission → `ErrGrantExceedsAuthority`;
  - delete a role whose permissions the actor lacks → `ErrGrantExceedsAuthority`;
  - parent not more senior → `ErrRoleParentNotEligible`;
  - an owner replacing the CompanyOwner role → `ErrInsufficientRank`;
  - a super actor replacing the CompanyOwner role → `role.ErrSystemDefault`.

**6. Ports** (`roles_http.go`):
- add `Actor: authn.AuthorityFrom(r.Context())` to all seven role commands;
- remove `actorMembershipFromContext` usage and the function itself;
- delete `requireGrantWithinAuthority` calls;
- canonicalise `parent_role_id` with `role.ParseID`;
- in `writeRoleMutationResult`, add `case writeAuthorityError(w, err):` right after `err == nil`;
- `handleCreateRole`: insert `if writeAuthorityError(w, err) { return }` before its switch.

Delete `internal/identity/ports/authority.go`. In `route_authz_test.go`, delete `TestRouteAuthz_GrantBeyondAuthorityRejected`.

**7. Wiring.** In `main.go`:
```go
UpdateRole:             command.NewUpdateRoleHandler(tx, authorityLock, roles, now),
DeleteRole:             command.NewDeleteRoleHandler(tx, authorityLock, roles, now),
ReplaceRolePermissions: command.NewReplaceRolePermissionsHandler(tx, authorityLock, roles, now),
GrantRolePermission:    command.NewGrantRolePermissionHandler(tx, authorityLock, roles, now),
RevokeRolePermission:   command.NewRevokeRolePermissionHandler(tx, authorityLock, roles, now),
SetRoleParent:          command.NewSetRoleParentHandler(roleHierarchyEdges, roles, tx, authorityLock, now, newRoleHierarchyEdgeID),
```
Grep for other callers and update them.

**8. OpenAPI:**
- role routes: `403 insufficient_rank | grant_exceeds_authority`, `409 role_system_default`, `422 role_parent_not_eligible` (parent);
- `CreateRoleRequest.hierarchy_level`: "required in practice: 0 is reserved for system roles (422 role_invalid); must be greater than the caller's rank";
- replace: "only added/removed permissions are checked".

**9.** Run G1–G6 and the full `task test:int ; echo INT=$?` (expect 0).

**10.** Commit: `fix(identity): role edits bounded by rank + diff-based ceiling; system-default roles immutable, level 0 reserved; role-parent edges ladder-consistent (ADR 0069)`.

---

## Task A6 — Permission requests: requestable set, approver rule, queue, detail (H4; findings 2, 4)

**Files:**
- Modify `internal/identity/domain/permissionrequest/request.go` (+ `request_test.go`)
- Modify `internal/identity/domain/permissionrequest/repository.go`
- Modify `permissionrequesttest/fake_repository.go`
- Modify `internal/identity/adapters/sql/permission_requests.sql` (+ `sqlc generate`)
- Modify `internal/identity/adapters/permission_request_repository_pg.go`
- Modify `internal/identity/app/command/permission_request.go` (+ test)
- Modify `internal/identity/app/query/permission_request.go` (+ test)
- Modify `internal/identity/ports/permission_requests_http.go`
- Modify `cmd/api/main.go`, `api/openapi.yaml`

**Interfaces produced:**
```go
var permissionrequest.ErrNotRequestable  // KindInvalidInput → 422 permission_not_requestable
// Repository: ListPendingApprovableBy REMOVED; added:
ListPendingByRequesters(ctx context.Context, tenantID tenant.ID, requesters []membership.ID, pageSize int, cursor pagination.Cursor) (pagination.Page[*Request], error)
type ApprovePermissionRequestCommand struct{ TenantID tenant.ID; RequestID permissionrequest.ID; Actor authority.Actor; DecisionReason string }
type DenyPermissionRequestCommand    struct{ TenantID tenant.ID; RequestID permissionrequest.ID; Actor authority.Actor; DecisionReason string }
func NewApprovePermissionRequestHandler(uow pg.UnitOfWork, lock AuthorityLock, requests permissionrequest.Repository, memberships membership.Repository, roles role.Repository, now func() time.Time, newOverrideID func() uuid.UUID)
func NewDenyPermissionRequestHandler(uow pg.UnitOfWork, lock AuthorityLock, requests permissionrequest.Repository, memberships membership.Repository, roles role.Repository, now func() time.Time)
type query.ListPendingForApproverQuery struct{ TenantID tenant.ID; Actor authority.Actor; Cursor pagination.Cursor; PageSize int }
func query.NewListPendingForApproverHandler(r permissionrequest.Repository, m membership.Repository, ro role.Repository)
```

### Steps

**1. Domain.**
- Add the sentinel:
```go
// ErrNotRequestable: tenant.admin and platform-reserved permissions are conferred only by role (ADR 0069 H4).
var ErrNotRequestable = errs.New(errs.KindInvalidInput, "permission_request", "permission cannot be requested")
```
- In `New`, after the `IsKnown` check: `if !permission.IsRequestable(perm.Name()) { return nil, fmt.Errorf("%w: %q", ErrNotRequestable, perm.Name()) }`.
- Tests: `New(… tenant.admin …)` and `New(… platform.tenants.manage …)` → `ErrNotRequestable`; `crm.leads.read_all` → ok. Change any existing test fixture that requests a non-requestable name to `tasks.work_items.read_all`.

**2. Repository and approver queue.** In `repository.go`, replace `ListPendingApprovableBy` with `ListPendingByRequesters` (godoc: "pending requests from the given requesters — the H4 approver queue; eligibility is computed by the query handler"). In SQL, replace `ListPendingPermissionRequestsByApproverPage` with:
```sql
-- name: ListPendingPermissionRequestsByRequestersPage :many
-- ADR 0069 H4 approver queue: pending requests from requesters the caller may
-- decide (eligibility computed in app/query). Keyset as ADR 0038.
SELECT id, tenant_id, requester_membership_id, permission_constant,
       duration_days, reason, state, approver_membership_id,
       decided_at, decision_reason, granted_override_id, expires_at,
       created_at, updated_at
FROM   identity.permission_requests
WHERE  requester_membership_id = ANY(sqlc.arg(requester_ids)::uuid[])
  AND  state = 'pending'
  AND  (created_at, id) < (sqlc.arg(before_created_at)::timestamptz, sqlc.arg(before_id)::uuid)
ORDER  BY created_at DESC, id DESC
LIMIT  sqlc.arg(row_limit);
```
Run `sqlc generate`. In the adapter, rename the method and build `[]pgtype.UUID` from `requesters` exactly as `RoleRepository.GetByIDs` does (empty input → empty page, no query). Pass `cursorOrPermReqSentinel(cursor)` and `int32(pageSize+1)` with the existing `//nolint:gosec` comment. Fake:
```go
func (f *FakeRepository) ListPendingByRequesters(_ context.Context, tenantID tenant.ID, requesters []membership.ID, pageSize int, _ pagination.Cursor) (pagination.Page[*permissionrequest.Request], error) {
	var items []*permissionrequest.Request
	for _, x := range f.requests {
		if x.TenantID() == tenantID && x.State() == permissionrequest.StatePending && slices.Contains(requesters, x.RequesterMembershipID()) {
			items = append(items, x)
		}
	}
	return pagination.BuildPage(items, pageSize, func(req *permissionrequest.Request) pagination.Cursor {
		return pagination.Cursor{SortValue: req.CreatedAt(), ID: req.ID().String()}
	}), nil
}
```
Update the `reqsErrRepo` stub in `query/permission_request_test.go`.

**3. Query** `ListPendingForApproverHandler.Handle`:
```go
	if q.TenantID.IsZero() {
		return pagination.Page[PermissionRequestView]{}, errors.New("list_pending_for_approver: tenant id required")
	}
	empty := pagination.Page[PermissionRequestView]{Items: []PermissionRequestView{}}
	if !q.Actor.Valid() {
		return empty, nil
	}
	members, err := h.memberships.ListForTenant(ctx, q.TenantID)
	if err != nil {
		return pagination.Page[PermissionRequestView]{}, fmt.Errorf("list_pending_for_approver: members: %w", err)
	}
	roles, err := h.roles.ListByTenant(ctx, q.TenantID)
	if err != nil {
		return pagination.Page[PermissionRequestView]{}, fmt.Errorf("list_pending_for_approver: roles: %w", err)
	}
	byID := make(map[role.ID]*role.Role, len(roles))
	for _, r := range roles {
		byID[r.ID()] = r
	}
	var eligible []membership.ID
	for _, m := range members {
		if m.Status() != membership.StatusActive {
			continue
		}
		var held []*role.Role
		for _, id := range m.RoleAssignments() {
			if r, ok := byID[id]; ok {
				held = append(held, r)
			}
		}
		if q.Actor.MayDeny(authority.MemberOf(m, held)) == nil {
			eligible = append(eligible, m.ID())
		}
	}
	if len(eligible) == 0 {
		return empty, nil
	}
	page, err := h.requests.ListPendingByRequesters(ctx, q.TenantID, eligible, pagination.ClampPageSize(q.PageSize), q.Cursor)
	if err != nil {
		return pagination.Page[PermissionRequestView]{}, fmt.Errorf("list_pending_for_approver: %w", err)
	}
	return projectPermissionRequestPage(page), nil
```
Query tests:
- an admin actor sees a pending request from a rank-50 member;
- a manager sees only their direct report's request;
- the zero actor gets an empty page;
- a pending request whose `approver_membership_id` is NULL is listed (regression for the old empty-queue bug).

**4. Commands.**
- Approve: replace `ApproverMembershipID` and `IsPlatformOperator` with `Actor`. Delete `callerCanApprove` and `canApproveSimilar`. Add the `lock` and `roles` deps; `facts := authorityFacts{memberships, roles}`. The body:
```go
	if !cmd.Actor.Valid() {
		return fmt.Errorf("%w: %w", ErrPermissionRequestForbidden, authority.ErrNoAuthority)
	}
	now := h.now()
	overrideID := h.newOverrideID()
	err := h.uow.WithinTx(ctx, pg.TxScopeTenant, func(ctx context.Context) error {
		if err := h.lock.LockTenantAuthority(ctx, cmd.TenantID); err != nil {
			return fmt.Errorf("authority lock: %w", err)
		}
		req, err := h.requests.GetByID(ctx, cmd.TenantID, cmd.RequestID)
		if err != nil {
			return err
		}
		if req.State() != permissionrequest.StatePending {
			return ErrPermissionRequestNotPending
		}
		if cmd.Actor.MembershipID() == req.RequesterMembershipID() {
			return ErrPermissionRequestSelfApproval
		}
		requesterM, err := h.facts.memberships.GetByID(ctx, req.TenantID(), req.RequesterMembershipID())
		if errors.Is(err, membership.ErrNotFound) {
			return ErrPermissionRequestNotFound
		}
		if err != nil {
			return err
		}
		requester, err := h.facts.memberOf(ctx, req.TenantID(), requesterM)
		if err != nil {
			return err
		}
		if err := cmd.Actor.MayApprove(requester, req.Permission().Name()); err != nil {
			return decisionRefused(err)
		}
		expiresAt := now.Add(time.Duration(req.DurationDays()) * 24 * time.Hour)
		if err := h.requests.UpdateByID(ctx, cmd.TenantID, cmd.RequestID, func(loaded *permissionrequest.Request) (bool, error) {
			return true, loaded.Approve(cmd.Actor.MembershipID(), cmd.DecisionReason, overrideID, expiresAt, now)
		}); err != nil {
			return err
		}
		return h.facts.memberships.UpdateByID(ctx, req.TenantID(), req.RequesterMembershipID(), func(m *membership.Membership) (bool, error) {
			return true, m.GrantPermission(req.Permission(), expiresAt, now)
		})
	})
```
  Then keep the existing error-translation switch, adding `case errors.Is(err, permissionrequest.ErrNotFound): return ErrPermissionRequestNotFound` and passing through `ErrPermissionRequestForbidden`, `ErrPermissionRequestSelfApproval`, `ErrPermissionRequestNotPending` and `authority.ErrImpersonationNotAllowed` unchanged. Add:
```go
// decisionRefused folds policy refusals into the existing 403 shape; an
// impersonation refusal keeps its own code.
func decisionRefused(err error) error {
	if errors.Is(err, authority.ErrImpersonationNotAllowed) {
		return err
	}
	return fmt.Errorf("%w: %w", ErrPermissionRequestForbidden, err)
}
```
- Deny: the same shape with `MayDeny(requester)` and `Deny(cmd.Actor.MayDeny…)`. It is one aggregate, but it runs in the UoW for the lock.
- `RequestPermissionElevation` maps nothing new: `ErrNotRequestable` propagates.
- Command tests (`permission_request_test.go`):
  - mechanically switch to the new constructors (`fakeUoW{}, &fakeAuthorityLock{}`, plus `roletest.NewFakeRepository()`);
  - replace `ApproverMembershipID: X, IsPlatformOperator: b` with `Actor: actorAs(t, X, authority.RankManager, <requested perm>, permission.IdentityPermissions.Roles.Approve)`; for the operator variant use an actor with `IsPlatformOperator: true` and `platform.users.manage` in its effective set;
  - the `b2MemRepo` stub must still satisfy `membership.Repository`;
  - rename `TestApprovePermissionRequest_MissingManagerRequiresPlatform` to `TestApprovePermissionRequest_NoManager_AdminTierApproves` and assert two cases: a plain rank-20 non-manager → `ErrPermissionRequestForbidden`; an Admin actor holding `tenant.admin` → success with the grant applied;
  - add: the manager lacks the requested permission → `ErrPermissionRequestForbidden`; an impersonation actor → `authority.ErrImpersonationNotAllowed`.

**5. Ports.**
- Create: map `errors.Is(err, permissionrequest.ErrNotRequestable)` → 422 `permission_not_requestable`, placed before the `ErrInvalidRequest` case.
- Approve/Deny: send `Actor: authn.AuthorityFrom(r.Context())`. Add `case errors.Is(err, authority.ErrImpersonationNotAllowed): writeError(w, 403, ErrCodeImpersonationNotAllowed, "")` before `ErrPermissionRequestForbidden`.
- List: `query.ListPendingForApproverQuery{TenantID: tenant.ID(tid), Actor: authn.AuthorityFrom(r.Context()), Cursor: cursor, PageSize: pageSize}`.
- Get detail (enumeration-safe 404 unchanged): replace the `isPlatform`/`isApprover` lines with:
```go
		actor := authn.AuthorityFrom(r.Context())
		visible := view.RequesterMembershipID == c.MembershipID ||
			(view.ApproverMembershipID != "" && view.ApproverMembershipID == c.MembershipID) ||
			actor.IsPlatformOperator() || actor.IsSuperUser() ||
			actor.Holds(permission.IdentityPermissions.Meta.TenantAdmin) // admins see every request (H4 visibility)
		if !visible {
			if u, uerr := a.Queries.GetUser.Handle(r.Context(), query.GetUserQuery{TenantID: tenant.ID(tid), MembershipID: membership.ID(view.RequesterMembershipID)}); uerr == nil && u.ReportsTo == c.MembershipID {
				visible = true // the requester's current manager
			}
		}
		if !visible {
			writeError(w, http.StatusNotFound, ErrCodePermissionRequestNotFound, "")
			return
		}
```
- `main.go`:
```go
ApprovePermissionRequest: command.NewApprovePermissionRequestHandler(tx, authorityLock, permissionRequests, memberships, roles, now, newOverrideID),
DenyPermissionRequest:    command.NewDenyPermissionRequestHandler(tx, authorityLock, permissionRequests, memberships, roles, now),
ListPendingForApprover:   query.NewListPendingForApproverHandler(permissionRequests, memberships, roles),
```
- OpenAPI:
  - submit: `422 permission_not_requestable` ("tenant.admin and platform-reserved permissions are conferred only by role");
  - approve/deny: replace the "manager-or-platform" text with the H4 rule; add `403 impersonation_not_allowed`;
  - list `role=approver`: "requests from members you may decide: your direct reports (with identity.roles.approve), or everyone below you if you hold tenant.admin";
  - `CreatePermissionRequestResponse.approver_membership_id`: "the requester's manager, if any; admin-tier members can also decide".

**6.** Run G1–G6. For G7:
```
go test -tags=integration -p 1 -count=1 ./internal/identity/... ; go test -tags=integration -p 1 -count=1 -run 'TestE2E_' ./cmd/api/...
```

**7.** Commit: `fix(identity): permission-request approval bound by rank + approver authority; tenant.admin not requestable; approver queue fixed (ADR 0069 H4, supersedes ADR 0055 §4)`.

---

## Task A7 — ADR 0069, fitness function, OpenAPI consolidation

**Files:**
- Create `docs/adr/0069-rank-hierarchy-and-grant-authority.md`
- Modify `docs/adr/README.md`
- Create `internal/architecture/authority_arch_test.go`
- Modify `api/openapi.yaml` (`info.description`)

**1. Fitness function.** Write the test. Its RED proof: temporarily delete `Actor` from `AssignUserRoleCommand`, run the test, see FAIL, revert.
```go
// authority_arch_test.go — ADR 0069: every authority-changing identity
// command carries the caller's authority explicitly, and no command smuggles a
// bare platform flag.
package architecture_test

import (
	"go/ast"
	"path/filepath"
	"slices"
	"testing"
)

var authorityCommands = []string{
	"AssignUserRoleCommand", "RevokeUserRoleCommand", "ReplaceUserPermissionOverridesCommand",
	"AssignUserManagerCommand", "RemoveUserManagerCommand", "DeactivateUserCommand", "ReactivateUserCommand",
	"CreateRoleCommand", "UpdateRoleCommand", "ReplaceRolePermissionsCommand", "GrantRolePermissionCommand",
	"RevokeRolePermissionCommand", "DeleteRoleCommand", "SetRoleParentCommand",
	"ApprovePermissionRequestCommand", "DenyPermissionRequestCommand",
}

// TestArch_AuthorityCommandsCarryActor fails when a listed command lacks an
// `Actor authority.Actor` field, or any identity command declares an
// `IsPlatformOperator` field (the pre-ADR-0069 smuggled flag).
// Scope: production — applies to non-test files under internal/identity/app/command.
//
// arch-test:no-negative-fixture (RED proof: delete Actor from AssignUserRoleCommand → this test fails; recorded in the A7 commit message)
func TestArch_AuthorityCommandsCarryActor(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(internalDir(t), "identity", "app", "command")
	found := map[string]bool{}
	walkGoFiles(t, dir, false, func(path string, src []byte) {
		_, f := parseFile(t, path, src)
		ast.Inspect(f, func(n ast.Node) bool {
			ts, ok := n.(*ast.TypeSpec)
			if !ok {
				return true
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok {
				return true
			}
			for _, fld := range st.Fields.List {
				for _, nm := range fld.Names {
					if nm.Name == "IsPlatformOperator" {
						t.Errorf("%s: %s carries IsPlatformOperator — pass authority.Actor (ADR 0069)", pathToSlash(path), ts.Name.Name)
					}
				}
			}
			if slices.Contains(authorityCommands, ts.Name.Name) {
				found[ts.Name.Name] = true
				if !hasActorField(st) {
					t.Errorf("%s: %s lacks `Actor authority.Actor` (ADR 0069)", pathToSlash(path), ts.Name.Name)
				}
			}
			return true
		})
	})
	for _, c := range authorityCommands {
		if !found[c] {
			t.Errorf("authorityCommands lists %s but no such struct exists — update the list", c)
		}
	}
}

func hasActorField(st *ast.StructType) bool {
	for _, f := range st.Fields.List {
		sel, ok := f.Type.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Actor" {
			continue
		}
		if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "authority" {
			continue
		}
		for _, n := range f.Names {
			if n.Name == "Actor" {
				return true
			}
		}
	}
	return false
}
```
If a `TestMeta_*` gate demands a registration or marker, follow its message; it is not a waiver.

**2. ADR 0069.** Header lines:
- `**Status:** Accepted`
- `**Date:** <today>`
- `**Supersedes (in part):** ADR 0055 §4 (approver rule) and §8 (approve/deny inline check); amends ADR 0068 D3 (re-stated in the domain), D4 (moved into commands, symmetric, diff-based) and D5 (permission-request routes stay authentication-only at the route; the authorization now lives in the commands' policy)`
- `**Builds on:** ADR 0036, 0045, 0058, 0062, 0068`

Body:
- **Context:** the five review findings plus the coordinator's re-review, and the owner ruling quoted verbatim.
- **Decision:** H1–H6 and R1–R9 copied from this plan, plus "Rank delivery" and the error-code table.
- **Consequences:**
  - existing tenants are re-tiered only where pristine, with the predicates listed;
  - the data migration writes no outbox rows;
  - `tenant.admin` is opt-in for existing Administrator roles (Q2);
  - 10-minute claim staleness (Q4);
  - Team Lead lacks `read_all` until the follow-up;
  - the approver queue was empty before this change.
- **Follow-up:** the team-scoped visibility feature, a reports-to cycle trigger, and authz-version rotation (Q4).
- `## Fitness function`: `TestArch_AuthorityCommandsCarryActor` (in `internal/architecture/`); the policy decision table `TestPolicy_DecisionTable`; the attack E2E suite (A8).

Add the README row `| 0069 | Rank hierarchy and grant authority — tiers, act-downward, diff-based grant ceiling, approver rule, owner invariants | Accepted |`.

**3. OpenAPI `info.description`:** extend the "Authorization error codes" paragraph with every code in the Decisions table and one sentence on rank and tier ("see `/auth/me/capabilities` `rank`/`tier`").

**4.** Run G1–G6. Commit: `docs(adr): ADR 0069 rank hierarchy and grant authority + authority-command arch gate` (include the RED-proof line).

---

## Task A8 — E2E attack suite in `cmd/api`

**Files:** create `cmd/api/authority_attacks_e2e_test.go`.

**Depends on** A1–A6 and the fixture helpers `newE2EFixture`, `registerAndLogin`, `authedJSON`, `postJSON`, `roleIDByName`, `loginToken`, `mintPlatformToken`, `decodeError`.

```go
//go:build integration

package main

// arch-test:no-timeout-needed — copy the exact marker comment used in cross_tenant_e2e_test.go here.

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/identity/app/jwt"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/ports"
)

type staff struct{ ID, Email, Password, Token string }

// staffWithRole: owner creates a member, assigns roleName (if any), then the
// member logs in (completing a pending first-login password change).
func (f e2eFixture) staffWithRole(t *testing.T, owner registeredTenant, prefix, roleName string) staff {
	t.Helper()
	id := ids.NewV7().String()
	s := staff{Email: prefix + "-" + id[len(id)-8:] + "@e2e.test", Password: "Staff-Pass-2026!" + id[len(id)-8:]}
	resp := f.authedJSON(t, http.MethodPost, "/api/v1/users", owner.AccessToken,
		ports.CreateUserRequest{Email: s.Email, Password: s.Password, FirstName: prefix, LastName: "Staff"})
	if resp.status != http.StatusCreated {
		t.Fatalf("create %s: %d %s", prefix, resp.status, resp.body)
	}
	var cu ports.CreateUserResponse
	_ = json.Unmarshal(resp.body, &cu)
	s.ID = cu.MembershipID
	if roleName != "" {
		f.mustStatus(t, f.authedJSON(t, http.MethodPost, "/api/v1/users/"+s.ID+"/roles", owner.AccessToken,
			ports.AssignUserRoleRequest{RoleID: f.roleIDByName(t, owner.AccessToken, roleName)}), http.StatusNoContent, "assign "+roleName)
	}
	s.Token = f.firstLogin(t, &s)
	return s
}

// firstLogin logs in and, when must_change_password is set (ADR 0053/D8), rotates once and re-logs in.
func (f e2eFixture) firstLogin(t *testing.T, s *staff) string {
	t.Helper()
	resp := f.postJSON(t, "/api/v1/auth/login", ports.LoginRequest{Email: s.Email, Password: s.Password, DeviceLabel: "e2e"})
	var lr ports.LoginResponse
	if resp.status != http.StatusOK || json.Unmarshal(resp.body, &lr) != nil {
		t.Fatalf("login %s: %d %s", s.Email, resp.status, resp.body)
	}
	if !lr.MustChangePassword {
		return lr.AccessToken
	}
	rotated := s.Password + "-r"
	f.mustStatus(t, f.authedJSON(t, http.MethodPost, "/api/v1/auth/change-password", lr.AccessToken,
		ports.ChangePasswordRequest{CurrentPassword: s.Password, NewPassword: rotated}), http.StatusNoContent, "first rotation")
	s.Password = rotated
	return f.loginToken(t, s.Email, s.Password)
}

func (f e2eFixture) relogin(t *testing.T, s *staff) { s.Token = f.loginToken(t, s.Email, s.Password) }

func (f e2eFixture) mustStatus(t *testing.T, r httpResp, want int, what string) {
	t.Helper()
	if r.status != want {
		t.Fatalf("%s: %d %s, want %d", what, r.status, r.body, want)
	}
}

func (f e2eFixture) mustCode(t *testing.T, r httpResp, want int, code, what string) {
	t.Helper()
	if r.status != want || decodeError(t, r.body).Error != code {
		t.Errorf("%s: %d %s, want %d %s", what, r.status, r.body, want, code)
	}
}

func (f e2eFixture) request(t *testing.T, s staff, perm string) string {
	t.Helper()
	resp := f.authedJSON(t, http.MethodPost, "/api/v1/permission-requests", s.Token,
		ports.CreatePermissionRequestRequest{Permission: perm, Reason: "need this for the quarterly push"})
	f.mustStatus(t, resp, http.StatusCreated, "request "+perm)
	var out ports.CreatePermissionRequestResponse
	_ = json.Unmarshal(resp.body, &out)
	return out.RequestID
}

// mintSuperUserToken: synthetic super user scoped to tenantID (mirrors mintPlatformToken).
func (f e2eFixture) mintSuperUserToken(t *testing.T, tenantID, slug string) string {
	t.Helper()
	pid := ids.NewV7().String()
	addr, _ := email.New("super-" + pid[len(pid)-12:] + "@platform.test") // arch-test:ignore-err - test fixture setup
	hash, err := person.NewPasswordHash("$argon2id$v=19$m=19456,t=2,p=1$c2FsdHkx$abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	p, err := person.New(person.ID(pid), addr, "Super", "User", hash, testNow)
	if err != nil {
		t.Fatalf("person: %v", err)
	}
	if err := f.persons.Add(t.Context(), p); err != nil {
		t.Fatalf("persons.Add: %v", err)
	}
	tok, err := f.Issuer.Issue(jwt.IssueArgs{PersonID: pid, TenantID: tenantID, TenantSlug: slug, MembershipID: ids.NewV7().String(),
		SecurityStamp: p.SecurityStamp().String(), IsSuperUser: true})
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	return tok
}

func TestE2E_Authority_DelegateCannotStripOwner(t *testing.T) {
	f := newE2EFixture(t)
	owner := f.registerAndLogin(t, "strip")
	d := f.staffWithRole(t, owner, "delegate", role.SystemRoles.Tenant.Administrator)
	ownerRole := f.roleIDByName(t, owner.AccessToken, role.SystemRoles.Tenant.CompanyOwner)
	u := "/api/v1/users/" + owner.MembershipID
	f.mustCode(t, f.authedJSON(t, http.MethodPatch, u+"/permission-overrides", d.Token,
		ports.ReplaceUserPermissionOverridesRequest{Granted: []string{}, Revoked: []string{"tenant.admin"}}), 403, "insufficient_rank", "strip tenant.admin")
	f.mustCode(t, f.authedJSON(t, http.MethodDelete, u+"/roles/"+ownerRole, d.Token, nil), 403, "insufficient_rank", "revoke owner role")
	f.mustCode(t, f.authedJSON(t, http.MethodPost, u+"/deactivate", d.Token, ports.DeactivateUserRequest{Reason: "hostile takeover"}), 403, "insufficient_rank", "deactivate owner")
	// empty the CompanyOwner role: rank blocks the delegate and the owner; immutability blocks a super user
	empty := ports.ReplaceRolePermissionsRequest{Permissions: []string{}}
	f.mustCode(t, f.authedJSON(t, http.MethodPut, "/api/v1/roles/"+ownerRole+"/permissions", d.Token, empty), 403, "insufficient_rank", "delegate empties owner role")
	f.mustCode(t, f.authedJSON(t, http.MethodPut, "/api/v1/roles/"+ownerRole+"/permissions", owner.AccessToken, empty), 403, "insufficient_rank", "owner empties owner role")
	super := f.mintSuperUserToken(t, owner.TenantID, owner.Slug)
	f.mustCode(t, f.authedJSON(t, http.MethodPut, "/api/v1/roles/"+ownerRole+"/permissions", super, empty), 409, "role_system_default", "super empties owner role")
	// last owner: even a super user cannot remove it
	f.mustCode(t, f.authedJSON(t, http.MethodPost, u+"/deactivate", super, ports.DeactivateUserRequest{Reason: "remove last owner"}), 409, "last_owner_protected", "deactivate last owner")
	f.mustCode(t, f.authedJSON(t, http.MethodDelete, u+"/roles/"+ownerRole, super, nil), 409, "last_owner_protected", "demote last owner")
	// with a second owner, owners manage owners
	o2 := f.staffWithRole(t, owner, "owner2", role.SystemRoles.Tenant.CompanyOwner)
	f.mustStatus(t, f.authedJSON(t, http.MethodPost, "/api/v1/users/"+o2.ID+"/deactivate", owner.AccessToken,
		ports.DeactivateUserRequest{Reason: "second owner leaves"}), http.StatusNoContent, "owner deactivates second owner")
}

func TestE2E_Authority_RoleAtOrAboveOwnRankRejected(t *testing.T) {
	f := newE2EFixture(t)
	owner := f.registerAndLogin(t, "rankrole")
	d := f.staffWithRole(t, owner, "admin", role.SystemRoles.Tenant.Administrator)
	s := f.staffWithRole(t, owner, "exec", role.SystemRoles.Tenant.SalesExecutive)
	assign := func(roleName string) httpResp {
		return f.authedJSON(t, http.MethodPost, "/api/v1/users/"+s.ID+"/roles", d.Token,
			ports.AssignUserRoleRequest{RoleID: f.roleIDByName(t, owner.AccessToken, roleName)})
	}
	f.mustCode(t, assign(role.SystemRoles.Tenant.Administrator), 403, "insufficient_rank", "admin assigns Administrator")
	f.mustCode(t, assign(role.SystemRoles.Tenant.CompanyOwner), 403, "insufficient_rank", "admin assigns CompanyOwner")
	f.mustStatus(t, assign(role.SystemRoles.Tenant.TeamLead), http.StatusNoContent, "admin assigns Team Lead")
	var caps ports.CapabilitiesDto
	_ = json.Unmarshal(f.authedJSON(t, http.MethodGet, "/api/v1/auth/me/capabilities", d.Token, nil).body, &caps)
	if caps.Rank != 10 || caps.Tier != "admin" {
		t.Errorf("admin capabilities rank/tier = %d/%s", caps.Rank, caps.Tier)
	}
}

func TestE2E_Authority_ManagerLinksLateralUpwardRejected(t *testing.T) {
	f := newE2EFixture(t)
	owner := f.registerAndLogin(t, "links")
	m1 := f.staffWithRole(t, owner, "m1", role.SystemRoles.Tenant.SalesManager)
	m2 := f.staffWithRole(t, owner, "m2", role.SystemRoles.Tenant.SalesManager)
	s := f.staffWithRole(t, owner, "s", role.SystemRoles.Tenant.SalesExecutive)
	link := func(sub, mgr string) httpResp {
		return f.authedJSON(t, http.MethodPut, "/api/v1/users/"+sub+"/manager", owner.AccessToken, ports.AssignUserManagerRequest{ManagerID: mgr})
	}
	f.mustCode(t, link(m1.ID, m2.ID), 422, "manager_not_eligible", "lateral")
	f.mustCode(t, link(m1.ID, s.ID), 422, "manager_not_eligible", "upward")
	f.mustStatus(t, link(s.ID, m1.ID), http.StatusNoContent, "downward")
}

func TestE2E_Authority_SockPuppetChainBounded(t *testing.T) {
	f := newE2EFixture(t)
	owner := f.registerAndLogin(t, "puppet")
	x := f.staffWithRole(t, owner, "hr", role.SystemRoles.Tenant.HrManager)
	f.mustStatus(t, f.authedJSON(t, http.MethodPatch, "/api/v1/users/"+x.ID+"/permission-overrides", owner.AccessToken,
		ports.ReplaceUserPermissionOverridesRequest{Granted: []string{"identity.users.create"}, Revoked: []string{}}), http.StatusNoContent, "owner grants users.create")
	f.relogin(t, &x)
	puppet := f.staffWithRole(t, registeredTenant{AccessToken: x.Token}, "puppet", "")
	f.mustStatus(t, f.authedJSON(t, http.MethodPut, "/api/v1/users/"+puppet.ID+"/manager", x.Token,
		ports.AssignUserManagerRequest{ManagerID: x.ID}), http.StatusNoContent, "self-appoint as puppet's manager")
	f.mustCode(t, f.authedJSON(t, http.MethodPost, "/api/v1/permission-requests", puppet.Token,
		ports.CreatePermissionRequestRequest{Permission: "tenant.admin", Reason: "need this for the quarterly push"}), 422, "permission_not_requestable", "request tenant.admin")
	unheld := f.request(t, puppet, "identity.users.update_permissions")
	f.mustCode(t, f.authedJSON(t, http.MethodPost, "/api/v1/permission-requests/"+unheld+"/approve", x.Token,
		ports.ApprovePermissionRequestRequest{}), 403, "permission_request_forbidden", "approve beyond own authority")
	held := f.request(t, puppet, "tasks.work_items.read_all")
	f.mustStatus(t, f.authedJSON(t, http.MethodPost, "/api/v1/permission-requests/"+held+"/approve", x.Token,
		ports.ApprovePermissionRequestRequest{}), http.StatusNoContent, "approve within own authority (bounded)")
}

func TestE2E_Authority_AdminSelfManagerThenApproveBounded(t *testing.T) {
	f := newE2EFixture(t)
	owner := f.registerAndLogin(t, "selfmgr")
	d := f.staffWithRole(t, owner, "admin", role.SystemRoles.Tenant.Administrator)
	s := f.staffWithRole(t, owner, "exec", role.SystemRoles.Tenant.SalesExecutive)
	f.mustStatus(t, f.authedJSON(t, http.MethodPut, "/api/v1/users/"+s.ID+"/manager", d.Token,
		ports.AssignUserManagerRequest{ManagerID: d.ID}), http.StatusNoContent, "admin appoints self")
	f.mustCode(t, f.authedJSON(t, http.MethodPost, "/api/v1/permission-requests", s.Token,
		ports.CreatePermissionRequestRequest{Permission: "platform.tenants.manage", Reason: "need this for the quarterly push"}), 422, "permission_not_requestable", "request platform permission")
	id := f.request(t, s, "crm.leads.assign")
	var queue ports.ListPermissionRequestsResponse
	_ = json.Unmarshal(f.authedJSON(t, http.MethodGet, "/api/v1/permission-requests?role=approver", d.Token, nil).body, &queue)
	if len(queue.Requests) == 0 {
		t.Error("admin approver queue must list the pending request")
	}
	f.mustCode(t, f.authedJSON(t, http.MethodPost, "/api/v1/permission-requests/"+id+"/approve", s.Token,
		ports.ApprovePermissionRequestRequest{}), 422, "permission_request_self_approval", "approve own request")
	f.mustStatus(t, f.authedJSON(t, http.MethodPost, "/api/v1/permission-requests/"+id+"/approve", d.Token,
		ports.ApprovePermissionRequestRequest{}), http.StatusNoContent, "admin approves within tenant.admin")
}

func TestE2E_Authority_ManagerSubordinateCollusion(t *testing.T) {
	f := newE2EFixture(t)
	owner := f.registerAndLogin(t, "collude")
	m := f.staffWithRole(t, owner, "mgr", role.SystemRoles.Tenant.SalesManager)
	s := f.staffWithRole(t, owner, "sub", role.SystemRoles.Tenant.SalesExecutive)
	f.mustStatus(t, f.authedJSON(t, http.MethodPut, "/api/v1/users/"+s.ID+"/manager", owner.AccessToken,
		ports.AssignUserManagerRequest{ManagerID: m.ID}), http.StatusNoContent, "link")
	f.mustCode(t, f.authedJSON(t, http.MethodPost, "/api/v1/permission-requests", s.Token,
		ports.CreatePermissionRequestRequest{Permission: "tenant.admin", Reason: "need this for the quarterly push"}), 422, "permission_not_requestable", "collusion for tenant.admin")
	beyond := f.request(t, s, "identity.roles.assign")
	f.mustCode(t, f.authedJSON(t, http.MethodPost, "/api/v1/permission-requests/"+beyond+"/approve", m.Token,
		ports.ApprovePermissionRequestRequest{}), 403, "permission_request_forbidden", "manager approves beyond authority")
	within := f.request(t, s, "crm.leads.read_all")
	f.mustStatus(t, f.authedJSON(t, http.MethodPost, "/api/v1/permission-requests/"+within+"/approve", m.Token,
		ports.ApprovePermissionRequestRequest{}), http.StatusNoContent, "manager approves held permission")
}

func TestE2E_Authority_ImpersonationIsAdminEquivalent(t *testing.T) {
	f := newE2EFixture(t)
	owner := f.registerAndLogin(t, "impauth")
	s := f.staffWithRole(t, owner, "exec", role.SystemRoles.Tenant.SalesExecutive)
	pending := f.request(t, s, "crm.leads.read_all")
	resp := f.authedJSON(t, http.MethodPost, "/api/v1/platform/impersonation/sessions", f.mintPlatformToken(t, ""),
		ports.CreateImpersonationSessionRequest{TargetTenantID: owner.TenantID, Reason: "support ticket 4711 authority test", DurationMinutes: 30})
	f.mustStatus(t, resp, http.StatusCreated, "open session")
	var sess ports.CreateImpersonationSessionResponse
	_ = json.Unmarshal(resp.body, &sess)
	imp := sess.AccessToken
	f.mustCode(t, f.authedJSON(t, http.MethodPost, "/api/v1/users/"+owner.MembershipID+"/deactivate", imp,
		ports.DeactivateUserRequest{Reason: "support lockout attempt"}), 403, "insufficient_rank", "impersonation deactivates owner")
	f.mustCode(t, f.authedJSON(t, http.MethodPost, "/api/v1/users/"+s.ID+"/roles", imp,
		ports.AssignUserRoleRequest{RoleID: f.roleIDByName(t, owner.AccessToken, role.SystemRoles.Tenant.CompanyOwner)}), 403, "insufficient_rank", "impersonation assigns owner role")
	f.mustCode(t, f.authedJSON(t, http.MethodPatch, "/api/v1/users/"+s.ID+"/permission-overrides", imp,
		ports.ReplaceUserPermissionOverridesRequest{Granted: []string{"tenant.admin"}, Revoked: []string{}}), 403, "grant_exceeds_authority", "impersonation confers tenant.admin")
	f.mustCode(t, f.authedJSON(t, http.MethodPost, "/api/v1/permission-requests/"+pending+"/approve", imp,
		ports.ApprovePermissionRequestRequest{}), 403, "impersonation_not_allowed", "impersonation approves")
	f.mustStatus(t, f.authedJSON(t, http.MethodPost, "/api/v1/users/"+s.ID+"/roles", imp,
		ports.AssignUserRoleRequest{RoleID: f.roleIDByName(t, owner.AccessToken, role.SystemRoles.Tenant.TeamLead)}), http.StatusNoContent, "impersonation assigns Team Lead")
}
```
- `staffWithRole(t, registeredTenant{AccessToken: x.Token}, …)` reuses the helper with the attacker as creator. `roleName == ""` skips the assignment.
- If `persons` / `Issuer` are named differently on `e2eFixture`, use the names `mintPlatformToken` uses.
- Run G1–G6, then `go test -tags=integration -p 1 -count=1 -timeout=15m -run 'TestE2E_Authority' ./cmd/api/... ; echo INT=$?` (expect 0). Then run the full `task test:int ; echo INT=$?`.
- Commit: `test(e2e): rank/grant-authority attack suite — sock-puppet, collusion, owner strip, empty owner role, lateral/upward links, self-approval, impersonation (ADR 0069)`.

---

## Consequences for the web BFF

- **New error codes to handle**, all with an RFC 9457 body and the `error` field:

  | Code | Status | Suggested UI |
  |---|---|---|
  | `insufficient_rank` | 403 | "You can only manage people and roles below your level." |
  | `forbidden` | 403 | |
  | `impersonation_not_allowed` | 403 | |
  | `last_owner_protected` | 409 | "A company needs at least one active owner." |
  | `owner_protected` | 409 | |
  | `role_system_default` | 409 | |
  | `manager_not_eligible` | 422 | |
  | `role_parent_not_eligible` | 422 | |
  | `permission_not_requestable` | 422 | |

  `grant_exceeds_authority` and `permission_request_forbidden` already exist but now fire on the *delta*, so the UI must stop pre-blocking unchanged rows.
- **Capabilities:** read `rank` (int) and `tier` (`owner|admin|manager|team_lead|user`). The UI computes a target's rank as min `hierarchy_level` over the target's `role_ids`, using `GET /roles` (Manager and Team Lead tiers now hold `identity.roles.view`). Hide or disable when `target.rank <= me.rank`; owners may also act on owners. This applies to:
  - role assign/revoke, overrides, deactivate/reactivate, manager change: hide unless `me.rank < target.rank`;
  - role pickers: show only roles with `hierarchy_level > me.rank` (owners: all); `is_super_admin` roles only for super users;
  - permission editors: offer only names in `effective_permissions`; `tenant.admin` only when `tier == owner`;
  - role edit/delete: only roles with `hierarchy_level > me.rank`; new-role level input has minimum `me.rank + 1`, and 0 is never allowed;
  - CompanyOwner role: always read-only;
  - manager picker: candidates more senior than the subordinate, and (non-admins) not more senior than me.
- **Requests UI:**
  - remove `tenant.admin` and platform permissions from the request form;
  - the approver queue (`?role=approver`) now actually returns items (managers: direct reports; admins: everyone below them);
  - show the approve button only when the item's permission is in my `effective_permissions`;
  - request detail is visible to admins and to the current manager;
  - impersonation sessions must hide approve/deny.
- **Directory:** Manager and Team Lead tiers now see `GET /users` / `GET /roles`. Ordinary executives still do not. Slug lookup needs `identity.tenants.view` (Q5): stop using it for plain members' tenant display and use the capabilities `tenant_slug`.
- **Re-login:** rank and permission changes reach a user's token at their next refresh (≤10 min). Prompt a refresh after editing *your own* team, or poll capabilities.

## Follow-up feature: team-scoped visibility for managers (not planned here)

**What exists today:**
- **CRM leads list** (`internal/crm/ports/http.go`): the list is narrowed to the caller's own assignments unless `authn.Satisfies(crm.leads.read_all)`, and `read_all` is **tenant-wide**. `?assignee=` other than self is refused without `read_all`.
- **Tasks:**
  - the work-item list (`internal/tasks/ports/http.go`) narrows to own unless `tasks.work_items.read_all`, which is tenant-wide;
  - `reassign` is hierarchy-gated by the reports-to **subtree**;
  - the dashboard `include_team` counts use the subtree.
  Both go through `tasks.command/query.HierarchyReader`, implemented in `cmd/api/tasks_hierarchy_bridge.go` as a recursive CTE over `identity.tenant_memberships.reports_to` (active only).
- **There is no reporting-line read model** (no projection table, no cache): each call runs the CTE.
- BRD §6.7's `assignerLevel < assigneeLevel` rule is **not** implemented; reassign uses the subtree only.

**What would be needed:**
1. A shared identity read port, `ListSubordinateMembershipIDs`, already shaped by `HierarchyReader`. Promote it to an identity query with a per-request cache, or to an outbox-maintained `identity.reporting_lines` closure table fed by `ManagerAssigned` / `ManagerRemoved` / `MembershipDeactivated` events (ADR 0041).
2. A new scope permission per module, for example `crm.leads.read_team` / `tasks.work_items.read_team`, kept distinct from tenant-wide `read_all`. List handlers would narrow to `subtree ∪ self` when the caller holds the scope permission but not `read_all`.
3. Grant `read_team` to the Manager and Team Lead tiers (H5 v2) and keep `read_all` for Admin-tier (`tenant.admin`) and Senior Manager.
4. Enforce the rank rule on task assignment (`assigner.rank < assignee.rank`) using the ADR 0069 rank. Reports-to links are now rank-ordered, so subtree ⇒ junior holds by construction for new links.
5. An enumeration-safe 404 for out-of-subtree detail reads (ADR 0044).

### Critical Files for Implementation
- `D:\Development\leadkart-go\internal\identity\domain\authority\policy.go` (new — the single policy)
- `D:\Development\leadkart-go\internal\identity\app\command\authority_support.go` (new — lock, facts, locked UoW) with `user_authz.go`, `user_management.go`, `role_management.go`, `permission_request.go`
- `D:\Development\leadkart-go\internal\identity\ports\authn\authority.go` (new — `AuthorityFrom`, claims-only) with `authorize.go`
- `D:\Development\leadkart-go\internal\identity\app\seed\role_catalog.go` with `D:\Development\leadkart-go\migrations\20261119000001_identity_role_tiers.sql`
- `D:\Development\leadkart-go\internal\identity\domain\role\role.go` (H6 immutability, level-0 reservation, `IsCompanyOwner`, `ParseID`)
