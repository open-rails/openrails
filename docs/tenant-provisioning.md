# Tenant Provisioning, Lifecycle, Credentials & Webhook Routing (issue #225)

This builds on the #223 tenant model (`pkg/tenant`, `billing.tenants`, migration
039) and the #224 in-process AuthKit control plane (`internal/controlplane`). The
implementation lives in `internal/tenancy`.

Migration `041_tenant_provisioning_secrets` extends `billing.tenants` with
provisioning / routing / tier columns and adds three control-plane tables:
`billing.tenant_secrets` (DB-backed secret store), `billing.tenant_credential_audit`
(rotation/test audit log), and `billing.tenant_exports` (export-before-delete
bookkeeping). It is additive and idempotent.

## Lifecycle service — `tenancy.Service`

Built in the HTTP server only when the control plane is present (it reuses the
control plane's pgx pool and operator-org provisioner). All operations are
idempotent.

| Operation | Behaviour |
|---|---|
| `Provision` | Upserts the `billing.tenants` row (resolve-by-slug, no duplicates), creates/links the operator AuthKit org via `controlplane.EnsureTenantOrg`, records routing/tier/region. Re-running returns the existing tenant. |
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

## Operator-gated provisioning admin API

Mounted at `/v1/admin/tenants`, gated by the SAME operator-admin authority as the
rest of `/v1/admin` (operator org + `openrails:admin`):

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

## Remaining / needs live infrastructure

- **Vault**: the Vault-backed store is a fail-closed stub. A managed deployment
  must implement `VaultKV` against `hashicorp/vault/api` and inject it.
- **Ingress**: host-based webhook routing assumes an ingress maps subdomains to
  the shared pods; the `webhook_host` directory column + `ResolveByHost` are in
  place, but the ingress config itself is out of repo.
- **Stripe Connect** onboarding (`accounts.create`, hosted onboarding,
  account-status webhooks) and a webhook **delivery monitor** (alert on N
  consecutive failures) are scoped by #225 but not implemented in this increment.
- Tenant **onboarding** (invite admins, mint tenant OATs) reuses the #222/#224
  OAT/bootstrap paths via `EnsureTenantOrg`; the OAT-minting convenience wrapper
  is left to the existing control-plane mint API.
