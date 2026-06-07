# Tenant, OIDC, and service-token glossary

OpenRails treats identity, resource ownership, and payable billing identity as
separate concepts. This is the canonical terminology for docs, manifests, API
examples, and tests.

## Terms

| Term | Meaning |
|---|---|
| OpenRails tenant | The OpenRails customer/integration boundary. It scopes billing data, service tokens, tenant issuers, credit balances, usage, and invoices. |
| Tenant issuer | An OIDC issuer trusted for one OpenRails tenant. It is registered with `issuer`, `jwks_uri`, accepted `audiences`, and `enabled`. |
| Delegated user | A verified external OIDC subject from a tenant issuer. OpenRails stores only the minimal `(tenant_id, issuer, subject)` reference when it needs billing/audit identity. |
| Tenant subject | The payable OpenRails identity row: `billing.tenant_subjects(id, tenant_id, issuer, subject)`. A subject may represent a person, company, upstream tenant, service, or delegated principal. |
| Invoker | The principal that caused usage when it differs from the payable tenant subject. Use `invoker_id` for attribution and budget caps. |
| Service token | An opaque AuthKit/OpenRails server-to-server credential (`openrails_st_...`) owned by a registered client/issuer and scoped by OpenRails resources. |
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

1. Bootstrap mints an opaque service token with
   `openrails:entitlements:read`.
2. The token carries `openrails.tenant=<tenant_uuid>` and optionally
   `openrails.tenant_subject=<tenant_subject_uuid>`.
3. The backend calls
   `GET /v1/service/tenant-subjects/{tenant_subject_id}/entitlements`.
4. OpenRails validates the service token, permission, tenant resource, and
   tenant-subject resource scope before returning entitlement rows.

### Tensorhub usage billing

1. Tensorhub is authorized as a service-token caller for an OpenRails tenant.
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
        audiences: ["openrails"]
        enabled: true
    service_tokens:
      - name: tensorhub-runtime
        permissions:
          - openrails:credits:read
          - openrails:credits:write
          - openrails:credits:spend
        resources:
          - kind: openrails.tenant
            id: 00000000-0000-0000-0000-000000000001
        outputs:
          - vault_mount: secret
            vault_path: openrails/tensorhub/runtime
            vault_field: token
```

Use this vocabulary in new OpenRails surfaces. Legacy `user_id` fields remain
only where older subscription/payment/admin flows have not yet been hard-cut.
