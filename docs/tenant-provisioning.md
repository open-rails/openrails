# Tenant Provisioning, Lifecycle, Credentials & Webhook Routing (issue #225)

This builds on the #223 tenant model (`pkg/tenant`, `billing.tenants`, migration
039) and the #224 in-process AuthKit control plane (`internal/controlplane`). The
implementation lives in `internal/tenancy`.

Terminology note: this document uses "tenant" for the OpenRails
customer/integration boundary. Bootstrap authority is a deploy action. Delegated
browser/admin identities come from registered OIDC tenant issuers, and payable
identity is `billing.tenant_subjects`; see
`docs/authkit-tenant-oidc-glossary.md`.

Migration `041_tenant_provisioning_secrets` extends `billing.tenants` with
provisioning / routing / tier columns and adds three control-plane tables:
`billing.tenant_secrets` (DB-backed secret store), `billing.tenant_credential_audit`
(rotation/test audit log), and `billing.tenant_exports` (export-before-delete
bookkeeping). It is additive and idempotent.

## Lifecycle service — `tenancy.Service`

Built in the HTTP server only when the control plane is present (it reuses the
control plane's pgx pool and tenant provisioner). All operations are
idempotent.

| Operation | Behaviour |
|---|---|
| `Provision` | Upserts the `billing.tenants` row (resolve-by-slug, no duplicates), ensures the embedded AuthKit tenant exists, records routing/tier/region. Re-running returns the existing tenant. |
| `Suspend` / `Resume` | Flips `status` + `suspended_at`. A suspended tenant denies writes (`IsWritable` is false) while reads still resolve. Idempotent. |
| `TierChange` | Updates the platform's OWN `billing_tier` for the tenant (dogfood). |
| `Export` | Writes a completed `tenant_exports` row (row-count manifest + secret-NAME enumeration; values never exported). Required before delete. |
| `Delete` | Gated purge: requires `Confirm` AND a completed export. Purges every tenant-owned `billing.*` row, secrets, and credential audit in one tx, then tombstones the directory row. The default tenant can never be deleted. |

### Suspension semantics

`IsWritable` is the write-gate signal (active = writable; suspended/deleted =
read-only). Processor **webhooks still resolve and process** for a suspended
tenant so historical billing state stays correct — suspension gates tenant-admin
mutations and service writes, not inbound webhooks.

## Per-tenant secrets — `tenancy.TenantSecretStore`

Namespaced by tenant id; one tenant can never read/overwrite another's secrets.
Canonical names: `stripe/secret_key`, `stripe/webhook_signing_secret`,
`stripe/webhook_signing_secret_thin`.

Three implementations, same `(tenant, name)` addressing:

- **DB-backed** (`NewDBSecretStore`) — persists to `billing.tenant_secrets`. The
  self-hosted / dev default. **Builds and runs without a live Vault.**
- **In-memory** (`NewMemorySecretStore`) — for tests / pure-dev.
- **Vault-backed** (`NewVaultSecretStore`) — documented adapter mapping
  `(tenant, name)` to a tenant-scoped KV path
  `secret/openrails/tenants/<tenant-id>/<name>`. It is a STUB until a managed
  deployment injects a live `VaultKV`; until then it **fails closed**
  (`ErrVaultNotConfigured`) rather than returning empty secrets. Swapping it in
  needs no schema or caller change.

Credentials are loaded **by tenant id at request time** (`LoadStripeCredentials`),
never injected process-wide. `RotateCredential` / `PutCredential` write an audit
row; `TestStripeCredential` verifies a key via a read-only Stripe `GET /v1/balance`
(no charge) and audits the result.

## Webhook routing — trust boundary after resolution

Route: `POST /v1/t/:tenant/webhooks/:provider` (mounted when the tenancy service
is present).

1. OpenRails resolves the tenant from the URL slug (`ResolveBySlug`) — an
   unknown/deleted tenant is a 404; there is **no default-tenant fallback** for a
   tenant-scoped URL.
2. It loads THAT tenant's signing secret(s) from the secret store.
3. It verifies the Stripe signature against the tenant's own secret(s)
   (`verifyStripeTenant`). A missing secret is rejected (never "skip
   verification"); a foreign tenant's secret never verifies the signature.

The router/ingress only *resolves* the tenant. **OpenRails always re-derives the
secret and re-verifies**, so the router is not the trust boundary for billing
semantics. `ResolveByHost` additionally maps an ingress `Host` header to a tenant
via the registered `webhook_host`.

### How a tenant configures their Stripe Dashboard webhook URL

Each tenant points their Stripe webhook endpoint at either:

- **Path form** (default): `https://<platform-host>/v1/t/<tenant-slug>/webhooks/stripe`
- **Subdomain form**: a per-tenant `webhook_host` (e.g. `hooks.acme.com`)
  registered on the tenant row; the ingress routes by host and OpenRails
  re-resolves via `ResolveByHost`.

The tenant pastes their Stripe **webhook signing secret** into OpenRails via the
credential API (`PUT /v1/admin/tenants/:id/credentials/stripe/webhook_signing_secret`),
and their Stripe **secret key** for thin-event hydration / credential tests.

## Bootstrap-gated provisioning API

Mounted at `/v1/admin/tenants`, gated by the same bootstrap-managed admin
authority as the rest of `/v1/admin` (`openrails:admin`):

| Method & path | Action |
|---|---|
| `POST /v1/admin/tenants` | Provision |
| `GET /v1/admin/tenants/:id` | Get |
| `POST /v1/admin/tenants/:id/suspend` \| `/resume` | Suspend / Resume |
| `POST /v1/admin/tenants/:id/tier` | Tier change (`{"tier":"..."}`) |
| `POST /v1/admin/tenants/:id/export` | Export (returns export id + row counts) |
| `POST /v1/admin/tenants/:id/delete` | Delete (`{"confirm":true}`; 409 if no export) |
| `PUT /v1/admin/tenants/:id/credentials/:name` | Rotate a per-tenant credential (`{"value":"..."}`) |
| `POST /v1/admin/tenants/:id/credentials/test-stripe` | Test the tenant's Stripe key (no charge) |

## Unified bootstrap manifest

Closed-registration deployments should bootstrap tenants through the deploy
pipeline, not by modeling bootstrap authority as an AuthKit tenant. New
deployments should use one explicit provisioning step after migrations:

```sh
billing migrate up -c /etc/openrails/config.yaml
billing bootstrap apply -c /etc/openrails/config.yaml -f /etc/openrails/bootstrap.yaml
billing run-server -c /etc/openrails/config.yaml
```

`/etc/openrails/config.yaml` is runtime infrastructure config. The separate
`/etc/openrails/bootstrap.yaml` file is desired tenant/catalog provisioning
state and is applied by an init job or operator command, not by normal API
replicas on startup.

The unified manifest shape is:

```yaml
version: 1

tenants:
  - slug: doujins
    name: Doujins
    issuers:
      - issuer: https://auth.doujins.com
        jwks_uri: https://auth.doujins.com/.well-known/jwks.json
        audiences: [openrails]

    service_jwt_principals:
      - issuer: https://auth.doujins.com
        subject: service:doujins-runtime
        permissions:
          - openrails:entitlements:read
          - openrails:credits:read
          - openrails:credits:write
          - openrails:credits:spend

catalogs:
  - name: default
    default_currency: usd
    default_providers: [nmi]
    tier_groups:
      - slug: plans
        display_name: Plans
        products:
          - slug: starter
            display_name: Starter
            tier_rank: 1
            prices:
              - unit_amount: 1000
                interval: month
```

- `tenants[].issuers[]` registers tenant-owned OIDC issuers with exact
  `issuer`, `jwks_uri`, accepted `audiences`, and optional `enabled`.
- Delegated browser/admin calls use JWTs signed by those issuer keys. OpenRails
  validates `iss`, `aud`, `kid`/JWKS signature, expiry, and delegated
  `openrails:self:*` or `openrails:tenant:*` permissions, then touches the
  minimal payable tenant subject `(tenant_id, issuer, subject)`.
- `tenants[].service_jwt_principals[]` grants server-side authorization to
  caller-minted first-party service JWTs. The JWT must still request
  permissions, but OpenRails intersects those requests with this grant.
- Bootstrap YAML does not mint generated opaque service-token secrets and does
  not write service-token material to Vault KV or mounted files. Non-OIDC
  clients and break-glass/admin scripts must use an explicit operator/admin token
  minting flow outside bootstrap.
- `catalogs[]` uses the existing catalog-as-code schema. Bootstrap applies it
  additively: missing products/prices are not removed by omission.

Delegated JWTs are for browser/direct self-service or tenant-admin operations.
First-party service JWTs are for backend calls such as entitlement reads and
credit reserve/capture/release flows, and must not be accepted by delegated
browser routes.

## Remaining / needs live infrastructure

- **Vault**: the Vault-backed store is a fail-closed stub. A managed deployment
  must implement `VaultKV` against `hashicorp/vault/api` and inject it.
- **Ingress**: host-based webhook routing assumes an ingress maps subdomains to
  the shared pods; the `webhook_host` directory column + `ResolveByHost` are in
  place, but the ingress config itself is out of repo.
- **Stripe Connect** onboarding (`accounts.create`, hosted onboarding,
  account-status webhooks) and a webhook **delivery monitor** (alert on N
  consecutive failures) are scoped by #225 but not implemented in this increment.
- Tenant **onboarding** (configure tenant issuers, grant service-JWT principals,
  or mint generated service tokens for non-OIDC callers) reuses bootstrap and
  control-plane functionality, but generated service-token minting is an explicit
  operator/admin action outside YAML bootstrap.
