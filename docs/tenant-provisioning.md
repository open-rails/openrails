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

catalogs:
  - name: default
    default_providers: [nmi]
    tier_groups:
      - slug: plans
        display_name: Plans
        products:
          - slug: starter
            display_name: Starter
            tier_rank: 1
            prices:
              - currency: usd
                unit_amount: 1000
                interval: month
```

- `tenants[].issuers[]` registers tenant-owned OIDC issuers with exact
  `issuer`, `jwks_uri`, optional `audiences` (default `openrails`), and optional
  `enabled`. Registration is the whole authorization: a tenant has full authority
  over its own resources, so there is no per-issuer permission grant to declare.
- Delegated browser/admin calls use JWTs signed by those issuer keys. OpenRails
  validates `iss`, `aud`, `kid`/JWKS signature, expiry, and delegated
  `openrails:self:*` or `openrails:tenant:*` permissions, then touches the
  minimal payable tenant subject `(tenant_id, issuer, subject)`.
- Caller-minted first-party **service JWTs** are authorized by that same issuer
  registration: a validly-signed token from a registered issuer is trusted, its
  permission claims are authoritative (self-assigned least-privilege chosen by the
  caller for the step), and it is scoped to the issuer's own tenant resources —
  OpenRails never lets one tenant reach another tenant's resources.
- Bootstrap YAML does not mint generated opaque service-token secrets and does
  not write service-token material to Vault KV or mounted files. Non-OIDC
  clients and break-glass/admin scripts must use an explicit operator/admin token
  minting flow outside bootstrap.
- `catalogs[]` uses the existing catalog-as-code schema. Bootstrap applies it
  additively: missing products/prices are not removed by omission.

### Provider identity & idempotency

Applying a catalog (via bootstrap first-run, the `billing catalog apply` CLI, or
the `/admin/catalog/*` API — all one shared path) fans each price out to its
declared `providers` and **find-or-creates** the corresponding object in each:
Stripe Product+Price, NMI Recurring Plan, Solana on-chain Plan. CCBill is
manual-link (supply `flex_id`).

Every provider object is **content-addressed** by the price's *substance* — the
product slug plus the immutable money terms (`<slug>.<currency>.<amount>.<cycle>`),
never the per-DB price UUID. Consequences:

- **Idempotent across a fresh/rebuilt DB.** Re-applying the same manifest to an
  empty control plane (DR, new cluster) re-derives the same Stripe lookup key, NMI
  `plan_id`, and Solana plan PDA, so it re-attaches to the existing provider
  objects instead of creating duplicates.
- **Cosmetic edits are free.** Changing `display_name`, `description`, or the
  `providers` list does not change identity, so it never regenerates provider
  objects (only mutable fields update where the provider supports it).
- **Money-term changes mint a new price.** A different amount/currency/interval is
  a new identity → create-new + archive-old (OpenRails prices are immutable).

Auto-generated NMI `plan_id`s are the content key with NMI-safe separators and
**no prefix** — `<slug>-<currency>-<amount>-<cycle>`, e.g. `premium-usd-2300-30`
(dots → hyphens; no `openrails-`, tenant, or application prefix). Auto-generated
Solana plan ids are `sha256(contentKey + ":" + mint)` truncated to a `uint64`.

### Explicit provider links

When an external object already exists (it was created in the provider's own
dashboard, or by an earlier system), declare it with `provider_links` instead of
letting OpenRails auto-create. The same schema works in the bootstrap manifest,
the `billing catalog apply` YAML, and the `PATCH /admin/catalog/prices/{id}`
API. Canonical keys per provider:

| Provider | `provider_links` key | Recommended key |
| --- | --- | --- |
| `stripe` | `stripe` | `lookup_key` (a user-set, account/mode-portable handle) |
| `mobius` (NMI) | `mobius` | `plan_id` (optional `provider`) |
| `solana` | `solana` | `plan_pda` |
| `ccbill` | `ccbill` | `form_name`, `flex_id` |

The recommended manual-link fields are the **operator-chosen** identifiers —
Stripe `lookup_key` and NMI `plan_id` — because they are human-readable, stable,
and the same string works across a fresh DB / test+live Stripe modes. Both are
*find-or-create*: OpenRails links an existing object (verifying its money terms)
or creates one at that identifier.

```yaml
prices:
  - currency: usd
    unit_amount: 2300
    interval: month
    provider_links:
      stripe: { lookup_key: premium }   # find-or-create at this key (recommended)
      mobius: { plan_id: premium }      # find-or-create the NMI plan at this id
      solana: { plan_pda: 7Xy...PdA }   # must already exist on-chain
      ccbill: { form_name: premium, flex_id: abc-123 }
```

To instead pin an *exact* pre-existing Stripe Price that OpenRails must never
recreate, link its generated id directly: `stripe: { price_id: price_123,
product_id: prod_123 }` (product_id optional). `price_id` is require-exists;
`lookup_key` is find-or-create.

A supplied link is **validated against the external provider before it is
accepted**, and when the object already exists no duplicate is created. Whether
a *missing* linked object is an error or is **created** depends on whether the
provider's linked identifier is client-creatable:

- **NMI/Mobius** — `plan_id` is operator-chosen **and** client-creatable, so a
  link is **find-or-create**: an existing plan is verified (amount and, when NMI
  reports a day-based frequency, the billing cycle must match); a missing one is
  **created at the operator's chosen id** from the price's terms. Link
  `plan_id: premium` and OpenRails uses or creates the `premium` plan.
- **Stripe** — `price_id` (`price_xxx`) is Stripe-**generated** and cannot be
  created at a chosen id, so a `price_id` link must already exist and is verified
  against `unit_amount`, `currency`, recurring interval/duration, and (when
  `product_id` is given) its Product. The client-creatable Stripe identifier is
  the **`lookup_key`**: a link supplying only `lookup_key` is **find-or-create**
  at that key (the auto-create flow, at the operator's key instead of the derived
  one).
- **Solana** — `plan_pda` is derived from `(merchant, plan_id)`, so OpenRails
  cannot publish at an arbitrary supplied PDA: a Solana link must already exist
  and is verified against amount (base units), period (`cycle × 24h`), the mint
  resolved from the price currency, and the tenant's merchant/owner. (To create
  a new on-chain plan, list `solana` under `providers` and let auto-create
  publish it.)
- **CCBill** — has no public read API, so its ids are stored as operator-owned
  without remote validation.

A **mismatched** link (the object exists but its money terms differ) **fails the
apply loudly** with an actionable error naming the offending field — OpenRails
never silently binds the wrong object. A **missing** link errors only where the
identifier is not client-creatable (Stripe `price_id`, Solana `plan_pda`).
Explicit links are operator-owned: OpenRails never renames them, even when the
auto-generated id template changes.

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
