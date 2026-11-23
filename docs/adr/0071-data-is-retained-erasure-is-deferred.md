# ADR 0071 — Data is retained; people are active or inactive, companies are active or suspended; privacy erasure is deferred

**Status:** Accepted (owner decision, 2026-11-23).
**Date:** 2026-11-23
**Amends:** [ADR 0003](0003-state-based-persistence-default.md) (the "right-to-erasure is straightforward" consequence), [ADR 0028](0028-security-stamp-cache.md), [ADR 0053](0053-must-change-password-and-account-lockout.md) (the `IsAnonymised` login gate), [ADR 0068](0068-authorization-hardening.md) and [ADR 0069](0069-rank-hierarchy-and-grant-authority.md) (anonymise, mark-for-deletion, hard delete). Those ADRs stay as written: they are the record of what was decided then.

## Context

The owner's words: *"We never delete any data. We keep it all. We just turn the users IsActive true and false and show based on this."* and *"No need to delete anything."* On privacy erasure: *"This is not required for now. We will have a look at it later for the GDPR."*

The API carried three ways of removing a person or a company: anonymising a person (scrubbing the names and flagging the row), marking a company for deletion with a restore window, and hard-deleting a company. Nothing is live and no real data exists, so there is no backfill, no compatibility shim and no legacy tolerance; git keeps the old code.

## Decision

1. **A person is active or inactive.** Deactivating a membership, reactivating it, and the global suspension of a person stay exactly as they are. A person who may not sign in is refused exactly as before, with the same answer as an unknown email (including the failed-login counting on the phantom row). The anonymised state is removed: `identity.persons.is_anonymised` and `anonymised_at`, `Person.Anonymise`, `AnonymisedEvent` and `identity.person_anonymised.v1`, the `AnonymiseUser` and `AnonymisePerson` commands, the permission `identity.users.anonymise`, and the routes `POST /api/v1/users/{userId}/anonymise` and `POST /api/v1/platform/persons/{personId}/anonymise`. `PersonDto` no longer carries `is_anonymised` or `anonymised_at`.
2. **A company is pending, active or suspended.** Suspend and activate stay. The statuses `pending_deletion` and `deleted`, the columns `deletion_scheduled_at`, `deletion_reason` and `hard_deleted_at`, `Tenant.MarkForDeletion`, `RestoreFromDeletion` and `HardDelete`, `TenantRepository.HardDeleteRow`, the tenant events and integration events for them (`identity.tenant_marked_for_deletion.v1`, `identity.tenant_restored.v1`, `identity.tenant_deleted.v1`) with their revoke subscribers, the permission `identity.tenants.delete`, and the routes `POST /api/v1/tenants/{tenantId}/mark-for-deletion`, `POST /api/v1/tenants/{tenantId}/restore` and `DELETE /api/v1/platform/tenants/{tenantId}` are removed. `TenantDto` no longer carries `deletion_scheduled_at` or `deletion_reason`, and its `status` is `pending`, `active` or `suspended`.
3. **All audit history is kept**, as before.
4. **The schema** is changed by two migrations: `20261125000001_identity_persons_drop_anonymised` (the three partial indexes on `identity.persons` are rebuilt on `WHERE is_active`) and `20261125000002_identity_tenants_drop_deletion` (the status check is narrowed without touching a row: a database that still held a tenant in a removed status would fail the migration loudly).

## Privacy erasure is deferred

Erasure is not decided; it is not built, and nothing here is a design for it. It **must be decided before** India's DPDP Act 2023 erasure duty starts (about May 2027) **or** the first customer outside India, whichever comes first. When it is decided it is a new ADR: what is erased, what is kept for tax and audit, and how an erased person stays a foreign-key target for history. Until then the answer to a data-subject request is a decision for the owner, not a feature of the API.

## Consequences

* The web has fewer actions: no anonymise, no company deletion or restore. The route table is in `docs/BFF_API_CHANGES.md` section 4d.
* `PlatformTenantUndeletable` keeps its name and code (`platform_tenant_undeletable`): it now guards only suspension of a tenant holding a SuperAdmin.
* `authn.TenantScopePathOnly` has no route left; it is kept as a generic scope kind with its tests.
* Purely technical expiry stays (idempotency keys, expired tokens, login phantom rows, processed outbox rows). Other code that removes business rows (the 7-year audit purge, the tasks purge, role delete, product soft delete, drug-schedule class delete, role-assignment and override row removal) is untouched and listed in the report of this change for the owner to decide.

## Fitness function

`TestArch_RouteHasSpecOperation` and `TestArch_BFFRouteGateTable` fail if a removed route comes back without an OpenAPI operation and a gate row; `TestArch_NoDropColumnWithoutADRRef` ties the two column-dropping migrations to this ADR. The end-to-end tests `TestE2E_RemovedErasureRoutes_AreGone` and `TestE2E_PersonAndCompanyLifecycle_OnlyActiveInactiveAndActiveSuspended` (integration tag) pin the behaviour.
