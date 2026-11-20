# ADR 0069 — Rank hierarchy and grant authority: tiers, acting downward, the grant ceiling, approvals and owner invariants

**Status:** Accepted
**Date:** 2026-11-20
**Supersedes (in part):** [ADR 0055](0055-permission-elevation-approval-workflow.md) §4 (approver rule) and §8 (approve/deny inline check); amends [ADR 0068](0068-authorization-hardening.md) D3 (re-stated in the domain), D4 (moved into commands, symmetric, delta-based) and D5 (the permission-request routes stay authentication-only at the route; the authorisation lives in the commands' policy)
**Builds on:** [ADR 0036](0036-permission-model.md), [ADR 0045](0045-scoped-jwt-impersonation-act-claim.md), [ADR 0058](0058-role-hierarchy-as-join-table-aggregate.md), [ADR 0062](0062-tdl-test-pyramid-fakes-canon.md), [ADR 0068](0068-authorization-hardening.md)

This ADR describes the system AS BUILT on branch `fix/identity-authz-and-bff-security` at the commit that introduces it. Every rule names the file that implements it. Where a rule is a default taken without an explicit owner ruling it is listed in [Pending owner confirmation](#pending-owner-confirmation) and marked `[pending]` where it is stated.

## Context

Two independent security reviews and four fix rounds found one family of defects: ADR 0068 D4 ("no grant beyond your own authority") was enforced only in the HTTP port, outside the transaction; removals were unbounded; the approval path ignored it; the manager link made someone an approver with no authority check; and every check read facts (the actor's own rank and permissions, a role's holders, the owner set) outside the lock that protects the write. The attack walks that closed them are in `cmd/api/*_e2e_test.go`.

The owner's ruling, verbatim: *"Owners above admin above manager above team lead above users … Admin sits above all. If manager is not there admin can see through everything and can approve everything."* Managers hold the day-to-day work permissions; admins hold tenant-wide authority.

## Decision

### D1 — Rank and tiers

A member's **rank** is the numerically lowest `HierarchyLevel` among their LIVE roles (`internal/identity/domain/authority/rank.go`, `RankOf`). Lower is more senior; seniority is strict (equal rank is not outranking). A member with no live role has `RankNone` (99). Rank comes from roles only: overrides never change it.

| Tier | Rank / band | Seeded roles (`internal/identity/app/seed/role_catalog.go`) |
|---|---|---|
| owner | 0 | Company Owner |
| admin | 1–10 | Administrator (10), Office Administrator (10) |
| manager | 11–20 | Senior Manager (15), Sales / Purchase / Dispatch / HR Manager (20) |
| team_lead | 21–30 | Team Lead (30) |
| user | 31–99 | the Executives (50) |

`Rank.Tier()` maps a rank to its band; a value outside [0, 99] is no rank and reads as the user tier, never owner. `levelOf` counts level 0 as `RankUser` on any role that is not the Company Owner system role, so a stray level-0 custom role fails closed. Level 0 is accepted only for system-default roles (`role.New`, `ChangeHierarchyLevel`); a created or re-levelled role is in [1, 99] for every actor, super users included (`validLevel`, `policy.go`).

**Rank claim and its staleness bound.** The rank travels as the JWT claim `rank`, minted from the same role load as the permission claims at login and refresh (`internal/identity/app/permissions`, `internal/identity/app/command/rank_claim.go`). A token without the claim reads as `RankNone` (fail closed). The claim is at most one access-token lifetime stale (`jwt.AccessTokenTTL`, 10 minutes). `GET /auth/me/capabilities` exposes `rank` and `tier`, computed from the same `authn.AuthorityFrom`, so the UI never has a second opinion.

### D2 — One decision point, one transaction, facts read inside it

The pure package `internal/identity/domain/authority` is the only place an authority decision is made. It imports no `app`, `ports`, `adapters`, SQL or `net/http`, never calls `time.Now()` (callers pass the decision instant) and declares one `var` per error sentinel. The zero `Actor` and the zero `Member` deny every decision; ids are compared only in canonical form.

* `authority.Actor` is built once by the port from verified claims (`authn.AuthorityFrom`, `internal/identity/ports/authn/authority.go`) and carried in every authority command (`Actor authority.Actor`). The actor's tenant comes from the claims, never from the re-bound request scope.
* `authority.Member` is the projection of a membership (`MemberOf`): rank, owner / super-admin flags, whether the person can act, manager, live overrides, and the effective level of each live role. It is built by one loader, `authorityFacts` (`internal/identity/app/command/authority_support.go`; `project`/`memberFrom` for one membership, `projectBatch` for many with a fixed number of reads).
* **Every authority command runs through one writer**, `authorityWriter.locked` (same file): one `UnitOfWork` transaction under the target tenant's RLS scope; the per-tenant advisory lock (`pg_advisory_xact_lock`, `internal/identity/adapters/sql/authority_lock.sql`) is taken FIRST; the acting member is re-read; then the command decides and writes on facts loaded in that transaction. `updateMember` (membership commands) and `updateRole` (role commands) are thin wrappers that end in `locked`. The fitness function in this ADR enforces that no command bypasses it.
* Membership saves take a row lock: `UpdateByID` loads the membership `FOR NO KEY UPDATE` (`GetMembershipByIDForUpdate`, `internal/identity/adapters/sql/memberships.sql`), so a concurrent writer waits and persists on what the previous one committed; `FOR NO KEY UPDATE` rather than `FOR UPDATE` so a foreign-key check on `role_assignments`, `reports_to` or `created_by` does not block. The person row is locked the same way for every `PersonRepository.UpdateByID`.

**Fresh actor under the lock.** `authorityFacts.freshActor` re-reads the acting member's membership, roles and person inside the transaction and `Actor.NarrowedBy` returns the actor the policy decides with: the MORE JUNIOR of the token's rank and the stored rank, and the INTERSECTION of the token's permissions with the ones resolved now (`tenant.admin` expanded on both sides by the one `effectiveSet`). A token can only ever lose authority relative to the stored facts. A missing, inactive or cannot-act actor (inactive membership; person suspended, locked, anonymised or deactivated) becomes the zero actor: 403 `forbidden`. Not re-read (`Actor.NeedsFreshFacts`): super users (no tenant membership), impersonation sessions (synthetic membership, bounded by the session store and the operator's stamp) and operators acting under the cross-tenant exemption (their authority is the platform permission). The same re-read supplies `holdUntil` for the approval expiry cap (D8).

**Stamp rotation on authority changes.** The TARGET person's security stamp is rotated in the same transaction (`Person.RotateSecurityStamp`, event `PersonSecurityStampRotatedV1`, evicted from the stamp cache by the existing subscriber) when an authority change was recorded: role assigned (`role_assigned`), revoked (`role_revoked`), overrides replaced (`overrides_replaced`), deactivated, reactivated, an approval (`permission_approved`, the requester), and a change to the DEFINITION of a role (`role_definition_changed`, every active holder, D6). Idempotent repeats rotate nothing (`Membership.HasPendingEvents`, or the role definition compared before and after). Manager changes, profile edits and denials rotate nothing. No refresh family is revoked by the rotation, so the next request with the old access token answers 401 `stale_token` (`WWW-Authenticate: Bearer realm="leadkart"`), the BFF refreshes silently and the new token carries the new rank and permissions; deactivation additionally revokes families through the existing membership-deactivated cascade. Stamp cache TTL is 30 s (`cache.SecurityStampTTL`); with the cascade down that window is the fallback.

### D3 — Acting on a member

`Actor.MayActOn` (`policy.go`) is the basis of every membership command: a valid actor and member; super users may act on anyone; a member holding a super-admin role only by a super user; self is refused (`ErrSelfTarget`); an exempt operator acts on any member of another tenant; otherwise the member must be in the actor's tenant and the actor must STRICTLY outrank them, except that an owner may act on another owner.

* **Assign / revoke a role** (`MayAssignRole`, `MayRevokeRole`, `mayTouchRole`): `MayActOn`; the role belongs to the target's tenant; the Company Owner role is subject to the ownership gate (D9); a super-admin role only by a super user; the role's level strictly below the actor (owners: any tenant role; exempt actors: any); an assigned role must be live; the role's whole permission set must be within the grant ceiling (D4). A deleted role is revoked on rank alone so its holders can always be cleaned up. Revoking the Company Owner role runs `GuardLastOwner`.
* **Overrides** (`MayChangeOverrides`): `MayActOn`; lifting a revoke needs the Admin tier (`mayUnrevoke`, D4 below) `[pending]`; the DELTA between the member's live snapshot (taken by `MemberOf`, never caller-supplied) and the requested state is checked against the ceiling (names added, removed, re-timed; revoked names added or removed); an owner's `tenant.admin` cannot be revoked (`ErrOwnerProtected`). The command computes the new state inside the transaction: a name that stays a live grant keeps its expiry, every other granted name is permanent (the overrides route has no expiry field); a revoked list is deduplicated and a name in both lists is 422 `validation_failed` before anything is locked.
* **Deactivate / reactivate** (`MayDeactivate`, `MayReactivate`): `MayActOn`; an owner target needs the ownership gate (D9); deactivating also runs `GuardLastOwner`.
* **Managers** (`MayReplaceManager`, `MayClearManager`, `maySetManager`, `mayUnlinkManager`): `MayActOn` the subordinate FIRST (so a caller who may not act learns nothing about the chain), then the manager must exist, be an active member of the same tenant and be STRICTLY more senior than the subordinate, and the actor must be at least as senior as the manager (self-appointment is allowed: it cannot grow authority, because approval needs `identity.roles.approve` and is capped by the approver's own permissions); replacing or clearing needs the actor to be the current manager or to strictly outrank them. Cycles: a bounded walk of the proposed manager's chain (`maxManagerChainDepth` = 64; a deeper chain is refused too), a DB `CHECK (reports_to <> id)` (`migrations/20261120000002_membership_reports_to_not_self.sql`), and approval re-checks that the approver outranks the requester, so a stale upward link is inert.
* **Profile edit of another member** (`UpdateUserProfileHandler`): `MayActOn`; editing oneself is unchanged and decides nothing. No stamp rotation.
* **Person-level guards** (`PersonChangeGuard`, `person_change_guard.go`): global suspension and anonymisation of a person run `GuardLastOwner` for every tenant the person is an ACTIVE member of, each under its own tenant lock, and apply the change inside the last guarded transaction after taking the person row and re-reading the memberships; if the active set changed, the guard restarts (3 attempts, then 409 `person_memberships_changing`).

### D4 — The grant ceiling

`Actor.mayChangePermissions` (`policy.go`) answers "may the actor confer OR remove every name in this set": unknown or empty names are refused for everyone, super users included; a super user may change anything else; `tenant.admin` only by an owner who holds it (or an exempt operator holding `platform.tenants.ownership`, D9); platform-reserved names (`permission.ImpliedByTenantAdmin` false) only by super users; every other name only if the actor holds it (`Actor.holds`: their own effective set; an exempt operator holds what `tenant.admin` implies, never platform-reserved names and never `tenant.admin` itself). Only the DELTA is checked: a role replace checks `Delta(current, new)`; role grant / revoke checks the one name when it changes anything; deleting a role checks the role's whole set (it is removed from every holder).

**Un-revoke `[pending]`.** An explicit revoke is a deliberate denial. Removing a name from a member's REVOKED list, by an overrides change or by approving a request for a revoked name, additionally needs the actor to be Admin tier or above (rank ≤ 10; an impersonation session counts; exempt actors pass) (`mayUnrevoke`); a manager or team lead gets 403 `grant_exceeds_authority`, and an approval stays pending. Putting names ON the revoked list is unaffected.

### D5 — Roles

Role commands (`internal/identity/app/command/role_management.go`) decide through the same writer (`updateRole`, `locked`).

* **System-default roles are immutable for everyone** (`mayMutateRole`, `role.ErrSystemDefault` → 409 `role_system_default`): rename, re-level, delete and every permission change, super users included, so Company Owner always holds `tenant.admin` at level 0. Other seeded roles are ordinary tenant roles.
* Super-admin roles are editable only by super users. A new level is in [1, 99]; the actor must be strictly more senior than the role's current level and its new level unless exempt in that tenant. Creating a role: level in [1, 99] and strictly below the actor in the actor's own tenant (exempt actors: any tenant), initial permissions within the ceiling, an optional parent that is live, in the same tenant and strictly more senior than the child (`parentEligible`).
* **Role-parent edges (ADR 0058) are metadata**: they carry no authority (rank is level-only; the approver chain is `reports_to`). `MaySetRoleParent` requires only that the actor may edit the child and that the parent is live, same-tenant and strictly more senior.
* **Role-in-use**: deleting a role held by an ACTIVE member is 409 `role_in_use` with `errors.active_holders`; a role held only by inactive members is deleted and those assignments are revoked in the same transaction. Authority is answered before role-in-use.

### D6 — Editing a role is acting on everyone who holds it `[pending]`

A role's level need not match its holders' ranks, so the role-level checks alone let a manager confer on or strip permissions from peers and seniors, and let a member make a time-bound permission permanent by granting it to a role they hold. `Actor.mayChangeRoleFor` (`policy.go`) is the ONE place the rule lives: grant, revoke, replace, delete and a re-level are decided with the role's holders (`authority.RoleHolders`, a parameter of `MayGrantRolePermission`, `MayRevokeRolePermission`, `MayReplaceRolePermissions`, `MayDeleteRole`, `MayEditRole`, so no command can leave them out), and the actor must be allowed to `MayActOn` every ACTIVE holder (membership status active, whether or not the person is locked); a holder who is the actor is refused (`self_modification_forbidden`), an owner editing a non-system role they personally hold included. A rename acts on nobody; a re-level to the level the role already has changes nobody.

* **Re-level reporting rule** (`reportingHolds`): after the move every reporting line touching an active holder must stay strictly ordered, else 422 `role_level_breaks_reporting`: the holder strictly junior to their manager, and every direct report strictly junior to the holder. A holder's new rank is the most senior of the new level and the levels of the other live roles they hold. The rule binds every actor (it is data integrity); a manager that was not loaded fails closed; counterparts of any status are checked.
* **Loads and cost** (`authorityFacts.roleHolders`): holders are loaded once per command with a FIXED number of repository reads whatever their count: `ListHoldingRole` (a re-level reads `ListForTenant` once instead, to get managers and reports), one role read, one person read. The stamp rotation is one person write per ACTIVE holder (the per-person security-stamp event requires it) and is the only cost that grows with holders. Tests: `role_holders_test.go` (reads equal over 3 and 30 holders; rotations equal the active holders).
* **Stale tokens end**: `updateRole` rotates every active holder's stamp in the same transaction when the role's permissions, level or deleted state changed (D2). A rename rotates nothing.

### D7 — Last-owner and owner invariants

`GuardLastOwner` refuses deactivating or demoting an owner when no OTHER active owner of the tenant remains (the target's own activity does not matter; an inactive owner, a non-owner, an invalid member or another tenant's owner never counts as "another owner"); it binds every actor, super users included, and the person-level guard applies it across tenants. `guardOwnerOverrides` refuses revoking `tenant.admin` from an owner (`owner_protected`). Tenant registration assigns the first Company Owner outside the writer (a brand-new tenant has no other member; pinned in the fitness function's exception list).

### D8 — Permission requests and approvals

**Requestable set** (`permission.IsRequestable` = `ImpliedByTenantAdmin`; `permissionrequest.New` refuses anything else, approve re-checks the STORED name): the 44 catalogue names below. Everything else (`tenant.admin` and every platform-reserved name) is conferred only by role.

`identity.meta.request_permission_elevation`, `identity.tenants.{view,update,update_settings}`, `identity.users.{view,create,update,deactivate,reactivate,unlock,update_permissions}`, `identity.roles.{view,create,update,delete,assign,revoke,approve}`, `crm.leads.{read,assign,manage,read_all}`, `platform.marketplace.{browse,purchase}`, `platform.lead_credits.read`, `inventory.catalog.{read,manage}`, `inventory.stock.{read,manage}`, `tasks.work_items.{read,read_all,manage,reassign}`, `dispatch.consignment_notes.{read,manage}`, `orders.quotations.{read,manage}`, `orders.orders.{read,manage}`, `orders.payments.{read,record}`, `orders.invoices.read`, `notifications.inbox.{read,manage}`.

**Not requestable** (17): `tenant.admin`, `platform.tenants.{view,create,manage,ownership}`, `platform.users.{view,create,manage}`, `platform.roles.{view,manage}`, `platform.impersonation.manage`, `platform.unverified_contacts.manage`, `platform.lead_credits.topup`, `identity.tenants.{suspend,activate,delete}`, `identity.users.anonymise`.

**Who may decide** (`mayDecide`, `MayApprove`, `MayDeny`): never an impersonation session, never the requester (`ErrSelfTarget`); a super user or an exempt operator in ANOTHER tenant; otherwise the approver must be in the requester's tenant and strictly outrank the requester, and then either (a) be Admin tier or above, or (b) be the requester's manager and hold `identity.roles.approve`. Approving additionally needs the permission to be requestable and:
* manager path: the approver HOLDS the permission;
* Admin path: the approver may approve any requestable permission WITHOUT holding it, except the **must-hold set** `[pending]` (additions this round marked *): `identity.users.{create,update,update_permissions,deactivate,reactivate,unlock}`, `identity.roles.{create,update,delete,assign,revoke,approve}`, `identity.tenants.{update,update_settings}`, `platform.marketplace.purchase`, `orders.payments.record`, `identity.meta.request_permission_elevation`*, `identity.users.view`*, `identity.roles.view`*, `identity.tenants.view`*, `crm.leads.read_all`*, `tasks.work_items.read_all`*, `orders.payments.read`*, `orders.invoices.read`*, `platform.lead_credits.read`* (25 names; `adminMustHold`, `policy.go`; a pristine Administrator holds all of them, the rule matters where an owner restricted an admin-tier role). Owners hold `tenant.admin`, which implies all of them.
* un-revoke: see D4.

**Expiry cap `[pending]`.** An approval never outlives the approver's own grant: when approving REQUIRED the approver to hold the permission (manager path, or an Admin's must-hold name) and the approver holds it only through a time-bound override (not through a role or a permanent grant; a timed `tenant.admin` caps every name it implies), the approved grant ends no later than the approver's (`Actor.CapApprovalExpiry`; `holdUntil` is set from the fresh read by `NarrowedBy`). The request row records the capped expiry.

**Check order** (`ApprovePermissionRequestHandler`): impersonation session (403, answered before anything is read) → the request and its requester exist, else 404 → eligibility to decide at all, else **404** `[pending]` (`eligibleToDecide`: a caller who is not the requester and fails `MayDeny` is told the request does not exist, so the answer is no existence or rank oracle; a requester whose roles do not load is 409 `member_roles_inconsistent` for a caller who could decide anything, 404 for everyone else) → authority over this permission (403 `grant_exceeds_authority`, including the un-revoke rule) → state (409 `permission_request_not_pending`) → requestable (422 `permission_not_requestable`; a legacy stored row stays pending, can be denied or cancelled) → requester still able to receive the grant (409 `permission_request_requester_inactive`). The grant is written on the requester's row as it is under the lock: only the approved name changes, expired override rows are left untouched, and a name leaves the revoked list. Deny uses the same gate and eligibility, without "holds the permission".

**Visibility and the queue.** `GET /permission-requests/{id}`: the requester and anyone who may deny it, else 404 (`AuthorizePermissionRequestViewHandler`). `GET /permission-requests?role=approver` (`ListDecidablePermissionRequestsHandler`): the pending requests the caller may DENY, each with `can_approve` (= requestable and `MayApprove`), decided on the fresh actor so `can_approve` is what approve answers. A caller who can decide nothing (`MayDecideAny`: below the Admin tier without `identity.roles.approve`, an impersonation session) gets an empty page with no read; a decider below the Admin tier (`DecidesWholeTenant` false) is scanned only over their own reports (the filter is in the SQL, `ListPendingPermissionRequestsInTenantPage`); the rest runs in one read transaction: one list, one batched membership read (`membership.Repository.GetByIDs`), one role read, one person read, bounded to a window of 100 requests.

### D9 — Platform operators

* **Cross-tenant exemption** (`operatorExempt`, `exemptOperatorIn`): a slug-anchored, non-impersonation platform operator holding `platform.users.manage` or `platform.roles.manage` acts on members and roles of ANOTHER (canonical, valid) tenant without rank, holding what `tenant.admin` implies there but no platform-reserved name. Inside the operator's own tenant, rank decides like anyone else. Targets that hold a super-admin role stay super-user-only, and the last-owner guard still applies.
* **Ownership gate `[pending]`** (`mayChangeOwnershipIn`, `platform.tenants.ownership`): an exempt operator needs `platform.tenants.ownership` for the decisions that change who OWNS a tenant: assigning or revoking the Company Owner role, conferring or removing `tenant.admin` by role or override, and deactivating OR reactivating an owner. Super users and every actor deciding in their own tenant are unaffected. The permission is platform-reserved, seeded to no role, not requestable. Everything else the exemption allows is unchanged.
* **`platform.impersonation.manage`** (platform-reserved) creates, lists and ends impersonation sessions; a session is **Admin-equivalent** whatever rank it claims (`NewActor` forces rank 10, never platform or super), so it cannot act on owners, assign Administrator or Company Owner, confer `tenant.admin` or remove the last owner, and it never approves (`ErrImpersonationNotAllowed`) or submits requests.

### D10 — The seeded catalogue and the tier migration

New tenants are seeded from `seed.DefaultRoleCatalog` (the table in D1; Team Lead is new; every non-owner role holds `identity.meta.request_permission_elevation`; Administrator and Office Administrator hold `tenant.admin`; Company Owner holds `tenant.admin`; managers and Team Lead hold the day-to-day grants in the catalogue; HR Manager holds `identity.users.update`).

`migrations/20261119000001_identity_role_tiers.sql` brings EXISTING tenants in line, and only where a role is **pristine**: live, not system-default, named a seed name, in the tenant's seed batch (same `created_at` as the tenant's Company Owner), at the old seed level AND with the old seed permission set; level and permissions change together or not at all. Upgraded: Senior Manager 20→15; Sales, Purchase, Dispatch and HR Manager 50→20 with the manager grants; the executives gain the elevation permission; Administrator keeps level 10 and gains the read bundle plus the elevation permission but NOT `tenant.admin` (existing owners opt in with `POST /roles/{id}/permissions/grant`). **Office Administrator is not upgraded** (no silent promotion of clerk-grade holders). A Team Lead role is inserted only if the tenant has no live role of that name (case-insensitive, trimmed) and every live seeded-manager-named role is strictly more senior than 30 after the upgrade. Non-system roles stored at level 0 move to 50; the bootstrap SuperAdmin role moves 0→1; role-hierarchy edges whose parent is no longer strictly more senior than the child and that a run touched are soft-removed (`removed_at`, `removal_reason`). The migration writes no outbox rows. **Audit rows are the rollback ledger**: one `common.audit_log_entry` row (system actor, payload = migration name, role, before and after) per changed or created role and removed edge; `Down` restores a role only while it still equals the recorded after-state and leaves roles edited since, so purging those audit rows makes `Down` leave those roles alone. The tier migration has only been edited in place on this branch, which is safe only because the branch was never applied to a persistent database; if anyone applied it, a follow-up migration is required.

### Error codes

Sentinels are mapped once, in `writeAuthorityError` (`internal/identity/ports/authority_errors.go`) and the per-route handlers.

| Sentinel | Status | `error` code | When |
|---|---|---|---|
| `authority.ErrNoAuthority` | 403 | `forbidden` | zero, cross-tenant or no-longer-active actor; a decision that cannot be taken safely |
| `authority.ErrInsufficientRank` | 403 | `insufficient_rank` | the actor does not strictly outrank the member or role |
| `authority.ErrGrantExceedsAuthority` | 403 | `grant_exceeds_authority` | confers or removes a permission beyond the actor's authority; un-revoke below the Admin tier; an approver who does not hold the permission |
| `authority.ErrSelfTarget` | 403 | `self_modification_forbidden` | the target (or a role holder, or a request's requester) is the actor |
| `authority.ErrOwnershipAuthorityRequired` | 403 | `ownership_authority_required` | an exempt operator changes ownership without `platform.tenants.ownership` |
| `authority.ErrImpersonationNotAllowed` | 403 | `impersonation_not_allowed` | an impersonation session approves, denies or submits |
| `authority.ErrNotApprover` | 403 | `not_approver` | still mapped; approve and deny no longer produce it (404 instead) |
| `authority.ErrLastOwnerProtected` | 409 | `last_owner_protected` | the tenant would lose its last active owner |
| `authority.ErrOwnerProtected` | 409 | `owner_protected` | `tenant.admin` revoked from an owner |
| `role.ErrSystemDefault` | 409 | `role_system_default` | any change to a system-default role |
| `role.ErrDeleted` | 409 | `role_deleted` | assigning or mutating a deleted role |
| `command.RoleInUseError` | 409 | `role_in_use` (`errors.active_holders`) | deleting a role an active member holds |
| `command.ErrMemberRolesInconsistent` | 409 | `member_roles_inconsistent` | a member's assigned roles cannot be loaded as the tenant's roles |
| `command.ErrPersonMembershipsChanging` | 409 | `person_memberships_changing` | the person guard's memberships kept changing; retry |
| `authority.ErrManagerNotEligible` | 422 | `manager_not_eligible` | manager inactive, foreign, not more senior, missing or a cycle |
| `authority.ErrRoleParentNotEligible` | 422 | `role_parent_not_eligible` | parent not live, foreign or not more senior |
| `authority.ErrRoleLevelBreaksReporting` | 422 | `role_level_breaks_reporting` | a re-level would invert a reporting line |
| `permissionrequest.ErrPermissionNotRequestable` | 422 | `permission_not_requestable` | a request or a stored legacy row for a non-requestable name |
| `command.OverridesConflictError` | 422 | `validation_failed` (`errors.revoked`) | a name in both overrides lists |
| `command.ErrPermissionRequestNotFound` | 404 | `permission_request_not_found` | no such request, or the caller may not decide or see it |
| `command.ErrPermissionRequestNotPending` / `...RequesterInactive` | 409 | `permission_request_not_pending` / `permission_request_requester_inactive` | state |
| stale access token | 401 | `stale_token` (+ `WWW-Authenticate`) | the person's security stamp rotated |

### Lock order

One order everywhere, so no cycle: **(1)** the tenant authority advisory lock (`pg_advisory_xact_lock('identity.authority:' || tenant)`), **(2)** the permission-request row (approve / deny) or the role row (role commands), **(3)** membership row(s) `FOR NO KEY UPDATE`, **(4)** person row(s) `FOR NO KEY UPDATE`. Every multi-row writer holds (1), so two of them cannot interleave; a writer that holds a row never waits for (1) or for another membership row. Writers that take only one membership row (profile edits take (1) too) never wait on a later lock. `FOR SHARE` is not used on fact loads (it would invite share-to-update deadlocks between two reactivations of one person in different tenants).

## Pending owner confirmation

These are defaults taken without an explicit ruling. Each is implemented and tested; reversing one is a change to one function named here.

1. **Role edits act on holders** (D6, `mayChangeRoleFor`): changing what a role confers, deleting it or re-levelling it needs authority over every active holder and is refused when the actor holds the role, an owner included; and the **re-level reporting rule** with its strict after-state check and 422 `role_level_breaks_reporting`.
2. **Un-revoke needs the Admin tier** (D4, `mayUnrevoke`): a manager or team lead who holds the name still cannot lift an explicit revoke, by overrides or by approval.
3. **Must-hold additions** (D8): the nine names added to `adminMustHold` this round.
4. **Approval expiry cap** (D8, `CapApprovalExpiry`): a grant approved on a time-bound holding ends with the approver's own.
5. **404 for ineligible callers on approve and deny** (D8, `eligibleToDecide`), with the impersonation session as the one exception (403 `impersonation_not_allowed`, about the session not the request, answered before any read).
6. **`platform.tenants.ownership`** (D9): the permission's name and the exact list of gated actions, including reactivating an owner.
7. **Earlier plan defaults R1–R9 and Q2, Q3, Q5, Q6** (`docs/plans/2026-11-19-rank-hierarchy-and-grant-authority.md`): Senior Manager at 15 and a new Team Lead role (R1); level 0 reserved to system roles (R2); the exemption definition for operators (R3); self-target denied in the policy (R4); `tenant.admin` conferrable only by owners (R5); exempt operators' ceiling (R6); `identity.roles.approve` as the approver capability (R7); seeded non-owner roles editable within the ceiling (R8); 409/422 for state and input errors (R9); no `tenant.admin` for existing Administrators (Q2); Office Administrator left unchanged in the migration though new tenants seed it at 10 with `tenant.admin` (Q3); slug lookups gated by `identity.tenants.view` (Q5); `CreateUser` carries no actor (Q6). No ruling on these is recorded in the repository.
8. **Stale-token handling**: the plan's Q4 default (trust the claim for ten minutes) was replaced by the fresh read under the lock and stamp rotation; the other modules' route gates still read the token, so they are stale for at most the stamp-cache window (30 s with the cascade down, immediately with it).

## Consequences

* **Front-end.** Hide an action on a member whose `rank` is not strictly greater than the caller's (owners: also on owners) (`/auth/me/capabilities` `rank`, `tier`). Treat 401 `stale_token` with `WWW-Authenticate` as "refresh silently": the session survives an authority change and the refreshed token carries the new rank and permissions; a deactivated member's refresh is refused. The approver queue items carry `can_approve`: enable Approve only when it is true, Deny always. Approve and deny answer 404 to a caller who may not decide the request. Role edits can answer 403 `insufficient_rank` / `self_modification_forbidden` (holders), 422 `role_level_breaks_reporting`, 409 `role_in_use`. New 409 `person_memberships_changing` is retryable.
* Existing tenants are re-tiered only where pristine; Administrators of existing tenants do not gain `tenant.admin`; custom roles keep their levels and permissions.
* A role edit ends every active holder's session token (they refresh); an approval ends the requester's.
* Team Leads and managers do not see their team's leads or tasks until team-scoped visibility exists (Team Lead has no `read_all`); the approver queue was empty before this work.

## Open follow-ups

* **Team-scoped visibility** for managers and team leads (hierarchy-scoped lists); today `read_all` is tenant-wide.
* **Optional expiry on the overrides route** (`PATCH /users/{id}/permission-overrides` makes every new grant permanent; time-bound grants arise only from approvals).
* **Hard-delete purge saga** for tenants (`hard_delete_tenant`), so memberships, roles and their audit rows leave together.
* Reactivating a membership of a person who cannot act (suspended, anonymised) is still allowed; the person guard closes the check-then-change race, not that.
* Operator-in-platform-tenant and impersonation actors are not re-read; their authority can be stale up to their token's life.
* Route-level permission checks in other modules read the token (the 30 s stamp-cache window with the cascade down).
* A reports-to cycle trigger in the database (today: command-level bounded walk plus the self-reference `CHECK`).

## Alternatives considered

* **Port-level checks (the original D4).** Rejected: outside the transaction, so every check-then-act race (last owner, demoted actor) stayed open, and every route repeated the rule.
* **A rank per permission or per resource.** Rejected: the owner's ruling is a single ladder; permissions stay a separate axis (ADR 0036).
* **Optimistic concurrency instead of an advisory lock.** Rejected: the decision reads many rows (owners, holders, manager chain) that no single version column covers.
* **Re-resolving the actor per request instead of under the lock.** Rejected: the cost lands on every request and still races the write.
* **Rotating a per-membership "authz version" instead of the person's security stamp.** Rejected: a second invalidation mechanism; the stamp, its event and its cache subscriber already end tokens, and not revoking refresh families keeps the session alive.
* **Role edits that ignore holders, relying on the role's level.** Rejected: a member can hold a junior role next to a senior one, so the level says nothing about who the edit reaches.

## Fitness function

* `TestArch_AuthorityWritesGoThroughTheAuthorityWriter` (`internal/architecture/authority_arch_test.go`): every write of role assignments, overrides, membership status, manager links (`memberships.UpdateByID`), roles and their permissions (`roles.UpdateByID`, `roles.Add`) and permission-request decisions (`requests.UpdateByID`) in `internal/identity/app/command` sits inside a function literal handed to `locked`, `updateMember` or `updateRole`; the two exceptions (first owner of a new tenant, a requester cancelling their own request) are pinned EXACTLY with reasons; nothing in `ports` or `app/query` may call those writes. Proof it bites: `TestAuthorityWriteScanner_FlagsABypass` (synthetic source) and a temporary bypass of the writer in `ReactivateUser`, recorded in the commit message.
* The policy decision tables in `internal/identity/domain/authority` (`TestPolicy_DecisionTable` and `round5_test.go`), the command tests in `internal/identity/app/command`, and the attack end-to-end suites in `cmd/api` (`role_holders_e2e_test.go`, `stale_actor_e2e_test.go`, `permission_request_*_e2e_test.go`, `operator_exemption_e2e_test.go`, and the race tests under `-count=20`).

## Sources

`docs/plans/2026-11-19-rank-hierarchy-and-grant-authority.md` (the plan this ADR supersedes where it differs), ADR 0036, 0044, 0045, 0055, 0058, 0062, 0068, and the owner's ruling quoted in Context.
