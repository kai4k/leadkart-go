# ADR 0073 — Inactive, never deleted: products, roles and drug schedule classes

**Status:** Accepted (owner decision, 2026-11-24).
**Date:** 2026-11-24
**Extends:** [ADR 0071](0071-data-is-retained-erasure-is-deferred.md) (people and companies were made active or inactive; this does the same for the business records that still had a delete route). **Amends:** [ADR 0061](0061-inventory-module-aggregates-and-events.md) (the product soft delete and its `product_has_live_stock` guard), [ADR 0069](0069-rank-hierarchy-and-grant-authority.md) (the role delete and its permission `identity.roles.delete`), [ADR 0070](0070-company-commerce-settings-module.md) (removing a drug schedule class). Those ADRs stay as written.

## Context

The owner's words: *"All become Inactive."* LeadKart never deletes a business record. A soft delete that is only labelled "delete" does not count: the row stays but the record disappears from every read, its name is freed, and nothing can bring it back. Three resources still had a delete route:

* **Products:** `DELETE /inventory/products/{id}` hid the product everywhere (`is_deleted`) once no live batch held stock.
* **Roles:** `DELETE /roles/{id}` hid the role (`is_deleted`) once no ACTIVE member held it, and revoked its assignments from the INACTIVE holders.
* **Drug schedule classes:** `DELETE /company/drug-schedule-classes/{id}` removed the class from the company's list once no product carried it.

Nothing is live, so there is no backfill, no compatibility shim and no legacy tolerance; git keeps the old code.

## Decision

1. **One lifecycle for all three: active or inactive, both directions.** Each gets `POST .../deactivate` and `POST .../reactivate`, both answering `200` with the updated record (ADR 0038), both idempotent (repeating one is a no-op: no event, and for a class no new policy version). The transition is a method of the aggregate (`Product.Deactivate` / `Activate`, `Role.Deactivate` / `Activate`, `Settings.DeactivateScheduleClass` / `ReactivateScheduleClass`), each emitting its own domain event into the outbox (`ProductDeactivatedV1` / `ProductReactivatedV1`, `RoleDeactivatedV1` / `RoleReactivatedV1`, and a class change is a new company-policy version with its reason in the history). The delete routes, the `SoftDeleted` / `Deleted` events and `ProductSoftDeletedV1` / `RoleDeletedV1` are removed.
2. **Lists take `?status=active|inactive|all` (default `active`)** and return `status_counts {active, inactive}` (whatever the filter is; on the first page only for the paginated product list), on the one vocabulary `internal/common/lifecycle` that the members list already used. Anything else is 400 `invalid_status_filter`.
3. **The soft-delete columns go.** `inventory.products` and `identity.roles` lose `is_deleted`, `deleted_at`, `deleted_by`; roles gain `is_active` (products already had it). The partial indexes that carried `WHERE NOT is_deleted` are rebuilt. A record deleted before the migration becomes INACTIVE, never active again. A SKU and a role name are unique over every record of the company, inactive ones included: a name is no longer freed by "deleting".
4. **PATCH no longer toggles `is_active`** on a product (one way to change status, with its own event).

## What "inactive" means, per resource

| | Inactive means | Unchanged |
|---|---|---|
| **Product** | no new batch (`409 product_inactive`); the reorder alert skips it | readable, editable, its batches and stock, every existing order and invoice line, the expiry alerts (stock still expires) |
| **Role** | confers nothing (no permission, no rank); no new holder (`409 role_inactive` on assign); not editable; not a parent; hidden from the default list | readable (`GET`), its assignments, its history |
| **Drug schedule class** | cannot be newly set on a product (`422 errors.drug_schedule: [inactive]`) | a product that already carries it keeps it, and its label still resolves; renaming still works |

**An order or quotation line for an inactive product is not blocked here.** Orders and quotations carry a `product_id` string and nothing in Orders (or anywhere) validates it against the catalogue today (lines arrive with round G5). The rule "an inactive product cannot be added to a new order or quotation" has nothing to attach to yet; it belongs to G5 and needs a catalogue port into Orders. Reported, not built.

## Roles: what delete did, and what deactivate does (security)

What delete did to the members who held the role (read from `DeleteRoleHandler`, `MayDeleteRole` and the authority-facts loaders, not guessed):

* the caller needed authority over the role AND over every holder, and the ceiling over every permission the role carries (`MayDeleteRole`, answered before the in-use rule);
* a role with an ACTIVE holder was refused, `409 role_in_use`, nothing changed; so a delete never removed permissions from an active member;
* with only INACTIVE holders left, the role was marked deleted and their assignments were revoked in the same transaction;
* a deleted role conferred nothing: every authority read skips it (`RankOf`, the permission resolver `GetByIDs`, `ListSuperAdminMembershipsInTenant`, the permission-holder query, `MayAssignRole`, `parentEligible`).

Deactivate keeps every one of those rules and loosens none:

* **Same decision, renamed:** `Actor.MayDeactivateRole` is `MayDeleteRole` unchanged, and the permission is renamed, not weakened: `identity.roles.delete` became `identity.roles.manage` with exactly the same holders (the `tenant.admin` implication set and the owner-only ceiling). The web's catalogue changes by that one name.
* **Same in-use rule:** an active holder is `409 role_in_use`.
* **An inactive role confers nothing**, through the same reads a deleted role was skipped by (`IsActive` replaces `IsDeleted` in each). SQL that decided who holds a permission or a super-admin role filters `is_active`.
* **One difference, tighter on data and equal on authority:** the assignments of the INACTIVE holders are kept (nothing is deleted). Consequence: such a member may be reactivated while the role is inactive; they receive nothing from it (proved end to end). When the role is reactivated they get it back, which is why reactivating asks `MayDeactivateRole` again (the same authority over the role's permissions and every holder, active or not) and rotates every active holder's security stamp, so their tokens carry the restored permissions at once.
* A race between deactivating and assigning is serialised by the tenant authority lock: whichever runs second sees the other's result (the assign is refused `role_inactive`, or the deactivate `role_in_use`), so that race never leaves an active member holding an inactive role (asserted in the race e2e). An active member CAN hold an inactive role by another path: a holder who was inactive when the role was deactivated is later reactivated. That member receives nothing from the role.

## Consequences

* The web: remove the three delete actions; add Deactivate / Reactivate; add the status chips and `status=` filter to the three lists; rename `identity.roles.delete` to `identity.roles.manage` in its permission catalogue; `RoleDto.is_active`, `ScheduleClassDto.active`; `PATCH /inventory/products` no longer takes `is_active`. The route table is in `docs/BFF_API_CHANGES.md` section 4e.
* Quotations already follow this: a draft is cancelled with `POST /orders/quotations/{id}/reject`, which keeps the row (state `rejected`); nothing to build.
* Batches are never deleted either (controller ruling on the owner's never-delete rule): `Batch.SoftDelete` (called only by domain tests; no handler, route or job ever used it), `ErrDeleted` and the `inventory.batches` columns `is_deleted`, `deleted_at`, `deleted_by` are removed (migration 20261125000007; a batch deleted earlier is visible again, a duplicate batch number takes its id). Stock is corrected by movements: write-off, quarantine, adjustment. The arch gate covers the batch table model and domain type.
* A stored company policy written before this change has no `active` on its drug schedule classes and would read every class as inactive; there is no live data, so no shim was written.

## Fitness function

`TestArch_InactiveNotDeleted_NoDeleteRouteOnTheThreeResources` (in `internal/architecture/`) fails when a module registers a `DELETE` route for a product, a role or a drug schedule class, or when the product or role table models carry a soft-delete column again. `TestArch_RouteHasSpecOperation` keeps `api/openapi.yaml` and the routes in step, and the per-resource end-to-end tests in `cmd/api/inactive_not_deleted_e2e_test.go` (deactivate, hidden from the default list, shown by `status=inactive`, the effect enforced, reactivate) plus the role security proofs in `role_authority_e2e_test.go` carry the behaviour.
