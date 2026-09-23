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
- `openrails apply-catalog --merchant NAME --file PATH` — one catalog application
  with a durable application ID and expected revision in the document.

Catalog application uses its document contract, with `prune: false` by default.
Merchant startup initialization uses `push-merchant-config --insert` (or `--seed`)
and creates missing objects only. `--overwrite` and `--prune` are retired. Managed
credentials use Client provider publication, not a manifest import into Vault/DB.
Deliberate metadata changes use `apply-merchant-config` with an application ID and
observed revision. See [the application contract](merchant-configuration-applications.md).

AuthKit authority bootstrap remains first-run only. Merchant startup reloads
snapshot credentials while preserving existing metadata and archived accounts.
Catalog manifests are applied explicitly and are not replayed at ordinary boot.

Embedded hosts provision merchants programmatically
(`embed.Options.Merchant`, same manifest shape) and pass auth at
Runtime construction; the issuer-as-owner path below is the standalone mechanism.

## Merchant identity and names

The merchant's name is its AuthKit group's instance slug. The group UUID owns
authorization; the billing merchant UUID owns payments, credits, customers and
secrets. A non-null billing-to-group binding is immutable, including after
retirement. Repeated provisioning of the same group returns the same billing
UUID. A different group claiming an old name cannot take over that billing row.

For AuthKit-bound merchants, resolve the requested name through AuthKit first,
then authorize and select billing data by the captured group UUID. The local
`openrails.merchants.slug` is a display projection. API writes keep their method,
body, authorization and idempotency key; forwarding is internal resolution, not
an HTTP redirect to another owner. Webhooks retain provider signature/account
checks after name resolution.

AuthKit owns one site policy for usernames and group names. Defaults allow a
rename every **72 hours** and reserve/forward each former name for **90 days**.
Aliases point directly to the immutable owner. Expiry is checked when resolving
or claiming a name; cleanup is not required before an expired name is available.
Later policy changes do not alter promises already recorded for former names.

Standalone configuration (these are the defaults):

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

Embedded hosts pass the same `authkit.NamingConfig` through `cfg.Auth.Naming`.
A non-nil `AttachOptions.Naming` replaces that whole input; AuthKit alone validates
and supplies defaults. `AttachOptions.NameAdmission` adds a host namespace rule
for both creation and rename. Creation allowance/payment-method checks remain
creation-only. The merchant persona's reserved names and slug pattern apply to
renames too. Customer group handles encode immutable payer IDs and cannot be
renamed; their display names can change.

Use `ResolveAuthorizedMerchant` at a host authorization boundary. With an empty
selector it infers the user's sole merchant group by UUID. If an authorized
operation subsequently calls a name-addressed AuthKit API, carry
`BindMerchantGroupContext(ctx, merchantID, originalReference)` into that call.
This pins its target without granting permissions; a deleted target fails closed
rather than resolving the same spelling to a new owner. Team/API-key wrappers
already accept the billing UUID and carry this scope themselves.

AuthKit-free embedded hosts explicitly own their local merchant names and
provide their own authorization. They do not gain AuthKit alias policy by
attaching a name to an unbound billing row.

### Hosted creation recipe (registration is provisioning)

A hosted product (openrails-saas shape) wires everything through
`controlplane.Options.MerchantCreation` (`embed/controlplane`):

```go
cp, err := controlplane.Attach(ctx, rt, controlplane.Options{
    HostedPosture: true,
    EmailSender:   sender,
    MerchantCreation: &controlplane.MerchantCreationConfig{
        ReservedSlugs: []string{"my-brand"}, // + merchant.ReservedHostedSlugs, always
        Admission: func(ctx context.Context, slug, ownerUserID string) error {
            return nil // host cost gate: allowance / card-on-file (or#914 item 3)
        },
    },
})
```

That one option: (1) mounts authkit's `POST /merchant` — authenticated users
claim a slug behind authkit's per-IP/per-user velocity limits, the reserved
list, and your admission gate — and OpenRails attaches the
`openrails.merchants` directory row on success, so one call is the whole
"registration is provisioning" flow (re-POSTing the same slug is the
idempotent repair; the response carries `group_id`); (2) holds in-process
`ProvisionMerchant` calls that name an `OwnerUserID` to the SAME policy
(typed refusals `controlplane.ErrSlugReserved` / `controlplane.ErrCreationRefused`).
Ownerless `ProvisionMerchant` and Bootstrap are operator acts and stay
ungated — that is how a platform merchant claims a reserved name.

For the Admission gate itself, `controlplane.MerchantCreationAdmission` composes the
standard hosted policy from openrails' own state — verified email always; a
free allowance of OWNED merchants; beyond it, a vaulted payment method on
file (no charge) unlocks more:

```go
admission, err := controlplane.MerchantCreationAdmission(rt, controlplane.MerchantCreationPolicy{
    FreeAllowance: 2,
    HasVaultedPaymentMethod: func(ctx context.Context, subject string) (bool, error) {
        // openrails-saas shape: the platform merchant's book holds the vault.
        return controlplane.SubjectHasVaultedPaymentMethod(ctx, rt, platformMerchantID, subject)
    },
})
```

The predicate is repair-safe: after verifying the owner, it resolves the
claimed slug to AuthKit's stable group ID. Re-posting a live or renamed-away
slug that resolves to a merchant the caller already owns bypasses the
allowance and vault checks because it creates nothing. A genuinely new slug
still runs the full gate.

Typed refusals: `controlplane.ErrEmailUnverified`, `controlplane.ErrVaultedPaymentMethodRequired`.

### Merchant retirement (never-used names go back in the pool)

Core provides the mechanism; when to warn about and retire an unused merchant
is the host's policy (openrails-saas owns its own, with its own notices).

- `cp.ListMerchantRetirementCandidates(ctx, req)` pages live,
  group-bound merchants created before `req.CreatedBefore`, oldest first,
  excluding reserved slugs (`merchant.ReservedHostedSlugs` plus
  `MerchantCreationConfig.ReservedSlugs`). Each candidate carries `Used`, probed
  under the merchant's own RLS scope.
- `cp.RetireUnusedMerchant(ctx, merchantID, groupID)` locks the merchant
  row, refuses a missing/retired merchant, a different group UUID, a reserved
  slug or any activity, and otherwise commits the irreversible tombstone before
  deleting exactly that AuthKit group with `ReleaseSlug: true`. Refusals are
  returned in the result.
- `cp.CompletePendingMerchantRetirements(ctx, limit)` retries committed
  retirements whose group release failed, by UUID, so a released name reclaimed
  in between is never deleted.

Activity is any customer (and everything owned through customers), payment or
subscription history including tombstones, ledger account, provider connection
(PSP, custodian, stored secret, applied webhook, rail intent), undelivered host
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
    profile:                       # customer-facing display
      display_name: MyApp Billing
      logo_url: https://myapp.example/logo.png
      from_email: billing@myapp.example
      support_url: https://myapp.example/support
    invoice:                       # arrears invoicing policy (amounts in micros)
      billing_period_boundary: calendar_month
      collection_threshold: 50_000_000
      monthly_floor: 1_000_000
      delinquency_grace_days: 7    # days past due before delinquent (default 14)
    psps:                          # operator-declared rail accounts
      mobius:
        nmi:
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
- `invoice` — arrears invoicing policy; amounts are micros. Includes the
  delinquency policy (`delinquency_grace_days`, `delinquency_amount_floor`) —
  see `docs/arrears-delinquency.md` for what OpenRails enforces (new spend) and
  what the operator must integrate (existing resources).
- `delegated_invoker_wasted_spend_windows` — per-invoker windowed spend caps.
- `psps.<key>.<rail>` — one entry per PSP. `key` is the manifest PSP name
  catalog `psp_links` and checkout use ("mobius"); the rail nests inside.
  Fields: `account_id`, `archived`, non-secret `settings`, `secrets`, and
  (Solana) `signer`. There is NO `environment` (#882) — it is derived from the
  deployment's `test_mode` (sandbox ⇒ `test`, live ⇒ `live`), so a deployment is
  all-test or all-live; a manifest still declaring it fails loudly.

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
in the manifest's own shape (`merchants.<slug>.psps.<key>.<rail>.secrets.*`),
merged over the manifest in order (later wins) and strict-parsed with it:
an unknown field, or secrets for a PSP the manifest never declared, is an
error, never a silent drop.

- Embedded hosts pass them from their own config tree:
  `embed.LoadMerchantConfigManifestWithOverlays(manifest, overlays...)`.
- Standalone snapshot custody lists mounted files in `merchant_manifest_overlays`
  (env `MERCHANT_MANIFEST_OVERLAYS`).

The engine itself reads no `BILLING_MERCHANTS_*` env and no secret directory.

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

Credential custody is selected explicitly by `secret_backend`:

- `snapshot`: host-owned in-memory values, reloaded and validated at startup.
- `vault`: managed KV-v2 with server-owned mount/scope and actual policy rights.
- `db`: envelope-encrypted PostgreSQL storage; `ENCRYPTION_MASTER_KEY` is required
  for managed storage in both sandbox and live deployments.

Managed provider writes use versioned Client publication with stable operation
identity and a revision precondition. Readers resolve the published credential
version. Direct backend edits do not publish credentials. Changing custody is an
explicit migration, independent of external HTTP publication. Snapshot values
are never implicitly copied to a managed backend.

## Merchant lifecycle

`internal/merchants.Service`; merchant status is `active` or `deleted`.

| Operation | Behavior |
|---|---|
| `Provision` | Idempotently creates the `openrails.merchants` row and records `permission_group_id` for control-plane merchants. |
| `TakePurgeInventory` | Records what a purge would destroy: per-table row counts, secret **names**, and an explicit `not_captured` list. **It is not a backup and restores nothing** (was `Export`, a name that implied otherwise). |
| `Delete` | One-way gated purge. See below. |

### The purge is one-way, and the inventory is not a backup

`TakePurgeInventory` copies no data. It writes counts and secret names to
`openrails.maintenance_runs` so an operator sees the blast radius
before confirming — nothing more. The only way back from a purge is
**whole-cluster Postgres point-in-time recovery** with the
`ENCRYPTION_MASTER_KEY` and Vault alongside it (`backup-and-recovery.md`).

`Delete` therefore stands behind five walls:

1. the destructive-action gate (`operations.md`) — the same instance kill switch
   and per-merchant policy that hold a mass cancellation. **Unwired = denied.**
2. a typed confirmation phrase: `purge merchant <slug> permanently, no backup exists`
3. a typed row count that must equal the true total
4. a purge inventory recorded against *that* count — a stale one authorises nothing
5. a `maintenance_runs` row (`kind='merchant_purge'`) recording who, what and how many

**There is deliberately no route and no CLI for it.** A test guard fails the
build if `merchants.DeleteOptions` is constructed anywhere outside the package.
The purge gains an operator surface when a real per-merchant snapshot/restore
exists to gate it on, not before.

Two things a purge cannot do, by construction:

- **It cannot revoke stored instruments.** It drops the local custody mirror;
  the cards stay at the PSP. Only the end user deletes their own instrument.
- **It cannot delete a merchant with real billing history.** The append-only
  grant log FK-pins the products and payments it justifies and is never purged,
  so `Delete` refuses whole rather than half-purging. Retire the merchant
  instead.

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
POST /v1/webhooks/:rail/:account_id                          # standalone: configured account resolves merchant
POST /billing/v1/webhooks/:rail/:account_id                  # embedded under /billing
```

`:rail` is the gateway KIND — `nmi`, `ccbill`, `stripe`, `solana`,
`basistheory` — never a PSP key. A PSP is named by `:account_id`, not by the
rail segment: `mobius` and `paykings` both post to `/v1/webhooks/nmi/{account_id}`. Accountless routes are not registered; payload account identities must agree with the selected account.

Before deploying this route contract, update existing provider webhook registrations that omit the account segment. Old URLs return404. Updating the callback URL does not change subscription ownership or billing schedules.

Provider account identity resolves the merchant in the runtime's configured
sandbox/live environment. A runtime bound to one merchant refuses another
merchant's account. The account's signature or provider-specific verification
must pass, and payload account identity must agree. Host headers and merchant
slugs do not grant callback authority. The public billing base supplies only the
external mount prefix; generated Stripe URLs use this same path.

## Admin surface

OpenRails core exposes no cross-merchant lifecycle or credential routes.
Merchant admin APIs are scoped to the authenticated merchant. Payment-provider
configuration routes require explicit HTTP publication. Snapshot credential writes
are unavailable, while authorized metadata operations and archive decisions remain
available through the Client. Catalog mutation routes are omitted unless
`allow_catalog_updates: true`; the same policy denies ordinary embedded Client
writes. Catalog reads remain available, and trusted local operator application
is independent of this flag. Catalog data always lives in the database.
See [self-hosting-mode1.md](self-hosting-mode1.md).
