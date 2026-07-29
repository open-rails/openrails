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
- `openrails push-merchant-catalog` — products, prices, entitlements, per-PSP
  links. Default file `/etc/openrails/catalog.yaml`.

All three share one **mutation-flag contract**: a bare command is plan-only
(prints the diff, mutates nothing);

- `--insert` creates missing state;
- `--overwrite` re-asserts manifest values over existing state — without it,
  secrets are **seed-once** (a value rotated out of band is never reverted to
  the manifest seed);
- `--prune` deletes merchant secrets (or archives catalog objects) absent from
  the manifest.

The flags compose; full reconciliation is `--insert --overwrite --prune`.

MODE 2 (`merchant_source: api`) replaces this contract for
`push-merchant-config` with a single flag: `--seed` runs the command as a
**seed-once importer** (create-only — missing merchants/PSPs/secrets are
created into the persistent stores; existing values are never touched).
Without `--seed` the command refuses, and `--seed` refuses to combine with the
mutation flags: after seeding, the HTTP APIs own merchant config and the
manifest is never re-asserted over them.

Startup behavior: if `/etc/openrails/bootstrap.yaml` exists, the server applies
it **first-run only** (gated by AuthKit's bootstrap marker). Normal restarts
never reapply merchant config or catalog manifests — with one deliberate
exception: in MODE 1 (`merchant_source: manifest`, the default) the server
itself re-converges the merchant manifest on **every** boot
(insert+overwrite+prune, secrets held in memory). In MODE 2
(`merchant_source: api`) boot manifests refuse to load; the one-time bootstrap
path is `push-merchant-config --seed`, which imports the manifest into the
persistent stores. Mode comparison:
[standalone-integration.md](standalone-integration.md#two-merchant-source-modes);
MODE 1 walkthrough: [self-hosting-mode1.md](self-hosting-mode1.md).

Embedded hosts provision merchants programmatically
(`embed.Runtime.UpsertMerchantConfig`, same manifest shape) and pass auth at
HTTP mount time; the issuer-as-owner path below is the standalone mechanism.

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
    profile:                       # customer-facing display
      display_name: MyApp Billing
      logo_url: https://myapp.example/logo.png
      from_email: billing@myapp.example
      support_url: https://myapp.example/support
    invoice:                       # arrears invoicing policy (amounts in micros)
      billing_period_boundary: calendar_month
      collection_threshold: 50_000_000
      monthly_floor: 1_000_000
    psps:                          # operator-declared rail accounts
      mobius:
        nmi:
          environment: live
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
  omitted leaves the stored value untouched. Also assignable at runtime via
  `PUT /v1/merchant/api-host` (owner-gated).
- `remote_application` — the host app's issuer (JWKS URI, inline static
  `jwks`, or raw `public_keys`), registered as merchant **owner**: delegated
  tokens signed by that issuer fully administer this one merchant and no other.
- `profile` — display name, logo, from-email, support/signup URLs.
- `invoice` — arrears invoicing policy; amounts are micros.
- `delegated_invoker_wasted_spend_windows` — per-invoker windowed spend caps.
- `psps.<key>.<rail>` — one entry per PSP. `key` is the manifest PSP name
  catalog `psp_links` and checkout use ("mobius"); the rail nests inside.
  Fields: `environment` (`test`|`live` — an **assertion** cross-checked
  against the deployment's `test_mode`, not a behavior selector; a
  contradiction refuses to boot), `account_id`, `archived`, non-secret
  `settings`, `secrets`, and (Solana) `signer`.

`account_id` is operator-declared, per rail (never derived from credentials at
runtime — details in `docs/rails/*.md`):

| Rail | `account_id` |
|---|---|
| nmi | the dashboard **Gateway ID** (NMI's merchant account id — not the ISO/reseller) — [rails/nmi.md](rails/nmi.md) |
| stripe | `acct_…`, operator-declared — [rails/stripe.md](rails/stripe.md) shows the curl to read it off your own account |
| ccbill | `clientAccnum-clientSubacc`, dash-joined (`900000-0000`) — [rails/ccbill.md](rails/ccbill.md) |
| solana | derived from the signer public key; a declared value is ignored — [rails/solana.md](rails/solana.md) |
| vaulted_card | the Basis Theory tenant id (`settings.gateway_account` names the NMI account it detokenizes into) |

### Env and secret-file overlays

Secret values should not live in the YAML. Two overlays route into the same
manifest tree, with precedence `yaml < secret files < env`:

- **Secret files**: a directory (default `/vault/secrets`, override
  `VAULT_SECRETS_PATH`) of files named like env vars, content = value.
- **Env vars**: `BILLING_MERCHANTS_<MERCHANT>_PSPS_<KEY>_<RAIL>_…`, e.g.
  `BILLING_MERCHANTS_MYAPP_PSPS_MOBIUS_NMI_SECRETS_SECURITY_KEY` →
  `merchants.myapp.psps.mobius.nmi.secrets.security_key`.

Both fail loudly on retired anchors (`_ACCOUNTS_`, `_RAIL_MERCHANT_ACCOUNTS_`,
`_PROVIDER_ACCOUNTS_` → "renamed to PSPS") and on any `BILLING_MERCHANTS_*`
name that routes to no manifest field — a typo is an error, never a silent drop.

## Secrets: seeding vs runtime source of truth

Secrets are addressed by `(merchant_id, name)`. PSP credentials use the
canonical account-scoped name (built only by `merchants.PSPSecretName`):

```text
psps/<rail>/<environment>/<account_id>/<secret_key>
```

Examples: `psps/nmi/live/100001/security_key`,
`psps/stripe/live/acct_123/webhook_signing_secret`,
`psps/ccbill/live/900000-0000/datalink_password`. The `account_id` segment is
URL-escaped (CCBill's composite id is dash-joined precisely so it never embeds
the `/` delimiter). Secret keys are validated against each rail's credential
registry — unknown keys are rejected.

Where the values live depends on the mode:

- **MODE 1** (`merchant_source: manifest`): in memory only, seeded from the
  manifest every boot. Nothing is persisted; there is no store to rotate —
  edit the file/env and reboot.
- **MODE 2** (`merchant_source: api`): a persistent backend, selected by
  `secret_backend`:
  - `vault` — Vault KV-v2 at `<mount>/openrails/merchants/<merchant-slug>/<name>`
    (e.g. `secret/openrails/merchants/myapp/psps/nmi/live/100001/security_key`);
    a Vault policy can scope a merchant to its own subtree.
  - `db` — `openrails.merchant_secrets`, envelope-encrypted via
    `openrails.merchant_deks`; `ENCRYPTION_MASTER_KEY` is required outside
    development.

  The backend is declared intent, never auto-detected — the data lives in
  exactly one place. `push-merchant-config --seed` is the bootstrap path: a
  seed-once import of a manifest file into the configured backend, through the
  same store services the HTTP APIs write through. It is idempotent and
  create-only — a re-run never reverts a value rotated via the API — and the
  store, not the file, is the runtime truth afterward. Keep the seed file
  outside `/etc/openrails/merchants.yaml` (a manifest at the conventional path
  refuses MODE-2 server boot) and delete it once seeded — it holds secret
  values.

Credentials are always loaded by merchant id at request time; they are never
global process configuration.

## Merchant lifecycle

`internal/merchants.Service`; merchant status is `active` or `deleted`.

| Operation | Behavior |
|---|---|
| `Provision` | Idempotently creates the `openrails.merchants` row and records `permission_group_id` for control-plane merchants. |
| `Export` | Writes a completed `merchant_exports` row: per-table row counts plus secret **names** — values are never exported. |
| `Delete` | Gated purge: requires `Confirm` and a completed export; purges every merchant-owned row and secret, then tombstones the directory row (`status='deleted'`, `deleted_at`). |

## API keys

Merchant-scoped backend credentials are minted through the self-serve surface
(requires the control plane; embedded hosts without one answer 501):

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

Inbound rail webhooks resolve the merchant first, then verify. Three surfaces
share one handler:

```text
POST /v1/webhooks/:provider                                  # standalone: merchant derived from the
POST /v1/webhooks/:provider/:account_id                      #   payload's account identity (NMI/CCBill; Stripe with account)
POST /v1/merchants/:merchant/webhooks/:provider              # merchant-scoped: slug in the path
POST /v1/merchants/:merchant/webhooks/:provider/:account_id  #   (+ account for multi-account rails)
POST /billing/v1/merchants/:merchant/webhooks/:provider      # embedded mount of the same handler
```

Deployments with per-merchant API hosts additionally resolve the merchant from
the `Host` header at `/webhooks/:provider[/:account_id]` (see
[operations.md](operations.md)). In every case the router is **not** the trust
boundary: OpenRails re-derives the merchant, loads that merchant's signing
secret, and verifies the signature. An unresolvable slug/host is rejected —
never routed to a default merchant.

## Admin surface

OpenRails core exposes no cross-merchant lifecycle or credential routes.
Merchant admin APIs are scoped to the authenticated merchant; in MODE 1 the
catalog and payment-provider **mutation** routes answer `405 manifest_driven`
([self-hosting-mode1.md](self-hosting-mode1.md)).
