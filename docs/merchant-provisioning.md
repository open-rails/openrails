# Merchant Provisioning, Credentials, and Webhook Routing

A **merchant** is OpenRails' billing/isolation namespace. In standalone
deployments, AuthKit authority for a merchant is a top-level **merchant
permission-group**; the merchant row carries `permission_group_id` pointing at
it (nullable for embedded-without-AuthKit). Vocabulary: a **rail** is the
gateway kind (`nmi`, `stripe`, `ccbill`, `solana`); a **PSP** is a merchant's
concrete account on a rail (`mobius` on nmi, `stripe` on stripe). See
[glossary.md](glossary.md).

This is the provisioning deep-dive. Getting started end-to-end:
[standalone-integration.md](standalone-integration.md). Day-2 operations
(mutation flags in depth, provider pull, cutover): [operations.md](operations.md).

## Provisioning model

Three file-backed push surfaces (example shapes in `config/bootstrap.example.yaml`,
`config/merchants_config.example.yaml`, `config/catalog.example.yaml`):

- `openrails push-auth-bootstrap` — AuthKit root authority: initial operator
  users and trusted remote applications. Default file `/etc/openrails/bootstrap.yaml`.
- `openrails push-merchant-config` — merchants: identity, profile, invoice
  policy, issuer-as-owner, PSPs (rail accounts + secrets). Default file
  `/etc/openrails/merchants.yaml`.
- `openrails apply-catalog --merchant NAME --file PATH` — one catalog application;
  a declarative document is safe to rerun on every boot.

Catalog application uses its document contract, with `prune: false` by default.
Merchant startup initialization uses `push-merchant-config --insert` (or `--seed`)
and creates missing objects only. `--overwrite` and `--prune` are retired. Managed
credentials use Client provider publication, not a manifest import into Vault/DB.
Deliberate metadata changes use `apply-merchant-config` with an application ID and
observed revision. See [the application contract](merchant-configuration-applications.md).

AuthKit authority bootstrap remains first-run only. Merchant startup reloads
snapshot credentials while preserving existing metadata and archived accounts.
Catalog manifests are applied explicitly and are not replayed at ordinary boot.

Embedded hosts declare their merchant programmatically
(`Config.Merchant`, the same shape) and pass their auth in `Deps`; the
issuer-as-owner path below is the standalone mechanism.

## Merchant identity and names

OpenRails owns merchant names (`billing.merchants.slug`). AuthKit groups carry
authority only and are addressed by UUID; a group's AuthKit name is not the
merchant's name. The billing merchant UUID owns payments, credits, customers and
secrets. A non-null billing-to-group binding is immutable, including after
retirement.

A name is unique among live merchants. A rename keeps the former name as an alias
that forwards to the merchant and that no other merchant can claim until it
expires; the merchant itself can take it back. Deleting or retiring a merchant
releases its name and aliases. Resolution follows live names and unexpired
aliases; API writes keep their method, body, authorization and idempotency key.

The site naming policy (`auth.naming`, OpenRails' own, which also governs
AuthKit usernames) governs renames. Defaults allow a rename every **72 hours** and keep each former name
for **90 days**; later policy changes do not alter recorded expiries.

```yaml
auth:
  naming:
    enabled: true
    rename_interval: 72h
    former_names:
      mode: finite
      duration: 2160h
```

Set `enabled: false` to disallow renames, or `rename_interval: 0s` for immediate
eligibility. Former-name modes are `finite`, `forever`, and `immediate`; omit
`duration` for the latter two. Environment equivalents are
`AUTH_NAMING_ENABLED`, `AUTH_NAMING_RENAME_INTERVAL`,
`AUTH_NAMING_FORMER_NAMES_MODE`, and `AUTH_NAMING_FORMER_NAMES_DURATION`.
Hosts running the control plane in process set
`Config.ControlPlane.Auth.Naming` (`openrails.NamingConfig`).

Merchants rename themselves with `PUT /v1/merchant/name {"name": ...}`
(`merchant:settings:update`), subject to the rename interval and, on hosted
deployments, the reserved names and creation pattern. `GET /v1/platform/merchants?q=`
searches current names. A signed-in user lists the merchants they hold a role in
with `GET /v1/merchants` (`{id, slug, display_name, role}`, the user's role in
each).

### Hosted creation recipe (registration is provisioning)

A hosted product (openrails-saas shape) wires everything through
`Config.ControlPlane.MerchantCreation`:

```go
cfg.ControlPlane = &openrails.ControlPlaneConfig{
    Auth:          openrails.AuthConfig{Issuer: "https://api.my-brand.example"},
    HostedPosture: true,
    MerchantCreation: &openrails.MerchantCreationConfig{
        ReservedSlugs: []string{"my-brand"}, // + billing.ReservedMerchantSlugs, always
        FreeAllowance: 2,                    // owned merchants before a card on file is required
    },
}
var client *openrails.Client
cfg.SendGrid = &openrails.SendGridConfig{APIKey: key, From: openrails.EmailAddress{Name: "My Brand", Address: "noreply@my-brand.example"}}
deps := openrails.Deps{
    // openrails-saas shape: the platform merchant's book holds the vault.
    HasVaultedPaymentMethod: func(ctx context.Context, subject string) (bool, error) {
        return client.SubjectHasVaultedPaymentMethod(ctx, platformMerchantID, subject)
    },
}
client, err := openrails.New(ctx, cfg, deps)
```

With it set, signed-in users create merchants they own with
`POST /v1/merchants {"name", "display_name"?}`: 201 on creation, 200 when the
name already resolves to a merchant the caller owns (the idempotent repair).
Refusals: 400 `invalid_name`, 409 `name_taken` / `name_reserved`, 403
`email_unverified` / `creation_refused`, 402 `merchant_creation_payment_method_required`. Creation
is capped at 12 per 24 hours per client IP and per user (429 with
`Retry-After`). The same policy holds in-process `ProvisionMerchant` calls that
name an `OwnerUserID` (typed refusals `billing.ErrMerchantSlugReserved` /
`billing.ErrMerchantCreationRefused`) and merchant renames. Ownerless
`ProvisionMerchant` and Bootstrap are operator acts and stay ungated — that is
how a platform merchant claims a reserved name.

`FreeAllowance` selects the standard hosted admission policy, composed from
OpenRails' own state: verified email always; a free allowance of owned
merchants; beyond it, a vaulted payment method on file (no charge,
`Deps.HasVaultedPaymentMethod`) unlocks more. The predicate is repair-safe: a
name that resolves to a merchant the caller already owns bypasses the allowance
and vault checks because it creates nothing. Typed refusals:
`billing.ErrMerchantCreationEmailUnverified`,
`billing.ErrMerchantCreationPaymentMethodRequired`.

### Merchant retirement (never-used names go back in the pool)

Core provides the mechanism; when to warn about and retire an unused merchant
is the host's policy (openrails-saas owns its own, with its own notices).

- `client.ListMerchantRetirementCandidates(ctx, req)` pages live,
  group-bound merchants created before `req.CreatedBefore`, oldest first,
  excluding reserved slugs (`billing.ReservedMerchantSlugs` plus
  `MerchantCreationConfig.ReservedSlugs`). Each candidate carries `Used`, probed
  with the merchant's own scoped queries.
- `client.RetireUnusedMerchant(ctx, merchantID, groupID)` locks the merchant
  row, refuses a missing/retired merchant, a different group UUID, a reserved
  slug or any activity, and otherwise commits the irreversible tombstone, which
  releases the name, before deleting exactly that AuthKit group. Refusals are
  returned in the result.
- `client.CompletePendingMerchantRetirements(ctx, limit)` retries committed
  retirements whose group release failed, by UUID.

Activity is any customer (and everything owned through customers), payment or
subscription history including tombstones, ledger account, provider connection
(PSP, custodian, stored secret, applied webhook, provider intent), undelivered host
event, outbound webhook or catalog definition. Every blocker references the
merchant row, so the retirement lock serializes concurrent writes. Retired
merchants cannot be restored. Self-hosted deployments need no host state to use
these operations.

## Merchant manifest anatomy

```yaml
version: 1
merchants:
  myapp:
    display_name: MyApp
    api_host: api.myapp.example    # canonical Host for public-route resolution
    remote_application:            # issuer-as-owner
      issuer: https://myapp.example
      jwks_uri: https://myapp.example/.well-known/jwks.json
    settings:                      # the configuration API's settings document
      profile:                     # customer-facing display
        display_name: MyApp Billing
        logo_url: https://myapp.example/logo.png
        from_email: billing@myapp.example
        support_url: https://myapp.example/support
      billing_period_boundary: calendar_month   # arrears invoicing (amounts in micros)
      collection_threshold: 50000000
      monthly_floor: 1000000
      arrears_grace_days: 7        # days past due before delinquent (default 14)
    psps:                          # operator-declared rail accounts
      mobius:
        rail: nmi
        account_id: "100001"
        settings:
          tokenization_key: replace-with-nmi-tokenization-key
        secrets:
          security_key: replace-with-nmi-security-key
          webhook_signing_secret: replace-with-nmi-webhook-secret
```

Per merchant:

- `api_host` — the merchant's canonical API host (bare lowercase hostname,
  globally unique): the `Host` header public routes and Host-routed webhooks
  resolve this merchant from. Declared hosts are asserted on every apply;
  omitted leaves the stored value untouched. An owner claims one at runtime
  with `PUT /v1/merchant/api-host` and binds it once a TXT record proves
  control of the domain (`POST /v1/merchant/api-host/verify`).
- `remote_application` — the host app's issuer (JWKS URI, inline static
  `jwks`, or raw `public_keys`), registered as merchant **owner**: delegated
  tokens signed by that issuer fully administer this one merchant and no other.
- `settings` — the merchant's settings (`billing.MerchantSettings`), the same
  document `GET /v1/merchant/configuration` reads and a configuration
  application changes, with the same names, units and validation
  ([merchant settings](api/merchant-settings.md)): `profile`, invoicing
  (`billing_period_boundary`, `collection_threshold`, `monthly_floor`, amounts in
  micros), the delinquency policy (`arrears_grace_days`,
  `arrears_delinquency_floor`; see `docs/arrears-delinquency.md`),
  `checkout_routing`, `dunning_policy`, `billing_policies`,
  `billing_policy_bindings` and `delegated_invoker_wasted_spend_limits`
  (windows in seconds). Omitted fields keep their stored values; a declared list
  replaces the stored one.
- `psps.<key>` — one entry per PSP. `key` is the manifest PSP name catalog
  `psp_links` and checkout use ("mobius"). Fields: `rail` (`nmi`, `ccbill`,
  `stripe`, `solana`), `account_id`, `archived`, `custodian`, non-secret
  `settings`, `secrets`, and (Solana) `signer`. There is no `environment`: it is
  derived from the deployment's `test_mode` (sandbox ⇒ `test`, live ⇒ `live`),
  so a deployment is all-test or all-live.
- `custodians.<key>` — one entry per card custodian a PSP references. Fields:
  `kind` (`basis_theory`), `account_id`, `archived`, `settings`, `secrets`.

`account_id` is operator-declared, per rail (never derived from credentials at
runtime — details in `docs/rails/*.md`):

| Rail | `account_id` |
|---|---|
| nmi | the dashboard **Gateway ID** (NMI's merchant account id — not the ISO/reseller) — [rails/nmi.md](rails/nmi.md) |
| stripe | `acct_…`, operator-declared — [rails/stripe.md](rails/stripe.md) shows the curl to read it off your own account |
| ccbill | `clientAccnum-clientSubacc`, dash-joined (`900000-0000`) — [rails/ccbill.md](rails/ccbill.md) |
| solana | derived from the signer public key; a declared value is ignored — [rails/solana.md](rails/solana.md) |

A PSP may additionally reference a **custodian** — a third party that holds the
cards it charges (`custodian: <key>`, resolved against the merchant's
`custodians:` block; today `basis_theory` on `nmi` only). That is a modifier on
the rail, not a rail: the `account_id` above is still the gateway's, and the
custodian's own tenant id is declared once on the custodian entry. Several PSPs
may reference the same custodian. See
[payment-method-custody.md](payment-method-custody.md).

### Secret overlays

Secret values do not belong in the committed YAML. Overlays are YAML documents
in the manifest's own shape (`merchants.<slug>.psps.<key>.secrets.*`),
merged over the manifest in order (later wins) and strict-parsed with it:
an unknown field, or secrets for a PSP the manifest never declared, is an
error, never a silent drop.

- Embedded hosts merge them in their own config loader and pass the result as
  `Config.Merchant` (`openrails.ParseMerchantDeclaration` parses one merchant's
  YAML; secrets are `PSPConfig.Secrets`).
- Standalone snapshot custody lists mounted files in `merchant_manifest_overlays`
  (env `MERCHANT_MANIFEST_OVERLAYS`).

The engine itself reads no environment variable and no secret directory.

## Secrets: seeding vs runtime source of truth

Secrets are addressed by `(merchant_id, name)`. PSP credentials use the
canonical account-scoped name:

```text
psps/<rail>/<environment>/<account_id>/<secret_key>
```

Examples: `psps/nmi/live/100001/security_key`,
`psps/stripe/live/acct_123/webhook_signing_secret`,
`psps/ccbill/live/900000-0000/datalink_password`. The `account_id` segment is
URL-escaped (CCBill's composite id is dash-joined precisely so it never embeds
the `/` delimiter). Secret keys are validated against each rail's credential
registry — unknown keys are rejected.

Credential custody is selected explicitly by `secret_backend`:

- `snapshot`: host-owned in-memory values, reloaded and validated at startup.
- `vault`: managed KV-v2 with server-owned mount/scope and actual policy rights.
- `db`: envelope-encrypted PostgreSQL storage; `ENCRYPTION_MASTER_KEY` is required
  for managed storage in both sandbox and live deployments.

Managed provider writes use versioned Client publication with stable operation
identity and a revision precondition. Readers resolve the published credential
version. Each credential slot also has a monotonic logical rotation generation,
independent of the immutable secret candidate's backend version. Changing its
value or custody advances that generation, including an A-to-B-to-A rotation;
receipt replay, unchanged values in the same custody, and metadata-only edits do
not. Qualification uses this generation to reject stale credentials. Retired
overlap slots retain their generation so later reuse cannot reset it.
Direct backend edits do not publish credentials. Changing custody is an
explicit migration, independent of external HTTP publication. Snapshot values
are never implicitly copied to a managed backend.

## Deleting a merchant

A merchant is never hard-deleted through an API. The standalone operator
soft-deletes one with `DELETE /v1/platform/merchants/{id}` and brings it back
with `POST /v1/platform/merchants/{id}/restore`; a never-used merchant is retired
with `Client.RetireUnusedMerchant` (above), which releases its name for good.

Two things no deletion does, by construction:

- **It does not revoke stored instruments.** The cards stay at the PSP; only the
  end user deletes their own instrument.
- **It does not remove real billing history.** The append-only grant log pins
  the products and payments it justifies and is never purged.

Moving a merchant's billing data to another deployment is an archive export and
restore: see [merchant portability](merchant-portability.md). Recovering a whole
deployment is Postgres point-in-time recovery with the `ENCRYPTION_MASTER_KEY`
and Vault alongside it: see [backup and recovery](backup-and-recovery.md).

## API keys

Merchant-scoped backend credentials are minted through the self-serve surface
(requires the control plane; embedded hosts without one do not mount the routes):

- `POST /v1/merchant/api-keys` `{"name": …, "role": …}` → 201 with the key
  **secret exactly once** — it is never stored or retrievable again. The
  non-secret `prefix` (`openrails_st_<key_id>`) identifies the key afterwards.
- `GET /v1/merchant/api-keys` lists (live, expired, revoked — no secret material).
- `DELETE /v1/merchant/api-keys/{id}` revokes; cross-merchant ids 404.

Roles are the fixed merchant catalog: `viewer` (read-only — the right choice
for LLM agents), `support`, `owner`. Minting requires
`merchant:credentials:manage` (owner-only) and is no-escalation: a caller can
never mint a key with authority beyond its own credential's.

## Webhook routing

Inbound rail webhooks resolve the merchant first, then verify. Each deployment
shape mounts the same account-addressed surface:

```text
POST /v1/webhooks/{rail}/{account_id}                        # standalone: configured account resolves merchant
POST /billing/v1/webhooks/{rail}/{account_id}                # embedded under /billing
```

`{rail}` is the gateway KIND — `nmi`, `ccbill`, `stripe`, `solana`,
`basistheory` — never a PSP key. A PSP is named by `{account_id}`, not by the
rail segment: `mobius` and `paykings` both post to `/v1/webhooks/nmi/{account_id}`.
There is no accountless route, and the payload's account identity must agree
with the selected account.

Provider account identity resolves the merchant in the runtime's configured
sandbox/live environment. A runtime bound to one merchant refuses another
merchant's account. The account's signature or provider-specific verification
must pass, and payload account identity must agree. Host headers and merchant
slugs do not grant callback authority. The public billing base supplies only the
external mount prefix; generated Stripe URLs use this same path.

## What the merchant API can change

Merchant routes are scoped to the authenticated merchant; cross-merchant
operations are the standalone operator's (`/v1/platform`). PSP metadata and
archive decisions are always available through the Client; writing a PSP
credential needs a writable secret backend, so it is refused under `snapshot`
custody. Catalog mutation routes are mounted only with
`allow_catalog_updates: true`; the embedded in-process Client is the process
owner and writes its own catalog whatever the flag says. Catalog data always
lives in the database. See [self-hosting-mode1.md](self-hosting-mode1.md).
