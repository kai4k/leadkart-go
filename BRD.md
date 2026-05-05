# LeadKart — Business Requirements Document

> **Status:** Placeholder. The authoritative business spec lives at [`d:\Development\LeadKart\BRD.md`](../LeadKart/BRD.md) (the .NET LeadKart repo). For the Go rebuild, treat that document as the source of truth for product behaviour.
>
> When the Go port stabilises (post-Identity v0.1), port the canonical sections here so the Go repo is independently buildable from spec.

## Quick context

LeadKart is a multi-tenant SaaS for the Indian PCD (propaganda cum distribution) pharma market.

- **Platform team** (LeadKart) sources, verifies, and sells pharma leads to pharma companies.
- **Tenants** (pharma companies) manage those leads through CRM, orders, inventory, dispatch, tasks, notifications.
- A **lead** is a potential buyer (medical store, chemist, distributor) — NOT a franchise partner.

## 8 bounded contexts

| Module | Owner of |
|---|---|
| Identity | Tenants, Persons, Memberships, Roles, RefreshTokenFamilies, AuthRouting |
| Platform | UnverifiedContacts, VerificationCalls, PlatformLeads, LeadCredits |
| CRM | CrmLeads, CallLogs, Reminders, AssignmentHistory |
| Orders | Order state machine + invoices + credit notes |
| Inventory | Products, Batches, StockMovements |
| Dispatch | DispatchRecords, ConsignmentNotes |
| Tasks | WorkItems (hierarchy-scoped) |
| Notifications | Outbound notifications (cross-module event delivery) |

## Cross-cutting concerns

- Multi-tenant isolation enforced at Postgres RLS layer (`SET LOCAL app.tenant_id`).
- Audit log: outbox table doubles as audit (Brandur "events table" pattern).
- DPDP Act 2023 + GDPR compliance — anonymisation, right-to-erasure.
- HIBP password screening + Argon2id + JWT + refresh-token families.
- Indian-specific value objects: GST, PAN, drug licence, FY-aware invoice numbers.
- Operator/SuperAdmin cross-tenant impersonation (Stripe Connect / AWS AssumeRole pattern).

See `docs/adr/` for architectural decisions and `.NET BRD.md` for full feature inventory.
