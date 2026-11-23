# ADR 0070 — Company commerce settings: one owner module, a versioned policy, event-fed replicas

**Status:** Accepted; text is as built by round G1 on branch `fix/commerce-integrity`.
**Date:** 2026-11-23
**Amends:** the "8 bounded contexts" map in `CLAUDE.md` (there are now nine: Identity, Platform, CRM, Orders, Inventory, Dispatch, Tasks, Notifications and Company settings).
**Builds on:** [ADR 0001](0001-modular-monolith.md) (modules do not import each other), [ADR 0006](0006-multi-tenancy-rls.md) (RLS), [ADR 0038](0038-pagination-strategy.md) and the web contract rule that a mutation returns the updated resource, [ADR 0064](0064-outbox-as-relay-and-watermill-forwarder.md) and [ADR 0067](0067-canonical-watermill-cqrs-messaging-stack.md) (outbox and cqrs), [ADR 0069](0069-rank-hierarchy-and-grant-authority.md) (the Administrator tier rule).
**Design source:** `leadkart-web/.superpowers/sdd/commerce-settings-design.md` sections 0, 1 and 5 (round G1 of section 6).

## Context

The commerce review found business rules scattered as constants and per-module defaults: the expiry cut-off, the shortage choices, the GST rate of a product, the invoice prefix. The owner's ruling: **business rules are company settings, with the options common in India and worldwide; only the company's Owner / Admin tier changes them; integrity and legal invariants are not settings.** Nothing is live, so there is no backfill, no compatibility shim and no legacy tolerance: every tenant gets its policy through the normal path.

Orders and Inventory both read these rules inside their own business transactions. A synchronous read from one module into another on a write path would take a second pooled connection while a row lock is held, the shape of review finding C1.

## Decision

### D1 — One owner module, `companysettings`

A new module `internal/companysettings` (schema `companysettings`, event topic `companysettings.events`) owns every company commerce setting. Layout is the TDL ports/adapters one: `domain/commercesettings` (the `Settings` aggregate, domain events, `Repository`), `app` (commands, queries, the daily job), `ports` (HTTP, subscribers), `adapters` (pgx and sqlc), `integrationevents`.

The design text writes the schema and event alias as `company_settings`. The architecture gates equate module directory, schema and alias prefix (`db_schema_arch_test`: a module's SQL may only name its own schema; `TestArch_TopicNamingConvention`: `^[a-z]+\.`), so schema, alias and topic are `companysettings`. The permission names the web sees stay as designed (`company_settings.read`, `company_settings.manage`, `company_settings.tax.manage`), because permissions are not module-directory names.

### D2 — The policy is a value object shared by three modules

`internal/common/commercepolicy` (stdlib, `common/humantext`, `common/textlimit` only, the `common/parcel` precedent) holds the `Policy` value object, `Default()`, and a **registry** of every patchable setting: its key (`<group>.<name>`), kind, options or bounds, whether the tax permission is needed. The registry is the single source for validation, the changed-key list, the permission split and the option metadata the web renders controls from (`Describe`). Nothing is half-applied: `Apply` validates the whole result, including the rules that span settings (the shortage default must be an allowed choice; `payment.advance_value` must fit `payment.advance_mode`).

**A tenant with no stored row reads the defaults through one function:** `commercepolicy.Initial()` is `Default()` at version 0. `companysettings`, the Orders replica and the Inventory replica all call it; there is no second place a default is written. Defaults fixed by the owner: `expiry.stop_months_before = 1`; **the GST rate list starts empty, no default rate**; `drug_schedule.mode = optional` (off / optional / required).

The settings are those of design section 1.2: expiry (`stop_months_before` 0..24, up to four `warning_days`), fulfilment (allowed shortage choices and the default, hold behaviour, invoice timing), pricing (rate override, discount and its cap, schemes and their valuation), `invoice.round_off`, tax and documents (credit-note split, annual return filing dates, document prefixes, e-invoice mode, e-way enforcement), payments and credit (advance, part payments, overpayment, dispatch gate, completion, credit limit, terms and overdue handling) and the drug schedule mode. **Nothing enforces them yet**: G2 onward reads them (see the table in the G1 report).

The shape is a registry plus a jsonb document, and a stored document is decoded ONTO the defaults (`commercepolicy.Decode`), so a setting added later is one registry row and a default with no schema change and no rewrite of stored rows: a key the row predates takes its default. The live policy is still validated on read; the immutable history is read as stored (`DecodeStored`, no `Validate`), so a bound tightened in a later release never makes an old row unreadable (review M3).

### D3 — Versioned, with one stale-save rule

Every accepted change moves the stored policy to the next version. `PATCH /api/v1/company/commerce-settings` carries `expected_version`; a stale one is `409 settings_version_conflict` with `current_version`, and **nothing changes**. The check runs on the row locked `FOR UPDATE` inside the saving transaction, so two saves cannot both pass it (`TestSettingsRepository_ConcurrentSavesOnTheSameVersionOneWins`). A patch equal to the current policy does not move the version. A tenant with no row is at version 0; the first stored version is 1. The response is the updated policy (ADR 0038).

### D4 — History is append-only

`companysettings.policy_history` has one row per version: the whole policy, the changed keys, the acting membership, the impersonating operator (the RFC 8693 `act.sub`) if any, the reason, the time. The application role may only INSERT and SELECT (granted by `20261123000004_companysettings_grants`, a module-owned grants migration because the base grants migration may not name a later schema and the documented recovery for a late-created role runs both; `pg.TablePrivilegeExceptions` and the grants test pin it). A reason is required whenever a tax, document or compliance setting changes (the settings marked `tax.manage`), and for every GST rate change. The history row is written in the same transaction as the change and the outbox event.

### D5 — The GST rate list is the company's own and part of the policy

`companysettings.gst_rates`: rate in basis points, a label, the person who confirmed it and when, active or not. LeadKart does not check a rate against the law. A rate is deactivated, never deleted; its rate never changes (add a new one); the same rate under the same label (case-insensitive) is refused as `duplicate`; the list is capped at 50. The list is part of the policy: a change to it moves the policy version and rides the same event, so Orders and Inventory see the rates a product may use. The settings PATCH refuses `tax.gst_rates` (`managed_elsewhere`).

### D6 — Orders and Inventory read their own replica, inside their own transaction

`companysettings.policy_changed.v1` carries the **whole policy** and its version. Orders and Inventory each keep `<module>.policy_replica` (one row per tenant, RLS + FORCE) and a subscriber that stores the policy `WHERE version < EXCLUDED.version`. So:

* a duplicate delivery changes nothing (same version);
* a reordered delivery changes nothing (an older version never replaces a newer one);
* a skipped version is fine (the policy is whole, never a delta);
* a malformed policy errors in the subscriber (retry, then the durable dead-letter queue) and is never stored.

**Delivery.** The in-process bus blocks `Publish` until a subscriber acknowledges (`messaging.NewPubSub`), and the worker starts the outbox forwarder only after the router is running: the forwarder deletes an outbox row as soon as its `Publish` returns, so a non-blocking bus lost an event whose handler was still running at shutdown, with no outbox row and no dead letter (review H2; the test fails with the non-blocking configuration). The subscribers take the tenant from the message ENVELOPE and refuse, as a permanent failure, a payload that names another tenant (review L1).

The replica is read through `pg.TxFromContext` (`ReadPolicy`): it joins the transaction the caller already holds, so a business handler holding a lock never takes a second pooled connection (`TestPolicyReplica_ReadsInsideTheCallersTransactionWithoutASecondConnection` runs it on a one-connection pool). There is no cache on this path. A tenant with no row reads `commercepolicy.Initial()`. Dispatch needs no replica: Orders gates dispatch.

An order will snapshot the policy at approval (design 1.3); the snapshot is built in round G5. The replica returns the `Version` with the policy so the snapshot can record it.

### D7 — Seeding and anti-entropy

A registered tenant gets its policy through the normal path: a subscriber on `identity.tenant_registered.v1` stores version 1 (the defaults) with a history row and a `policy_changed` event, so the replicas learn it like any other version; a replay finds version 1 and acknowledges. A daily River job (`companysettings.republish_policies`) lists every tenant with a stored version (the module's one cross-tenant read, under platform scope) and re-announces the current version under its OWN alias `companysettings.policy_republished.v1` (applied by the same replica code, no history row, left out of the tenant audit timeline so it is never mistaken for a change; review M2), so a dead-lettered event cannot leave a replica stale for long. The job runs **when the worker starts** as well as every 24 hours: River keeps no schedule across restarts, so an interval-only job never ran for a worker deployed more often than the interval (review H1; the schedule is tested). A tenant that fails is logged and left to the next run: failing the job would have River retry the whole sweep up to 25 times (`TestE2E_CompanySettings_AChangeReachesBothReplicasAndReplayIsHarmless` makes Orders miss events and watches it converge).

### D8 — Permissions: only the Owner / Admin tier writes

Three permissions in the closed catalogue: `company_settings.read`, `company_settings.manage` (the non-tax settings) and `company_settings.tax.manage` (the tax, document and compliance settings and the GST list). `manage` and `tax.manage` join `authority.AdminTierOnlyPermissions` beside `identity.users.create`: they are **held and conferred only at the Administrator tier or above**, so a grant below the tier is inert, never reaches `effective_permissions` and never passes a gate (`authn.Satisfies`, `authority.MayHoldAt`). `tenant.admin` implies them for the Owner and the new tenant's Administrator. The aggregate enforces the split per changed key (`ManageRequiredError`, `TaxManageRequiredError`, both 403), so the route gate (either write permission) is only the first line. `company_settings.read` is not tier-limited. The GST list is also readable with `inventory.catalog.read` or `.manage` (the picker of a product form). The routes honour no foreign `X-Tenant-Id` write: an operator uses an impersonation session.

### D9 — What is not a setting

Integrity and legal invariants stay in code: nothing is allocated, packed or dispatched past the printed expiry month; net rate plus GST never exceeds the batch MRP; invoice numbers are gapless and at most 16 characters whatever the prefix; issued documents are immutable; and the rest of design section 1.4. The setting `expiry.stop_months_before` starts at 0, which is the legal limit: there is no negative value and no grace period.

## Consequences

* A change is visible to Orders and Inventory after the event is delivered (at-least-once, eventually consistent), not instantly. Rules that must not lag (the expiry cut-off) are read from the replica in the same transaction as the decision; a company that tightens a rule sees it bind from the version the replica holds.
* **Retention (review L2):** a tenant that has a stored policy cannot be hard-deleted, because `policy_history` is append-only for the application role. The owner has ruled that data is never deleted; the future of tenant hard delete is an open question for the owner. Nothing was changed here.
* DECIDED IN G2: `drug_schedule.mode = off` is refused on the save itself while any product carries a schedule class (a read port bound at the composition root), and Inventory also refuses a schedule on a product while the mode is off, because the replica lags and the two writes can cross.
* SUPERSEDED (the G1 note): `drug_schedule.mode = off` is stored but its design rule (refused while any product is marked H, H1 or X) is not enforced in G1: no product carries a drug schedule yet. G2 adds the product field; the refusal needs a check that `companysettings` cannot make without importing Inventory, so G2 must decide between a refusal in Inventory when a product is scheduled while the mode is off and a read port. Recorded here so it is not forgotten.
* Open owner questions of the design (section 7) that this round does not decide. The policy shape is a registry plus a document, so either answer fits without a schema change: Q2 (block a drug sale to a buyer with no licence number at confirm) and Q3 (refuse Schedule X on orders until the register ships) would each be one registry row; Q1 (one payment settling several invoices), Q4 (more than one GSTIN or warehouse) and Q5 (e-invoice API) are not settings of this module.
* CORRECTED: the Orders tables ARE FORCEd (`FORCE  ROW LEVEL SECURITY`, two spaces, in the init migration). The gap was the architecture gate, whose regexp matched one spelling and whose schema list left out orders, crm, dispatch, tasks and notifications. The gate now reads any whitespace and covers those schemas, and a catalog test asserts `relforcerowsecurity` on every RLS table (review M7).

## Verification

Unit: `internal/common/commercepolicy`, `internal/companysettings/...`, `internal/orders/app/command`, `internal/orders/ports/subscribers`, the Inventory twins. Integration (`-tags=integration`): the repository, history reader, tenant lister, both replicas (guard, RLS, one-connection read), the migrations and privileges tests. End to end (`cmd/api`): the routes (`company_settings_e2e_test.go`) and the proof (`company_settings_replicas_e2e_test.go`). Architecture: `task test:arch` (route and spec drift, RLS+FORCE for the new schema, the append-only history grant).

## Fitness function

* `TestArch_RouteHasSpecOperation` (`internal/companysettings/ports/route_spec_test.go`): every `/api/v1/company/*` route is a spec operation and the reverse.
* `TestArch_EveryTenantTableHasRLSAndForce` (`internal/architecture/multi_tenancy_arch_test.go`): the `companysettings` schema is in the tenant-schema set, so each of its tables enables and FORCEs RLS in its migration.
* `TestArch_AllRegisteredEventsSatisfyMarker` and `TestArch_AllRegisteredEventsHaveCanonicalAlias` (`internal/companysettings/integrationevents/arch_test.go`): every event is tenant-scoped and named `companysettings.<event>.v<N>`.
* `TestArch_DomainHasNoInfraImports` (`internal/architecture/persistence_arch_test.go`): the aggregate imports only the shared pure packages (`commercepolicy` is on its allow-list).
* `TestAppRoleHasPrivilegesOnEveryApplicationTable` (`internal/common/pg/migrations_test.go`, integration): the application role holds SELECT and INSERT only on `companysettings.policy_history`.
* `TestAdminTierOnly_TheSetIsExactlyUsersCreateAndTheCompanySettingsWrites` and `TestSatisfies_CompanySettingsWritesAreAdminTierOnly` pin the Owner / Admin tier rule.
