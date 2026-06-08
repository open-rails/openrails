# Tenant, OIDC, and service-token glossary

OpenRails treats identity, resource ownership, and payable billing identity as
separate concepts. This is the canonical terminology for docs, manifests, API
examples, and tests.

## Terms

| Term | Meaning |
|---|---|
| OpenRails tenant | The OpenRails customer/integration boundary. It scopes billing data, machine credentials, tenant issuers, credit balances, usage, and invoices. |
| Tenant issuer | An OIDC issuer trusted for one OpenRails tenant. It is registered with `issuer`, `jwks_uri`, accepted `audiences`, and `enabled`. |
| Delegated user | A verified external OIDC subject from a tenant issuer. OpenRails stores only the minimal `(tenant_id, issuer, subject)` reference when it needs billing/audit identity. |
| Tenant subject | The payable OpenRails identity row: `billing.tenant_subjects(id, tenant_id, issuer, subject)`. A subject may represent a person, company, upstream tenant, service, or delegated principal. |
| Invoker | The principal that caused usage when it differs from the payable tenant subject. Use `invoker_id` for attribution and budget caps. |
| Generated service token | An opaque AuthKit/OpenRails server-to-server credential (`openrails_st_...`) minted by OpenRails for non-OIDC clients, scripts, and explicit generated-token use cases. |
| Service JWT | A first-party OIDC server-to-server JWT minted by a tenant's own AuthKit issuer with `token_use=service`, short expiry, `jti`, `permissions`, and `aud=openrails`; OpenRails intersects requested permissions/resources with a server-side grant. |
| Bootstrap authority | A deploy/operator action that runs manifests and one-shot bootstrap commands. It is not a billable tenant subject and should not be modeled as a customer tenant. |

## Patterns

### Doujins/Hentai0 browser or admin call

1. OpenRails has an OpenRails tenant for the integration.
2. The tenant manifest registers Doujins/Hentai0 as tenant issuers with OIDC
   `issuer`, `jwks_uri`, and accepted `audiences`.
3. The browser/admin caller presents a delegated JWT signed by the registered
   issuer.
4. OpenRails verifies `iss`, `aud`, `kid`/JWKS, expiry, tenant binding, and
   delegated `openrails:self:*` or `openrails:tenant:*` permissions.
5. OpenRails touches the tenant subject `(tenant_id, issuer, subject)` and uses
   that row for payable identity when the operation needs one.

### Doujins/Hentai0 backend entitlement read

1. Bootstrap registers the Doujins/Hentai0 issuer for the tenant. That
   registration is the whole authorization — a tenant has full authority over its
   own resources, so there is no separate grant.
2. The backend mints a 15-minute service JWT from its own AuthKit issuer with
   `aud=openrails`, `token_use=service`, `jti`, and its self-assigned permissions
   (e.g. `openrails:entitlements:read`).
3. The backend calls
   `GET /v1/service/tenant-subjects/{tenant_subject_id}/entitlements`.
4. OpenRails verifies the issuer/JWKS/claims, treats the token's permissions as
   authoritative, and scopes the read to the issuer's own tenant before returning
   entitlement rows.

### Tensorhub usage billing

1. Tensorhub is authorized as a generated service-token caller or service-JWT
   principal for an OpenRails tenant.
2. A payer is a `tenant_subject_id`; the caller/invoker is an `invoker_id`.
3. Reserve, hold, capture, usage rollup, invoice, and budget APIs use the
   tenant subject directly. OpenRails never derives the payer from `user_id`,
   `payer_account_id`, `account_id`, or `subject_type`.

## Manifest shape

```yaml
tenants:
  - slug: tensorhub
    issuers:
      - issuer: https://doujins.example
        jwks_uri: https://doujins.example/.well-known/jwks.json
```

Use this vocabulary in all OpenRails surfaces. The `user_id` column has been
hard-cut from every payable billing table (subscriptions, payments,
payment_methods, processor_customers, checkout_sessions, product_access_grants,
admin_grants, notification_queue, entitlements) — they reference
`tenant_subject_id` and recover `(tenant_id, issuer, subject)` by joining
`billing.tenant_subjects`. Billing API responses expose `tenant_subject_id`, not
`user_id`. For a self-service identity the tenant subject id equals the user's
UUID (issuer `openrails:self`); external/delegated subjects resolve to their own
`tenant_subjects` row.
