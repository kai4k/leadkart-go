# ADR 0065 — Lead marketplace: multi-buyer, tiered, dynamic pricing

**Status:** Accepted
**Date:** 2026-05-29
**Supersedes (in part):** [ADR 0059](0059-platform-module-aggregates-and-events.md) — the single-buyer `PlatformLead` model.

## Context

ADR 0059 modelled a verified lead as sold to **one** tenant: `platform.platform_leads` carries `sold_to_tenant_id` / `sold_at` / `sold_to_membership_id` / `amount_paisa`, the `pl_select` RLS policy keys on `sold_to_tenant_id = app.current_tenant()`, and `PlatformLead` has a one-way transition to a terminal `Sold` state.

That is wrong for the actual business. The Platform team **sources and verifies a lead, then resells it to several tenants** — a lead is inventory sold N times, capped by a **sale limit**, not a one-off sale. Future tiers (priority/premium) will restrict some leads to certain tenants (prime membership) and carry smaller limits, with dynamic pricing.

## Decision

**Model a lead↔buyers many-to-many with a per-purchase row; keep tiers + dynamic pricing as open hooks.**

1. **`platform.lead_purchases`** join table — one row per buyer: `(id, lead_id FK, tenant_id, membership_id, purchased_at, amount_paisa)`, UNIQUE `(lead_id, tenant_id)` (a tenant can't buy the same lead twice). Replaces the `sold_to_*` columns on `platform_leads`. Availability = `count(lead_purchases WHERE lead_id = X) < sale_limit`.

2. **Tier + sale limit.** `platform_leads.tier` (default `standard`; `priority`/`premium` reserved — canon naming TBD) + nullable `platform_leads.sale_limit` (per-lead override). A `platform.lead_tiers` config table holds the **per-tier default sale_limit + default base price**. Effective limit = `coalesce(lead.sale_limit, tier.default_sale_limit)`. **Schema hook now; membership-eligibility enforcement deferred** until a tenant subscription/package concept exists (per the owner: keep it open + scalable).

3. **Dynamic pricing — computed at purchase, snapshotted on the purchase row.** Price = tier base price, minus discounts by (a) the buyer tenant's package and (b) how many times the lead is/will be shared (volume). The computed `amount_paisa` is stored on the `lead_purchases` row (an immutable record of what that buyer was charged); it is NOT a flat price on the lead. The pricing function is a domain service the `PurchaseLead` handler calls; the package/share-count inputs are explicit params so the rule is testable and evolvable.

4. **Visibility / RLS.** Marketplace browse (non-PII projection) shows leads that are *available* (under limit) and the tenant is *eligible* for the tier — "available + eligible" lives in the **query**, not RLS. Full PII (contact details) is unlocked only to tenants present in `lead_purchases` for that lead (you buy to get the contact). `pl_select` RLS becomes: platform, OR the lead is openly listed (available), OR the tenant has a `lead_purchases` row for it.

## Consequences

- Migration: add `lead_purchases` + `lead_tiers` + `tier`/`sale_limit` columns; drop `sold_to_tenant_id`/`sold_at`/`sold_to_membership_id`/`amount_paisa` from `platform_leads`; rewrite `pl_select`.
- `PlatformLead` aggregate gains a purchases view + `RecordPurchase(tenant, membership, amount, now)` enforcing the sale-limit invariant + no-double-buy; the terminal `Sold` state is replaced by an availability check.
- `PurchaseLead` handler: check availability under limit, compute price (pricing service), insert a `lead_purchases` row, emit the event — all in one tx.
- `LeadPurchasedV1` becomes a **per-purchase** event (buyer tenant + membership + amount). Subscribers (CRM ingest) already key on `purchase_id`, so the per-purchase shape fits; the CRM consumer imports the producer struct (single source of truth) so the field change is one place.
- Pricing inputs (tenant package, share-count discount curve) are explicit + defaulted now; the discount rules harden as the subscription concept lands.

## Fitness function

`TestArch_EveryTenantTableHasRLSAndForce` + `TestArch_EveryTenantTableHasRLSPolicies` (in `internal/architecture/`).

`platform.lead_purchases` + `platform.lead_tiers` are tenant-data-plane tables and must carry RLS+FORCE + policies. Sale-limit + no-double-buy invariants are enforced in the `PlatformLead` aggregate and covered by `platformleadtest.FakeRepository` unit tests + an adapter integration test for the UNIQUE `(lead_id, tenant_id)` 23505 translation (lands with the implementation).
