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
- `openrails push-merchant-config` — merchants: identity and issuer-as-owner;
  without Vault the manifest also carries their configuration (settings, PSPs
  with their secrets). Default file `/etc/openrails/merchants.yaml`.
- `openrails apply-catalog --merchant NAME --file PATH` — one catalog application;
  a declarative document is safe to rerun on every boot.

Catalog application uses its document contract, with `prune: false` by default.
`push-merchant-config --insert` (or `--seed`) creates missing merchants only;
`--overwrite` and `--prune` are retired. Where configuration lives decides the
rest ([merchant configuration](merchant-configuration.md)): without Vault the
manifest is the configuration, read at every boot; with Vault the manifest
names merchants only and a manifest declaring configuration is refused, while
staff edit it over HTTP ([vault.md](vault.md)).

AuthKit authority bootstrap remains first-run only. Catalog manifests are
applied explicitly and are not replayed at ordinary boot.

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
Hosts building on the server package set `server.Config.Auth`'s `Naming`
(`server.NamingConfig`).

The operator renames a merchant with the server's `RenameMerchant` (or
`openrails merchants rename`); a hosted product renames one for its owner with
`RenameMerchantParams.ActorUserID` set, which adds the rename interval and, on
hosted deployments, the reserved names and creation pattern. `ListMerchants`
(`openrails merchants list --query`) searches current names, and
`ListUserMerchants` lists the merchants a user holds a role in, with that role.
No route renames or lists merchants.

### Hosted creation recipe (registration is provisioning)

A hosted product (openrails-saas shape) builds on the server package and wires
everything through `server.Config.MerchantCreation`:

```go
engine.SMTP = &openrails.SMTPConfig{Host: "smtp.sendgrid.net", Username: "apikey", Password: key, From: openrails.EmailAddress{Name: "My Brand", Address: "noreply@my-brand.example"}}
cfg := server.Config{
    Engine:       engine,
    Auth:         server.AuthConfig{Issuer: "https://api.my-brand.example"},
    LocalSignIn:  true,
    Registration: iam.RegistrationModeOpen, // github.com/open-rails/authkit/iam
    MerchantCreation: &server.MerchantCreationConfig{
        ReservedSlugs: []string{"my-brand"}, // + billing.ReservedMerchantSlugs, always
        FreeAllowance: 2,                    // owned merchants before a card on file is required
    },
}
var srv *server.Server
deps := server.Deps{
    Engine: openrails.Deps{Postgres: pool},
    // openrails-saas shape: the platform merchant's book holds the vault.
    HasVaultedPaymentMethod: func(ctx context.Context, subject string) (bool, error) {
        return srv.SubjectHasVaultedPaymentMethod(ctx, platformMerchantID, subject)
    },
}
srv, err := server.New(ctx, cfg, deps)
```

The product's own registration route calls `ProvisionMerchant` with the user
as `OwnerUserID`; the policy then holds: `billing.ErrInvalidMerchantSlug`,
`billing.ErrMerchantSlugReserved`, `billing.ErrMerchantCreationEmailUnverified`,
`billing.ErrMerchantCreationPaymentMethodRequired`, or
`billing.ErrMerchantCreationRefused`. A name already held answers that merchant
with `Created` false, whoever owns it: the product checks it is the user's
(`ListUserMerchants`) before treating it as the idempotent repair, and a
display name applies only to a merchant the call creates. Rate limits on
registration are the product's. Ownerless `ProvisionMerchant` and Bootstrap
are operator acts and stay ungated — that is how a platform merchant claims a
reserved name. OpenRails serves no route that creates a merchant.

`FreeAllowance` selects the standard hosted admission policy, composed from
OpenRails' own state: verified email always; a free allowance of owned
merchants; beyond it, a vaulted payment method on file (no charge,
`server.Deps.HasVaultedPaymentMethod`) unlocks more. The predicate is repair-safe: a
name that resolves to a merchant the caller already owns bypasses the allowance
and vault checks because it creates nothing. Typed refusals:
`billing.ErrMerchantCreationEmailUnverified`,
`billing.ErrMerchantCreationPaymentMethodRequired`.

### Merchant retirement (never-used names go back in the pool)

Core provides the mechanism; when to warn about and retire an unused merchant
is the host's policy (openrails-saas owns its own, with its own notices).

- `srv.ListMerchantRetirementCandidates(ctx, req)` pages live,
  group-bound merchants created before `req.CreatedBefore`, oldest first,
  excluding reserved slugs (`billing.ReservedMerchantSlugs` plus
  `server.MerchantCreationConfig.ReservedSlugs`). Each candidate carries `Used`, probed
  with the merchant's own scoped queries.
- `srv.RetireUnusedMerchant(ctx, merchantID, groupID)` locks the merchant
  row, refuses a missing/retired merchant, a different group UUID, a reserved
  slug or any activity, and otherwise commits the irreversible tombstone, which
  releases the name, before deleting exactly that AuthKit group. Refusals are
  returned in the result.
- `srv.CompletePendingMerchantRetirements(ctx, limit)` retries committed
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
    secrets:                       # the merchant's own credentials
      scim_token: replace-with-a-long-random-token
```

Per merchant:

- `api_host` — the merchant's canonical API host (bare lowercase hostname,
  globally unique): the `Host` header public routes and Host-routed webhooks
  resolve this merchant from. Declared hosts are asserted on every apply;
  omitted leaves the stored value untouched. A hosted product lets an owner
  claim one at runtime, bound once a TXT record proves control of the domain
  (the server's `ClaimMerchantAPIHost`, `VerifyMerchantAPIHost`).
- `remote_application` — the host app's issuer (JWKS URI, inline static
  `jwks`, or raw `public_keys`), registered as merchant **owner**: its RFC 9068
  access tokens act for this one merchant and no other, within that role
  ([auth](auth.md#trusted-issuers); needs `resource_server`).
- `settings` — the merchant's settings (`billing.MerchantSettings`), the same
  document `GET /v1/admin/configuration` reads and a configuration
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
- `secrets.scim_token` — a provisioning token (at least 32 characters) the
  merchant's directory presents at `/v1/app/scim/v2`
  ([customer contacts](customer-contacts.md)). OpenRails keeps its SHA-256 as
  the merchant's declared token, replaced when this changes and removed when it
  is removed; tokens minted over the API stand beside it.

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
in the manifest's own shape (`merchants.<slug>.psps.<key>.secrets.*`,
`merchants.<slug>.secrets.scim_token`),
merged over the manifest in order (later wins) and strict-parsed with it:
an unknown field, or secrets for a PSP the manifest never declared, is an
error, never a silent drop.

- Embedded hosts merge them in their own config loader and pass the result as
  `Config.Merchant` (`openrails.ParseMerchantDeclaration` parses one merchant's
  YAML; secrets are `PSPConfig.Secrets`).
- The standalone server lists mounted files in `merchant_manifest_overlays`
  (env `MERCHANT_MANIFEST_OVERLAYS`).

The engine itself reads no environment variable and no secret directory.

## Credential names

Inside OpenRails a PSP credential is named by its account:

```text
psps/<rail>/<environment>/<account_id>/<secret_key>
```

Examples: `psps/nmi/live/100001/security_key`,
`psps/stripe/live/acct_123/webhook_signing_secret`,
`psps/ccbill/live/900000-0000/datalink_password`. The value is the PSP
document's `secrets.<secret_key>`, in the manifest or in Vault. Secret keys are
validated against each rail's credential registry — unknown keys are rejected.
A changed credential is checked against its declared account before it is used.

## Deleting a merchant

A merchant is never hard-deleted through an API. The standalone operator
soft-deletes one with the server's `DeleteMerchant` (`openrails merchants
delete <id>`) and brings it back with `RestoreMerchant` (`openrails merchants
restore <id>`); a never-used merchant is retired with `RetireUnusedMerchant`
(above), which releases its name for good.

Two things no deletion does, by construction:

- **It does not revoke stored instruments.** The cards stay at the PSP; only the
  end user deletes their own instrument.
- **It does not remove real billing history.** The append-only grant log pins
  the products and payments it justifies and is never purged.

Moving a merchant's billing data to another deployment is an archive export and
restore: see [merchant portability](merchant-portability.md). Recovering a whole
deployment is Postgres point-in-time recovery with Vault (or the manifest)
alongside it: see [backup and recovery](backup-and-recovery.md).

## API keys

OpenRails serves no route that mints credentials. A self-hosted backend
authenticates with a client-credentials access token from the merchant's
trusted issuer ([standalone](standalone-integration.md#deployment)). A hosted
product mints merchant API keys with the server's `CreateMerchantAPIKey`
(listed by `ListMerchantAPIKeys`, revoked by `RevokeMerchantAPIKey`): the
secret is in the result once and never stored; the non-secret `prefix`
(`openrails_st_<key_id>`) identifies the key afterwards.

Roles are the fixed merchant catalog: `viewer` (`server.MerchantBillingRead`:
read-only, the right choice for LLM agents), `support`
(`server.MerchantBillingRead` and `server.MerchantBillingManage`: acts on
customers), `owner` (everything, the merchant's configuration and the
programmatic routes included). A key is minted as an `Actor`, never with
authority beyond the actor's own.

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

## What the admin API can change

Merchant routes are scoped to the authenticated merchant; cross-merchant
operations are the standalone operator's: the server's Go methods and the
`openrails` CLI, never a route. PSPs, settings and alert webhooks change only
where Vault holds the configuration; with a file they are read-only. Catalog
routes are `Permissions.Catalog`'s, and a document skips what an edit set; the embedded
in-process Client is the process owner and writes its own catalog whatever the
mount says. Catalog data always
lives in the database. See [self-hosting-mode1.md](self-hosting-mode1.md).
