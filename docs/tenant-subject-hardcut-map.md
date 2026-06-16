# Tenant Subject Hard-Cut Map

Issue #317 makes `tenant_subject_id` the durable payable identity across
OpenRails. `tenant_subject_id` identifies who owns the balance, entitlement,
subscription, payment method, invoice, or owed amount. `invoker_id` identifies who
caused an action when that differs from the payable subject.

## Canonical Model

| Concept | Canonical field | Notes |
|---|---|---|
| Payable identity | `tenant_subject_id uuid not null` | FK/logical reference to `billing.tenant_subjects.id`. |
| External principal | `issuer`, `subject` | Stored once in `billing.tenant_subjects` and touched idempotently. |
| Action attribution | `invoker_id text` | For service tokens use `serviceToken:<key_id>`; for delegated users use `<issuer>:<subject>` or the resolved delegated subject when the route contract requires it. |
| Tenant boundary | `tenant_id uuid not null` | RLS and directory boundary; do not infer it from a caller-provided subject string. |
| Service-token scope | `openrails.merchant`, `openrails.customer` | AuthKit resource scopes are opaque there and interpreted only by OpenRails. |

## Rename / Model Map

| Current surface | Current payable field | Target |
|---|---|---|
| `billing.entitlements`, `models.Entitlement`, entitlement repo/service | `user_id text` | `tenant_subject_id uuid`; timeline uniqueness/exclusion becomes `(tenant_id, tenant_subject_id, entitlement, period)`. |
| `billing.subscriptions`, `models.Subscription`, subscription services | `user_id text` | `tenant_subject_id uuid`; tier-group uniqueness becomes `(tenant_id, tenant_subject_id, tier_group)` for active/pending/past_due rows. |
| `billing.checkout_sessions`, `models.CheckoutSession` | `user_id text` | `tenant_subject_id uuid`; request contracts accept tenant subject, not host user id. |
| `billing.payments`, `models.Payment` | `user_id text` | `tenant_subject_id uuid`; processor transaction lookup remains processor-owned metadata. |
| `billing.payment_methods`, `models.PaymentMethod` | `user_id text` | `tenant_subject_id uuid`; vault uniqueness becomes `(tenant_id, tenant_subject_id, vault_id)` plus processor vault uniqueness. |
| `billing.processor_customers`, `models.ProcessorCustomer` | `user_id text` | `tenant_subject_id uuid`; customer mapping keyed by `(tenant_id, tenant_subject_id, processor)`. |
| `billing.admin_grants`, `models.AdminGrant` | `user_id text` | `tenant_subject_id uuid` when the grant is payable/entitlement-bearing; retain separate actor fields only for audit. |
| `billing.product_access_grants`, `models.ProductAccessGrant` | `user_id text` | `tenant_subject_id uuid`; service/admin routes should use tenant-subject paths or bodies. |
| `billing.notification_queue`, `models.NotificationQueue` | `user_id text` | `tenant_subject_id uuid` plus delivery metadata if notifications stay in OpenRails. |
| Service entitlement route | `/v1/service/users/{user_id}/entitlements` | `/v1/service/tenant-subjects/{tenant_subject_id}/entitlements`. |
| Admin user support routes | `/v1/admin/users/{user_id}...` | `/v1/admin/tenant-subjects/{tenant_subject_id}...` for payable billing state; keep external user search only where it is clearly host-support metadata. |
| Credit/account APIs | Mixed historical `owner_id`, `payer`, `account-settings` wording | Existing DB/code mostly maps to `tenant_subject_id`; remaining JSON/query/docs should say `tenant_subject_id`. |

## Already Converted

- `billing.tenant_subjects` exists with `(tenant_id, issuer, subject)` uniqueness.
- Credit balances, transactions, blocks, account settings, spend limits, tier
  policies, budget reservations, usage events, and invoices have the
  `tenant_subject_id` column through migrations 071/072 and Go models.
- Service-token resource scope uses `openrails.customer` with UUID values.
- Delegated JWT resolution touches `(tenant_id, issuer, subject)` idempotently.

## Hard-Cut Rules

- Do not add an `accounts` table as a compatibility layer.
- Do not accept `payer_account_id`, `account_id`, payable `delegated_user_id`,
  `subject_type`, or old `owner_id`/`payer` request fields after the route/API
  cut. Reject them with strict bind/validation tests.
- Do not duplicate `tenant_id`, `issuer`, and `subject` across payable tables;
  join through `billing.tenant_subjects`.
- Keep `user_id` only where it means non-payable delivery/audit metadata, and
  name that field explicitly if it remains after the hard cut.
