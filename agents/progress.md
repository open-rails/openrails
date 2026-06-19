<!-- openrails issue tracker — ACTIVE issues -->

> One `# #<id>: <name>` section per issue, separated by `---` lines; section anchor for
> tooling is a line starting with `# #`. IDs are stable for an issue's whole lifecycle and
> share ONE per-repo id space across progress.md / future.md / completed.md; new issues take `next_id` below and bump it.
> CONCURRENT EDITS: only ever edit/append your own issue's section with targeted string
> replacement — never rewrite the whole file.


next_id: 534

---

# #533: Log external provider mutations executed by convergence/provider intents

**Completed:** no

The convergence engine and provider-intent executor need an append-only record of every external payment-provider mutation they actually execute. We do not need an audit log for ordinary internal DB changes here; Postgres state and reconcile findings already cover that. The important evidence is: "OpenRails called Stripe/NMI/CCBill/Solana and changed something outside our database."

This should be symmetric with `pull-provider`:

- `pull-provider` logs/plans local mirror changes it would make and records what it did make when mutation flags are used.
- Provider-intent/convergence execution should log remote mutations it is about to attempt and the remote mutation result once attempted.

## Scope

Log only external side effects, for example:

- provider subscription cancel/delete/update
- provider refund/void/capture
- provider catalog product/price/plan create/update/archive
- provider payment retry or rebill attempt
- provider customer/payment-method/vault mutation
- Solana on-chain transaction submission when OpenRails submits the transaction

Do not log routine internal state repairs, grant derivation, lifecycle status flips, local mirror upserts, or other Postgres-only convergence changes in this issue.

## Target Model

- Add a structured append-only external mutation log, likely table-backed so it is queryable by merchant/provider/account/intent/run.
- Link each event to the source of the side effect when available:
  - `provider_intent_id`
  - `convergence_run_id` or finding id if applicable
  - merchant id
  - provider account id
  - provider type/environment/account id
  - idempotency key / request id
  - remote object id(s)
- Store action metadata in a scrubbed form: action type, target type/id, non-secret parameters, status, error class/message, remote response identifiers. Never store provider API keys, webhook secrets, card data, private keys, raw Authorization headers, or full unsanitized request/response bodies.
- Record at least two phases:
  - `planned` / `attempting`: OpenRails decided to call the provider.
  - `succeeded` or `failed`: provider call result.
- Make retry behavior explicit: multiple attempts against the same provider intent should be separate attempts linked to the same intent/idempotency key, not overwritten history.
- Use the same merchant/provider-account scoping from #530/#529 so an external mutation log can never ambiguously mix provider accounts.

## CLI / Operator Surface

- `pull-provider` remains the read-side mirror command and should continue to show would-change/did-change local mirror behavior.
- Add or extend an operator-facing command/report for external mutation history, probably under `openrails intents` or `openrails provider-actions`, for example:

```bash
openrails intents log --merchant doujins --provider stripe
openrails intents log --merchant doujins --intent <uuid>
```

- The output should be safe for terminals/log collectors by default: compact summary in table mode, full scrubbed structured fields in `--format json`.

## Tasks

- [ ] Inventory every code path that mutates a remote provider from provider intents, convergence repairs, catalog push, refund/cancel/rebill flows, and Solana submission.
- [ ] Design and add the append-only external mutation log schema with merchant/provider-account/idempotency linkage.
- [ ] Add a small writer API used at the provider boundary, not scattered ad hoc across business logic.
- [ ] Wrap provider-intent execution so every external attempt writes `attempting` then `succeeded`/`failed`.
- [ ] Ensure retries append attempts instead of mutating away prior history.
- [ ] Ensure logs are scrubbed and add tests proving secrets/card/private-key material is not persisted.
- [ ] Add CLI/report surface for querying external provider mutation history.
- [ ] Update `pull-provider` reporting/docs to describe the symmetry: pull logs local mirror changes; provider-intent/convergence logs remote provider changes.
- [ ] Add integration tests with real local HTTP provider fakes/dev servers where available, proving a remote mutation creates the expected external mutation log entries.

---

# #532: Standardize CLI mutation flags: insert, overwrite, prune

**Completed:** yes

Several OpenRails CLI commands reconcile one source of truth into another. They should share the same operator safety model: by default, commands plan and report only. Mutations happen only when the operator explicitly enables the mutation class.

## Target Semantics

- No mutation flags: dry-run / plan-only. Print what would change and persist no changes unless a command has a separately documented read-only report side effect.
- `--insert`: create missing records or provider objects.
- `--overwrite`: update existing records or overwrite mutable/provider-owned fields.
- `--prune`: remove, disable, archive, or tombstone records/objects absent from the declared or observed source.
- Flags compose. Full reconcile-to-source is `--insert --overwrite --prune`.
- Keep remote provider safety explicit: `pull-provider` still never mutates external processors; its flags only mutate the OpenRails local mirror/converged state.

## Command Mapping

- `openrails push-bootstrap`
  - `--insert`: create missing initial platform/AuthKit/control-plane seed objects.
  - `--overwrite`: update seeded authority fields, keys, permissions, or other mutable bootstrap-owned data.
  - `--prune`: remove/disable bootstrap-owned objects absent from the bootstrap file, within a narrowly documented scope.

- `openrails push-merchant-config`
  - `--insert`: create missing merchants, profiles, issuer links, provider accounts, and secrets.
  - `--overwrite`: update existing merchant config/profile/provider secret values.
  - `--prune`: disable/delete config, secrets, or provider accounts absent from the manifest according to the provider-account safety rules.

- `openrails push-merchant-catalog`
  - `--insert`: create missing products, prices, tier groups, catalog metadata, and provider catalog objects.
  - `--overwrite`: update existing OpenRails-owned catalog fields/provider catalog mappings.
  - `--prune`: archive OpenRails-owned catalog extras absent from the desired catalog. Never touch foreign provider objects.

- `openrails pull-provider`
  - `--insert`: import provider-observed records missing locally.
  - `--overwrite`: update existing local mirror rows from provider-observed truth.
  - `--prune`: delete/tombstone/archive eligible local mirror rows attributed to the pulled provider account that are absent from the provider source.

## Tasks

- [x] Replace command-specific "dry-run unless --overwrite" wording with the shared mutation-flag contract.
- [x] Add `--insert` to `pull-provider`; make current provider-missing materialization require `--insert`, not `--overwrite`.
- [x] Change `pull-provider --overwrite` to update existing local mirror/derived local rows without importing provider-missing records.
- [x] Make `pull-provider --prune` the explicit destructive opt-in for eligible provider-account-scoped excess rows; it does not require `--overwrite`.
- [x] Update `push-merchant-config` to use `--insert --overwrite --prune` rather than "default seed-once plus overwrite/prune" as the public mental model.
- [x] Update `push-merchant-catalog` so default is plan-only and writes require `--insert` and/or `--overwrite`; preserve `--prune` as archive-only for OpenRails-owned extras.
- [x] Update `push-bootstrap`/`push-merchant-config` manual runs to default to plan-only; startup first-run behavior remains separately documented and insert-only.
- [x] Add CLI help tests/smokes proving each command exposes the same mutation flags and dry-run default.
- [x] Update `docs/operations.md` and examples/runbooks to show plan-only first, then explicit mutation flags.
- [x] Cross-reference #533 so local mirror mutation logs and external provider mutation logs use consistent terms for planned/applied changes.

Validation:

- `go test ./pkg/catalog ./cmd/openrails ./internal/reconcile ./internal/bootstrap`
- `go test -tags integration ./internal/integrationharness -run TestStandaloneMerchantCatalogApplyOptionsOverHTTP -count=1`
- `go test ./...`

---

# #531: Split bootstrap, merchant config, and catalog into separate CLI/file surfaces

**Completed:** no

The current `push-merchant-config` direction is doing too much under one "bootstrap" concept. Separate the lifecycle surfaces so initial empty-DB provisioning can run all three, but normal operations can re-run only the part that changed.

## Target Split

1. **Platform/bootstrap authority**
   - File: `/etc/openrails/bootstrap.yaml` or `/run/openrails/bootstrap.yaml`
   - Command: `openrails push-bootstrap --file ...`
   - Scope: initial OpenRails/AuthKit platform/control-plane authority only: instance issuer/JWKS/signing/public authority wiring, bootstrap administrative authority if needed, and other global control-plane seed state.
   - Not merchant provider secrets.
   - Not products/prices.
   - Startup auto-run, if present, is first-run only and non-destructive.

2. **Merchant config**
   - File: `/etc/openrails/merchants.yaml` or `/run/openrails/merchants.yaml`
   - Command: `openrails push-merchant-config --file ...`
   - Scope: merchant rows/profile, merchant issuer ownership, provider accounts, provider credentials/secrets, webhook route instructions/docs, merchant-level operational settings.
   - Uses the shared #532 mutation flags: default plan-only, `--insert`, `--overwrite`, and `--prune`.

3. **Merchant catalog**
   - File: `/etc/openrails/catalog.yaml` or `/run/openrails/catalog.yaml`
   - Command: `openrails push-merchant-catalog --file ...`
   - Scope: products, prices, tier groups, entitlements/catalog metadata, provider catalog push/links.
   - Uses the shared #532 mutation flags. `--prune` means catalog-prune semantics only: archive OpenRails-owned provider/catalog extras absent from desired catalog.

4. **Provider pull**
   - No YAML file; provider APIs are the source of truth.
   - Command: `openrails pull-provider --merchant ...`
   - Scope: pull provider-observed state into OpenRails' local mirror, then converge local derived state.
   - Uses the shared #532 mutation flags. It never mutates external processors.

## Command Names

Use these four operator-facing commands:

```bash
openrails push-bootstrap --config /etc/openrails/config.yaml --file /run/openrails/bootstrap.yaml
openrails push-merchant-config --config /etc/openrails/config.yaml --file /run/openrails/merchants.yaml
openrails push-merchant-catalog --config /etc/openrails/config.yaml --file /run/openrails/catalog.yaml
openrails pull-provider --config /etc/openrails/config.yaml --merchant doujins --provider stripe
```

The first three are declarative file-backed push commands. They plan only unless `--insert`, `--overwrite`, or `--prune` is present. `pull-provider` is not file-backed; it reconciles from provider-observed state.

Naming rationale: the file-backed commands push declared state into OpenRails-owned/external systems such as AuthKit/control-plane state, HashiCorp Vault or the DB secret backend, and remote provider catalog surfaces. `pull-provider` moves the opposite direction: provider-observed state back into OpenRails' local mirror.

## Operations Manual

Update `docs/operations.md` as the operator manual for running OpenRails. Do not create a root `OPERATIONS.md` unless the docs layout changes; this repo already keeps operational runbooks under `docs/`.

The manual must document at least:

- the four command surfaces:

```bash
openrails push-bootstrap --file /run/openrails/bootstrap.yaml
openrails push-merchant-config --file /run/openrails/merchants.yaml
openrails push-merchant-catalog --file /run/openrails/catalog.yaml
openrails pull-provider --merchant doujins --provider stripe
```

- the default plan-only behavior when no mutation flags are supplied.
- the shared mutation flags: `--insert`, `--overwrite`, `--prune`.
- empty-DB/private standalone initialization order.
- normal operational re-runs: config rotation, catalog changes, provider pull/reconcile.
- the split between declarative file-backed `push-*` commands and provider-observed `pull-provider`.
- the relation to #533: local mirror changes are reported by `pull-provider`; remote provider mutations are logged by provider-intent/convergence execution.

## Target YAML Shapes

### `bootstrap.yaml`

Initial platform/control-plane authority only. Do not put merchant provider credentials, merchant profiles, products, or prices here. Runtime issuer/signing-key location remains infrastructure config/env, not bootstrap state.

```yaml
version: 1

authority:
  api_keys:
    - name: openrails-bootstrap-admin
      secret:
        env: OPENRAILS_BOOTSTRAP_ADMIN_API_KEY
      permissions:
        - openrails:instance:admin

  users:
    - email: admin@example.com
      password:
        env: OPENRAILS_BOOTSTRAP_ADMIN_PASSWORD
      permissions:
        - openrails:instance:admin

  remote_applications:
    - issuer: https://ops.example.com
      jwks_uri: https://ops.example.com/.well-known/jwks.json
      audiences: [openrails]
      permissions:
        - openrails:instance:admin
```

### `merchants.yaml`

Merchant identity, issuer ownership, browser origins, profile, provider accounts, and provider secrets only. The provider `account_id` should be discovered from credentials wherever possible; the manifest should not require it for providers with a reliable whoami/account endpoint.

```yaml
version: 1

merchants:
  - slug: doujins
    display_name: Doujins

    issuer:
      uri: https://doujins.com
      jwks_uri: https://doujins.com/.well-known/jwks.json
      audiences: [openrails]
      allowed_origins:
        - https://doujins.com

    profile:
      display_name: Doujins
      logo_url: https://doujins.com/assets/logo.png
      from_email: billing@doujins.com
      support_url: https://doujins.com/support

    provider_accounts:
      - provider_type: stripe
        environment: live
        mode: primary
        secrets:
          secret_key:
            env: DOUJINS_STRIPE_SECRET_KEY
          webhook_signing_secret:
            env: DOUJINS_STRIPE_WEBHOOK_SECRET

      - provider_type: nmi
        environment: live
        mode: secondary
        secrets:
          production_key:
            env: DOUJINS_MOBIUS_PRODUCTION_KEY
          tokenization_key:
            env: DOUJINS_MOBIUS_TOKENIZATION_KEY
          webhook_signing_secret:
            env: DOUJINS_MOBIUS_WEBHOOK_SECRET
```

### `catalog.yaml`

Products, prices, tier groups, entitlements, and provider catalog links only. No merchant profile, provider credentials, users, orgs, or API keys.

```yaml
version: 1

catalogs:
  - merchant: doujins

    default_providers: [stripe, nmi]

    tier_groups:
      - slug: memberships
        display_name: Memberships

        products:
          - slug: basic
            display_name: Basic
            description: Basic recurring membership.
            tier_rank: 1
            status: active
            entitlements:
              - access.basic

            prices:
              - currency: usd
                unit_amount: 999
                interval: month
                interval_count: 1
                provider_links:
                  stripe:
                    lookup_key: doujins-basic-monthly
                  nmi:
                    plan_id: doujins_basic_monthly

          - slug: premium
            display_name: Premium
            description: Premium recurring membership.
            tier_rank: 2
            status: active
            entitlements:
              - access.basic
              - access.premium

            prices:
              - currency: usd
                unit_amount: 1999
                interval: month
                interval_count: 1
                provider_links:
                  stripe:
                    lookup_key: doujins-premium-monthly
                  nmi:
                    plan_id: doujins_premium_monthly
```

## Initial Provisioning

On an empty DB/private standalone install, an operator or init job may run all three in order:

```bash
openrails push-bootstrap --config /etc/openrails/config.yaml --file /run/openrails/bootstrap.yaml --insert
openrails push-merchant-config --config /etc/openrails/config.yaml --file /run/openrails/merchants.yaml --insert
openrails push-merchant-catalog --config /etc/openrails/config.yaml --file /run/openrails/catalog.yaml --insert --overwrite
```

After initial provisioning, each command is run independently based on what changed. Do not make normal server restarts silently reconcile all three surfaces.

## Tasks

- [ ] Rename/restructure CLI commands as `push-bootstrap`, `push-merchant-config`, and `push-merchant-catalog` with no `apply` subcommand; mutation is controlled only by `--insert`, `--overwrite`, and `--prune`.
- [ ] Split manifest structs/parsers/examples/docs into three files with no overlapping top-level keys.
- [ ] Keep first-run startup auto-run limited to platform/bootstrap authority only, or require an explicit init mode/job to run all three. Do not run merchant config/catalog reconcile on every server boot.
- [ ] Preserve manual mutation safety using the shared mutation flag contract from #532: default plan-only, `--insert`, `--overwrite`, and `--prune`.
- [ ] Update `config/merchants.example.yaml`, `config/catalog.example.yaml`, and add/repair `config/bootstrap.example.yaml` so each file demonstrates only its own scope.
- [ ] Update `docs/operations.md` to explain the four command surfaces, dry-run default, mutation flags, empty-DB initialization order, and later independent operational re-runs.
- [ ] Add CLI tests proving each command rejects keys owned by the other manifest types.

---

# #530: Bootstrap secrets must use runtime secret backend and provider-account-scoped attribution

**Completed:** no

Current problem: runtime OpenRails can use a Vault-backed `MerchantSecretStore` when `vault.enabled: true`, but `push-merchant-config` currently builds `merchants.NewDBSecretStore(cp.Pool())` directly. That means a Vault-enabled deployment can import bootstrap provider secrets into the DB secret store while the running server expects them in Vault. Also, provider secrets are still broad merchant secret names like `stripe/secret_key`, which is not enough for multiple same-provider accounts or live/test separation.

Security goal: bootstrap secret import and runtime secret reads must use the same backend and the same provider-account identity model. A secret imported for one merchant/provider account/environment must never be read or applied as another merchant/provider account/environment's credential.

## Target Model

- One secret-store selection function shared by runtime server construction, startup bootstrap, and manual `push-merchant-config`.
- If `vault.enabled: true`, bootstrap writes imported provider secrets into OpenRails' canonical Vault paths, not DB.
- If Vault is disabled, bootstrap writes into the DB-backed merchant secret store, with envelope encryption when configured.
- Provider account identity includes `(merchant_id, provider_type, environment, account_id)`, where `environment` is `live` or `test`.
- Provider account identity is discovered from credentials whenever possible before account registration:
  - Stripe: `/v1/account`
  - NMI/Mobius: profile/account identity
  - CCBill: account/subaccount config until a better whoami exists
  - Solana: public wallet/authority
- Secrets should be stored/addressed in a way that cannot collide across multiple provider accounts of the same type for the same merchant. Broad names like `stripe/secret_key` are only safe for single-account compatibility and should not be the long-term primary addressing model.
- Bootstrap file/secret material is seed input only. After import, OpenRails' canonical secret backend is runtime truth.

## Tasks

- [ ] Extract a shared secret-store builder used by both standalone runtime wiring and `push-merchant-config` / startup bootstrap.
- [ ] Make `push-merchant-config` honor `config.vault.enabled`; Vault-enabled deployments must import provider secrets into Vault-backed `MerchantSecretStore`.
- [ ] Add integration coverage proving bootstrap import writes to Vault when Vault is enabled and runtime reads the same value from Vault.
- [ ] Add integration coverage proving DB-backed bootstrap still works and uses envelope encryption when configured.
- [ ] Add provider-account environment (`live|test`) to DB schema, generated queries, provider account upsert/find/list APIs, and relevant unique indexes.
- [ ] Change provider secret addressing to include provider account identity or provider account id, so multiple Stripe/NMI/etc accounts for one merchant cannot overwrite each other's credentials.
- [ ] Update bootstrap provider-account reconciliation to resolve account identity from credentials before writing/registering account rows; fail clearly if identity cannot be resolved for providers that require discovery.
- [ ] Ensure checkout/session stamping, provider intents, provider-pull, mirror rows, and webhook credential lookup all use the resolved provider account/environment and do not fall back across accounts.
- [ ] Update admin/merchant secret APIs to show/write provider-account-scoped secrets, not only broad merchant-level secret names.
- [ ] Update docs/examples/runbooks to explain seed material vs runtime secret backend, Vault import paths, and provider-account-scoped secret ownership.

---

# #529: Make webhook routing explicit: `/merchants/:merchant`, no bootstrap host/path defaults

**Completed:** yes

Private standalone and embedded OpenRails should route provider webhooks through an explicit merchant path, not by guessing from the provider payload and not by storing the deployment mount URL in each merchant's bootstrap config.

Default private/embedded shape:

```text
/v1/merchants/{merchant}/webhooks/{provider}
/billing/v1/merchants/{merchant}/webhooks/{provider}   # when embedded under /billing
```

OpenRails SaaS can use host/subdomain routing instead, e.g. `https://doujins.openrails.com/v1/webhooks/stripe`; that is tracked in `~/openrails-saas` and should not force private/self-hosted operators to configure per-merchant hosts.

## Decisions

- Use `/merchants/:merchant/webhooks/:provider` as the clear operator-facing route. The current `/m/:merchant/webhooks/:provider` is too terse for docs and provider dashboards.
- Each merchant/provider account should be configured at the provider to call that merchant's exact webhook URL.
- Merchant routing must happen from a trusted envelope before payload trust: explicit path slug or host/subdomain. Do **not** infer merchant solely from an unverified provider payload.
- After route resolution, load that merchant/provider account's webhook secret and verify the signature/IP/provider authentication with the merchant-specific credentials.
- `webhook_host` / `webhook_path` are not normal bootstrap config for private standalone. The OpenRails mount/base URL is deployment config; merchant bootstrap should not repeat it.
- `billing_tier` and `region` are SaaS/platform hosting metadata, not merchant billing configuration. Delete them from core OpenRails bootstrap/model; if `openrails-saas` needs plan/region data, it should own that in SaaS-specific state.
- Replace merchant `name` with one operator/customer-facing `display_name`. Do not keep both `merchants.name` and `profile.display_name` in the bootstrap shape. Hard cut: there is no legacy fallback or compatibility path; we have no old data to preserve. Store display/branding data in merchant configuration/profile; `slug` remains the stable identifier.
- `issuer.allowed_origins` remains part of the merchant bootstrap shape because it declares the merchant browser origins. CORS implementation belongs to #519, not this webhook/bootstrap issue.

## Target merchant bootstrap shape

```yaml
version: 1

merchants:
  - slug: doujins
    display_name: Doujins

    issuer:
      uri: https://doujins.com
      jwks_uri: https://doujins.com/.well-known/jwks.json
      audiences: [openrails]
      allowed_origins:
        - https://doujins.com

    profile:
      display_name: Doujins
      logo_url: https://doujins.com/logo.png
      from_email: billing@doujins.com
      support_url: https://doujins.com/support

    provider_accounts:
      - provider_type: stripe
        environment: live
        mode: primary
        secrets:
          secret_key: {env: DOUJINS_STRIPE_SECRET_KEY}
          webhook_signing_secret: {env: DOUJINS_STRIPE_WEBHOOK_SECRET}
```

Removed from this shape: `name`, `billing_tier`, `region`, `webhook_host`, `webhook_path`, `profile.support_email`. Provider webhook URLs are deployment/operator instructions, not merchant bootstrap fields.

Production bootstrap delivery:
- Preferred production path: store the full bootstrap YAML as a deploy-time Vault secret, fetch/render it into the container at startup as an ephemeral file (for example `/run/openrails/bootstrap.yaml`), run first-run `push-merchant-config`, then treat OpenRails' canonical merchant secret backend as the runtime source of truth.
- The Vault bootstrap YAML is seed material, not the steady-state source of truth. Startup bootstrap remains first-run only; later secret rotations happen through OpenRails' merchant secret backend/admin surface unless an operator explicitly runs manual bootstrap with overwrite.
- Use separate Vault policy/roles where possible: a short-lived bootstrap role can read the deploy bootstrap YAML; the long-running OpenRails role should only access OpenRails-owned runtime secret paths.
- Do not bake the bootstrap YAML into the image and do not commit inline-secret bootstrap files to git. Local dev may use checked-in examples with env/file placeholders only.

Provider-account notes:
- Provider account identity includes environment. The public enum is `live` or `test`; omitted means `live`. Provider-specific terms like sandbox/devnet/testnet normalize to `test`. Live and test credentials are separate provider accounts, not two keys on one account. Effective identity is `(merchant_id, provider_type, environment, provider_account_id)`.
- `account_id` is the durable provider-returned account identity inside that environment. Do not require operators to hand-write it when OpenRails can discover it from credentials: Stripe `/v1/account`, NMI/Mobius profile report, CCBill account/subaccount config, Solana public wallet/authority.
- `provider_key` is only a local/process selector, not durable identity. Do not expose it in the normal bootstrap shape unless a later CLI genuinely needs an explicit selector for multiple same-type accounts.
- `display_name` is optional operational labeling and should not be part of the minimal bootstrap shape.
- Replace public `role` + `status` with one `mode`: `primary`, `secondary`, `legacy`, or `disabled`. Internally this may still map to role/status columns, but the manifest should use one field.
- Do not let global `test_env` silently swap a live provider account to test credentials. The selected provider account/environment must be explicit. Production should reject `environment: test` as `mode: primary` unless the deployment is explicitly non-production.

## Tasks

- [x] Rename/add the merchant-scoped webhook route from `/v1/m/:merchant/webhooks/:provider` to `/v1/merchants/:merchant/webhooks/:provider`; hard cut: no `/m/...` alias.
- [x] Update embedded route docs/examples so hosts configure provider callbacks like `/billing/v1/merchants/{merchant}/webhooks/{provider}` when OpenRails is mounted under `/billing`.
- [x] Extend merchant-scoped webhook handling beyond Stripe: Stripe, NMI/Mobius, and CCBill now resolve merchant first and verify/provider-check after resolution.
- [x] Ensure provider account selection is account-aware: route resolves merchant first, then merchant-scoped provider credentials are loaded for that merchant, with no cross-merchant fallback.
- [x] Remove `webhook_host` and `webhook_path` from `config/merchants.example.yaml` and from the recommended bootstrap shape in docs; leave host-based routing as an advanced/SaaS concern.
- [x] Remove `billing_tier` and `region` from `ManifestMerchant`, merchant provisioning APIs, examples, docs, admin responses, and schema if no core runtime dependency remains.
- [x] Replace bootstrap `name` with required `display_name`.
- [x] Drop `name` from `openrails.merchants` entirely; do not add `display_name` to `openrails.merchants` unless a measured list-query problem later requires denormalization.
- [x] Remove `Name` from `ManifestMerchant`, `merchants.ProvisionRequest`, and `merchants.Merchant`.
- [x] Store profile fields only in merchant configuration/profile: `display_name`, `logo_url`, `from_email`, and `support_url`. Remove `support_email`.
- [x] Update admin views/API responses to return `display_name`, not `name`.
- [x] Update `RegisterMerchant`, provisioning SQL, sqlc generated code, migrations, examples, docs, and tests for the hard cut.
- [x] Keep `issuer.allowed_origins` in the example only as merchant browser-origin declaration; defer all CORS behavior/docs/tests to #519.
- [x] Simplify provider account manifest fields: replace public `role` + `status` with `mode: primary|secondary|legacy|disabled`.
- [x] Add provider account `environment` enum: `live` or `test`; omitted/default is `live`. Normalize provider-specific terms (`prod`/`production` -> `live`; `sandbox`/`devnet`/`testnet` -> `test`) at manifest/API boundaries. Include environment in uniqueness, row identity, mirror rows, intent binding, checkout/session stamping, and provider-pull matching.
- [x] Fail bootstrap clearly if account identity cannot be resolved for a provider account being registered: `account_id` is required until bootstrap-time provider whoami discovery is added.
- [x] Support multiple same-provider accounts across environments without mixing objects: provider account identity now includes `(merchant_id, provider_type, environment, account_id)`.
- [x] Reject unsafe environment/routing combinations: production deployments cannot bootstrap `environment: test` as `mode: primary`.
- [x] Remove `provider_key` and `display_name` from the normal bootstrap example; keep local selector/operator labels out of the manifest.
- [x] Fix stale docs/comments around webhook routing, including any text implying `webhook_host` is the normal private deployment model.
- [x] Add route tests for `/v1/merchants/:merchant/webhooks/:provider` proving unknown merchants do not fall back to a default merchant and known merchants load merchant-scoped credentials before verification.
- [x] Add/adjust integration smoke coverage for Stripe, Mobius/NMI, and CCBill against the actual HTTP server.

## Validation

- `go test ./internal/db ./internal/merchants ./internal/bootstrap ./internal/controlplane ./internal/platform ./internal/http ./internal/http/middleware/ginmw ./internal/intents ./internal/reconcile ./cmd/openrails ./embed ./pkg/embedded/...`
- `go test -tags integration ./internal/http -run TestMerchantWebhookRouteHTTPResolvesMerchantBeforeVerifyingStripe -count=1`
- `go test -tags integration ./internal/bootstrap -run TestReconcileMerchantManifestAppliesMerchantConfiguration -count=1`
- `go test -tags integration ./internal/integrationharness -run TestStandaloneNoDefaultMerchantResolvesRequestScopedMerchant -count=1`
- `go test ./migrations/postgres`
- `SQLC_ADMIN_DATABASE_URL='postgres://admin:admin_password@127.0.0.1:25432/openrails_db?sslmode=disable' task sqlc`
- `git diff --check`

---

# #528: Converge billing admin on the delegated model — retire per-user /v1/admin, rename /merchant-admin → /admin

**Completed:** no

The per-user `/v1/admin` model is outdated. The correct model (same as #527's bootstrap): a merchant has an org + issuer in OpenRails' AuthKit; the issuer (host app) mints JWTs that grant ITS users permissions in OpenRails — a user reading/modifying their own billing (`openrails:self:*`), or an admin-user acting on another user (`openrails:merchant:*`). That is the **delegated** model, served today by `/v1/merchant-admin/*` (+ `/v1/self/*`). The per-user `openrails:admin` surface (`/v1/admin/*`, `HasAdminPermission(org,userID)` live check) assumes local users that the standalone model doesn't have.

**Investigation (2026-06-19):**
- No consumer calls OpenRails' billing `/v1/admin/*`: embedded hosts (cozy-art/doujins/tensorhub) use `/v1/merchant-admin/*` + `/v1/self/*`; their own `/v1/admin/*` paths are app-admin (runpod/fleet/galleries), unrelated. No host imports `embgin.RegisterAdminRoutes`. In standalone it's unreachable (no local users). So the per-user surface is effectively dead.
- It is NOT a pure rename: the two surfaces each have unique endpoints.
  - `/v1/admin` only: `GET /payments`+`/payments/:id`, `/intents`, `POST /solana/recurring/plans`, `/users/:id/product-access` (get/grant/revoke), entitlement-features (RegisterEntitlementFeatureRoutes), `/reconcile/*`.
  - `/v1/merchant-admin` only: `/merchant-configuration` (get/put), `/secrets/*`.
  - Shared (handlers reused): subscriptions, refund, off-channel, users/* profile+entitlements+nmi+ccbill, metrics, repair-alerts, manual-rebill-attempts.

**Plan:**
- Phase 1 (OpenRails, non-breaking): port the `/v1/admin`-only endpoints onto the delegated surface (RegisterMerchantAdminRoutes) with `openrails:merchant:*` perms; delete the per-user RegisterAdminRoutes + `registerAdminRoutesOn/At` + server.go mount + embedhttp mount + `embgin.RegisterAdminRoutes`. (Leave `HasAdminPermission`/`IsLiveAdmin` for now — still used by admin-gated catalog reads; migrate those to the delegated principal as a follow-up.)
- Phase 2 (breaking, cross-repo): rename `MerchantAdminRoutePrefix` `/merchant-admin` → `/admin` and `RegisterMerchantAdminRoutes` → `RegisterAdminRoutes`; update host frontends `/v1/merchant-admin/*` → `/v1/admin/*` (and `/billing/...`) in cozy-art/doujins/tensorhub.

**Sequencing decision (OPEN):** Phase 2 breaks host frontends the moment the path changes. Either (a) hard cut + update all three host repos in lockstep, or (b) mount the delegated surface at BOTH `/admin` and `/merchant-admin` for a deprecation window, migrate hosts, then drop `/merchant-admin`.

**DECIDED:** hard cut (no alias, no old-route support); hosts updated later. Also redesign routes + request/response shapes while consolidating — minimal RESTful surface, only fields actually used, no theoretical/unused routes or fields.

## Proposed minimal RESTful surface (REVIEW)

Single delegated admin surface at `/v1/admin/*` (`openrails:merchant:*` perms). Consolidations vs today in **bold**:

```
Subscriptions
  GET    /admin/subscriptions
  GET    /admin/subscriptions/{id}
  DELETE /admin/subscriptions/{id}                 # cancel (was POST /:id/cancel)

Payments
  GET    /admin/payments[?user_id=]                # **folds /users/{id}/payments into a filter**
  GET    /admin/payments/{id}
  POST   /admin/payments/{id}/refunds              # was POST /:id/refund
  POST   /admin/payments                           # record off-channel (body: user_id + fields)

Users (billing)
  GET    /admin/users/{id}                         # billing profile
  GET    /admin/users/{id}/payment-methods         # **folds /nmi + /ccbill + their /metrics into one shape**
  GET    /admin/users/{id}/entitlements
  POST   /admin/users/{id}/entitlements
  DELETE /admin/users/{id}/entitlements/{id}

Entitlement features / product features
  GET/POST        /admin/features
  GET/POST/DELETE /admin/products/{id}/features

Metrics
  GET /admin/metrics                               # **one response folding summary/revenue/subscriptions/processors/churn**

Configuration & secrets
  GET/PUT          /admin/configuration
  GET              /admin/secrets
  PUT/DELETE       /admin/secrets/{name}
  POST             /admin/secrets/{name}/validate

Operational
  GET /admin/repair-alerts
  GET /admin/manual-rebill-attempts
  GET /admin/provider-intents                      # was /intents

Reconciliation (#107)
  GET/POST /admin/reconcile/runs
  GET      /admin/reconcile/runs/{id}
  GET      /admin/reconcile/findings
  PATCH    /admin/reconcile/findings/{id}          # {status: acknowledged|dismissed}; was POST /ack + /dismiss

Recurring plans (#254)
  POST /admin/recurring-plans                      # was /solana/recurring/plans
```

Shapes: trim each request/response to fields a consumer actually reads (ground in host frontend usage during impl); drop pagination/filter knobs nothing uses, collapse single-use nested objects.

**Usage data (host repos, 2026-06-19):** of the merchant-admin surface, hosts only call `merchant-admin/subscriptions`, `merchant-admin/users/{id}`, `merchant-admin/metrics/summary`. (`/v1/self/*` is heavily used for self-service; `/v1/service/*` for s2s — both out of #528 scope. The `/v1/admin/*` paths in host repos are the hosts' OWN app admin — runpod/galleries/creators/etc. — not OpenRails billing.) Admin UIs are incomplete, so "uncalled" ≠ "droppable" for obvious console operations.

**Resolution (FINAL — user decisions 2026-06-19):**
- DROP (routes + dead handlers): `/provider-intents`, `/reconcile/*` (#107), `/recurring-plans` (#254), entitlement-features + `active_entitlements` (#245).
- Metrics: fold the 5 into one `GET /admin/metrics`.
- **Three separate concepts, kept distinct** (NOT merged): entitlements, product-access/ownership (#250), credit balance. Each is its own section in user reads + its own write actions.
- **No dedicated read endpoints for entitlements/product-access/credits.** Their info is EMBEDDED in user reads (user-detail + user list/search). Writes stay as dedicated actions.

Resulting user surface:
```
GET    /admin/users[?entitlement={slug}&...]   # list/search; each row embeds entitlements/product-access/credit summary (NEW — admin user search)
GET    /admin/users/{id}                        # billing profile: subscriptions + payment-methods + entitlements + product-access + credit-balance sections
POST   /admin/users/{id}/entitlements           # grant
DELETE /admin/users/{id}/entitlements/{slug}    # revoke
POST   /admin/users/{id}/product-access         # grant ownership (#250, separate concept)
DELETE /admin/users/{id}/product-access/{id}    # revoke ownership
# credit-balance read embedded; credit write = existing PermMerchantCreditsWrite op (keep if present)
# DROPPED reads: GET /users/{id}/entitlements, /product-access, /nmi, /ccbill, /nmi/metrics, /ccbill/metrics, active_entitlements
```
- `payment-methods` folds /nmi + /ccbill (+ metrics) into one embedded section / `GET /admin/users/{id}/payment-methods`.
- Self surface (`/v1/self/*`) likewise embeds the caller's active entitlements in their self-detail response.
- Admin user search (`GET /admin/users?entitlement=`) is NEW capability (no current handler) — build minimal (list + filter), or flag as follow-up if the list query is non-trivial.

**Implementation plan — build-safe increments (DECIDED, in progress). Detailed checklists below.**

### Increment 1 (structural) — DONE (commit 53fbbe3c)
- [x] ginroutes/routes.go: `MerchantAdminRoutePrefix`→`AdminRoutePrefix` ("/admin"); `RegisterMerchantAdminRoutes`→`RegisterAdminRoutes`; ported `GET /admin/payments` + `/payments/:id` (`read`).
- [x] Unmount per-user surface: `server.go` (drop `registerAdminRoutesOn` call), `embedhttp.go` (drop `RegisterAdminRoutes` mount), `pkg/embedded/gin/gin.go` (delete `embgin.RegisterAdminRoutes`).
- [x] Rewire callers: routes_self.go, pkg/embedded/gin/self.go. Update tests: ginroutes/self_service_test.go, routes_self_test.go, pkg/embedded/gin/self_test.go, embedded_mux_test.go.
- [x] Validated via HTTP route harness (`go test ./internal/http/... ./pkg/embedded/gin/...`).

### Increment 2 (drops) — DONE (commit a89ca887): dropped features' routes + dead per-user code removed; self handlers + product-access kept; build+vet+route tests green.
- [x] Delete dead per-user `RegisterAdminRoutes` (internal/http/routes/routes.go) + `registerAdminRoutesAt`/`registerAdminRoutesOn` (internal/http/routes_admin.go).
- [ ] **Before deleting each handler, grep its callers** — keep any still used by `/self` or `/service`; only delete admin-only ones.
- [ ] `/provider-intents`: drop route + `httphandlers.GetAdminProviderIntents` (verify not used elsewhere).
- [ ] `/reconcile/*` (#107): drop routes + `AdminReconcile{Run,ListRuns,GetRun,ListFindings,AckFinding,DismissFinding}`. Leave the underlying convergence-engine service/worker/migration 008 (non-admin paths) unless clearly admin-only — DECIDE + note.
- [ ] `/recurring-plans` (#254): drop route + `AdminPublishSolanaPlan` + `publishSolanaPlanRequest` (internal/http/handlers/solana_recurring.go). Leave on-chain plan execution / webhook paths.
- [ ] entitlement-features + `active_entitlements` (#245): delete `RegisterEntitlementFeatureRoutes` (internal/http/routes/entitlement_features.go) + handlers `Create/ListEntitlementFeatures`, `ServiceGetActiveEntitlements`, `List/Attach/DetachProductFeature`. Verify `ServiceGetActiveEntitlements` isn't used by `/self`.
- [ ] Fix orphaned imports; remove/upd tests referencing dropped handlers. Build + vet + route tests green.

### Increment 3 (RESTful route shapes) — DONE (commit d0336be0)
- [x] ginroutes/routes.go: subscription cancel `POST /subscriptions/:id/cancel` → `DELETE /subscriptions/:id` (handler `AdminCancelSubscription` unchanged).
- [ ] refund `POST /payments/:id/refund` → `POST /payments/:id/refunds`.
- [ ] Update ginroutes/self_service_test.go path assertions. Build + tests green.

### Increment 4 (shapes — handler logic; the big one) — NOT a mechanical pass; needs design decisions + DB/ClickHouse integration tests. Findings from increment-2/3 investigation:
> - **product-access is currently DEAD code**: `ginroutes.RegisterProductAccessRoutes` (which mounts /me + /service + /admin product-access) is defined but **mounted nowhere**. So "porting" it = reviving a dormant feature; also decide the /me + /service surfaces, not just /admin. Handlers exist (`GetMyProducts`, `GetAdminUserProductAccess`, `Grant/RevokeAdminProductAccess`, `ServiceGetUserProductAccess`).
> - **metrics fold is a design decision, not a fold**: the 5 handlers (admin_metrics.go) have DIFFERENT default ranges (churn 180d, others 30d), `granularity` params (revenue/subscriptions), and per-section multi-currency disambiguation (`?currency`, else error on multi). A single `GET /admin/metrics` must decide: one `?period`+`?currency` applied uniformly (churn computes its own window internally), sections as `{summary,revenue,subscriptions,processors,churn}`. Confirm the unified shape before building. ClickHouse-backed → needs the analytics stack to integration-test.
> - composite user-detail + payment-methods + self-entitlements compose multiple services → DB-integration-test against dbtest/compose.
- [ ] New perm `PermMerchantProductAccessWrite = "openrails:merchant:product-access:write"` in internal/controlplane/catalog.go (const + catalogEntries + merchantCatalog map + MerchantCatalogNames); owner wildcard `*` already covers it.
- [ ] Port product-access WRITES to ginroutes/routes.go: `POST /admin/users/:user_id/product-access` (`GrantAdminProductAccess`) + `DELETE /admin/users/:user_id/product-access/:id` (`RevokeAdminProductAccess`), gated by the new perm.
- [ ] Composite `GET /admin/users/:user_id` (`GetAdminUserBillingProfile`): response = `{profile, subscriptions[], payment_methods[], entitlements[], product_access[], credit_balance}`. Compose from the subscription/payment-method/entitlement/product-access/credit services. Define a minimal composite response struct.
- [ ] DROP dedicated reads (routes + handlers if now-unused): `GET /users/:id/entitlements`, `/product-access`, `/nmi`, `/nmi/metrics`, `/ccbill`, `/ccbill/metrics`.
- [ ] `payment-methods`: combined `GET /admin/users/:user_id/payment-methods` folding NMI + CCBill (+ their metrics) into one shape (replaces the 4 dropped reads); also the embedded section in user-detail.
- [ ] Fold metrics → one `GET /admin/metrics` returning `{summary, revenue, subscriptions, processors, churn}`; remove the 5 sub-routes (handlers `GetAdminMetrics*` reused internally or composed).
- [ ] Self surface: embed caller's active entitlements in the `/v1/self/*` self-detail response (find the self-detail handler in ginroutes self-service + add entitlements section).
- [ ] Shape trimming: per kept request/response struct, drop fields no consumer reads (ground in host frontend usage); collapse single-use nested objects; drop unused filter/pagination knobs.
- [ ] DB-integration-test the composite + payment-methods + metrics shapes against the dbtest/compose harness.

### Increment 5 (NEW capability — admin user search)
- [ ] `GET /admin/users[?entitlement={slug}&limit=&cursor=]`: new handler `ListAdminUsers` + query (list billing users for the tenant, optional entitlement filter), each row embedding entitlement/product-access/credit summary. Minimal pagination.
- [ ] New sqlc query (internal/db) or hand-written; gate with `read`. DB-integration-test the filter.

### Cross-cutting cleanup
- [~] **CRITICAL — migrate the `tests/` admin INTEGRATION suite to the delegated model.** PROGRESS (commit 4f5e918c): pattern ESTABLISHED + DB-VALIDATED — `tests/admin_user_detail_integration_test.go` mounts `RegisterAdminRoutes` with a delegated host-seam principal (`newHostSeamAdminRouter`); 2 passing tests vs real Postgres prove the delegated `/v1/admin` surface + #528 composite user-detail (entitlements+payment_methods+product_access) work end-to-end + perm gate fails closed (403 w/o billing:read). Also FIXED a real #527 migration prefix collision the harness caught (011→016). REMAINING: migrate the 4 old full-server files to `newHostSeamAdminRouter` + new paths. Original finding: the suite (`tests/admin_metrics_test.go`, `admin_subscription_test.go`, `admin_offchannel_payments_test.go`, `admin_entitlements_source_test.go`) authenticates via `setupTestSuiteWithAdminAuth` → a **per-user admin-role JWT**, and hits the OLD paths (`/v1/admin/metrics/summary`, `/v1/admin/payments/:id/refund`, etc.). #528 made `/v1/admin` the DELEGATED surface (`openrails:merchant:*`), removed the per-user surface, folded metrics → `/v1/admin/metrics`, and renamed refund→`/refunds`, cancel→`DELETE`. **So this entire suite has been BROKEN since increment 1** and was NOT caught (only route-level tests in internal/http + ginroutes were run, not `tests/`). To validate #528 end-to-end: (a) make `setupTestSuiteWithAdminAuth` mint a DELEGATED token with `openrails:merchant:*` perms (or add a delegated variant), (b) update all admin test paths to the new surface (folded metrics, `/refunds`, `DELETE` cancel, composite user-detail shape). THIS is the DB-backed validation the redesign needs. **Run `go test ./tests/ -run TestAdmin...` after each fix.**
- [ ] Migrate admin-gated catalog reads (`internal/auth/policy.IsLiveAdmin` + the runtime `AdminChecker`, used to show inactive catalog rows to admins) off per-user `HasAdminPermission(org,userID)` to the delegated principal's perms. Then the per-user `PermAdmin`/`AdminPermissionChecker`/`AdminPermissionRequiredMW`/`HasAdminPermission` machinery can be deleted (controlplane/authority.go, auth/policy/admin*.go).
- [ ] Docs + comment sweep (deferred from Increment 1): code comments referencing `/v1/merchant-admin` (internal/app/app.go, internal/http/routes_self.go, internal/http/server.go); docs (README.md, docs/api/endpoints.md, docs/principal-boundary-audit.md, docs/vault-secret-ops.md); rename test helper `newMerchantAdminRouter`→`newAdminRouter`.
- [ ] (Later, separate repos) update host frontends (cozy-art/doujins/tensorhub): `/v1/merchant-admin/*`→`/v1/admin/*` paths + new shapes; cozy-art may switch any per-user `/v1/admin` billing calls to the delegated surface.

**Verification each increment:** build + vet + route/unit tests, AND DB-integration tests via the `internal/dbtest` harness (testcontainers, or `OPENRAILS_TEST_DB_URL` against `docker-compose.yaml`); host-level e2e via `~/cozy/e2e` and the host repos' full-stack compose. (Earlier "not DB-testable" note was WRONG — corrected.)

---

# #527: Hard-cut bootstrap redesign — merchant-with-issuer as one entity, first-run-only, no local accounts

**Completed:** no

**Progress 2026-06-19:** AuthKit companions SHIPPED in **v0.38.0** (tagged + pushed): #87 verify-only Service, #89 password seed-once, #88(b) owner-assignable-to-remote_application confirmed + test. OpenRails (uncommitted working tree, builds + bootstrap tests green):
- bumped to authkit v0.38.0; `controlplane.New` runs **verify-only when no signing key is discoverable** (#87).
- **Manifest HARD CUT done**: `bootstrap_manifest.go` is now merchants-only (no `auth`/users/global-roles/orgs); each merchant has an inline `issuer`. Deleted `AuthBootstrapManifest`/`AuthBootstrapOrg`/`AuthKit()`/`HasAuthBootstrap`/`printAuthBootstrapResult`/`ensureManifestOwnerOrg`.
- **issuer-as-owner provisioning done**: `provisionMerchantOrg` routes through authkit `ProvisionOrg` → backing org (slug-derived, 1:1) + issuer registered as `owner`. `bootstrap_apply.go` auth branch removed.
- example manifest + bootstrap parse/validation tests rewritten to the new shape (passing).
- **First-run gate**: migration `011` adds `openrails.bootstrap_state` singleton marker; `applyStartupBootstrap` checks it UNDER the advisory lock BEFORE loading the manifest, so reboots of a provisioned deployment skip entirely (a stale manifest can't brick a restart). Marker written only after a fully successful apply.
- **DB-enforced 1:1**: migration `011` drops the non-unique `idx_merchants_owner_org_id` and adds `uq_merchants_owner_org_id` UNIQUE on non-null (nullable for embedded/merchant-less orgs).
- **CLI tiers**: `--overwrite` (re-assert secrets; default is seed-once — existing secrets left untouched so out-of-band rotations survive) + `--prune` (delete secrets absent from the manifest); `--dry-run` reports the mode; startup is always additive+seed-once.
- config.go retired-`auth.issuers` warning now points at `merchants[].issuer`.
- Paul's superseded #525 auth-shim WIP backed up to `agents/.527-session-backup.patch` (untracked).

- docs/merchant-provisioning.md updated to the merchant-with-issuer model (first-run, CLI tiers, embedded note).
- `/v1/admin` removal RETRACTED (see Decisions) — it's load-bearing for embedded hosts; not a break-glass duplicate.

REMAINING (minor / deliberately not done): `--prune` for provider-accounts/issuers (only secrets pruned — account FKs make it riskier); `merchantForIssuer` loop→direct-lookup simplification (marginal; constraint already guarantees ≤1); config.go `auth.issuer`-optional (low value — kept required). AuthKit #88(a) tx-aware deferred → ProvisionMerchant relies on idempotent re-run (authkit ProvisionOrg + merchant upsert). NOT integration-tested against Postgres this session (build + vet + parse tests only).

Hard cut. Design AuthKit + OpenRails bootstrap as if from scratch; no legacy support, no migration shims. The current bootstrap seeds local users, passwords, global roles, top-level orgs, and per-merchant issuers as separate manifest sections joined by a slug convention, and reconciles all of it on every boot. Almost none of that is needed.

## Mental model

OpenRails owns **authority**; the host application (doujins, cozy-art, …) owns **identity**. Standalone OpenRails has **no local accounts, no login page, no passwords, no global admin**. A host app authenticates its own users with its own roles and decides who may act; OpenRails trusts that app *as the owner of one merchant* and never re-litigates "which human."

Authority is **stored, never self-claimed**: a federated issuer's tokens have platform-authority claims (`global_roles`/`org_roles`/`roles`) stripped on verify. The issuer is granted the authority OpenRails assigned it — the `owner` role (wildcard `*`) on the merchant's backing org — so a compromised/buggy host app can only ever affect its own merchant, never another and never the platform.

## A merchant is ONE entity

Today a merchant is four things joined by `merchant.slug == org.slug` + `owner_org_id`: the `merchants` row, an AuthKit org, an AuthKit `remote_application` + membership, and the encrypted secret store + `merchant_configurations`. The operator hand-writes two manifest sections and keeps them in sync. The implicit slug coupling is load-bearing and undocumented.

Collapse to a single logical entity. The org and remote_application become **derived implementation details the operator never names**. One `ProvisionMerchant` call (and one manifest entry) does, in one transaction / one rollback unit:

1. create the backing org (slug derived from merchant),
2. register the issuer as an `owner`-role member of that org,
3. create the merchant row (`owner_org_id`),
4. store provider secrets (encrypted),
5. write `merchant_configurations` profile.

## Target manifest (hard cut)

```yaml
version: 1
merchants:
  - slug: doujins
    name: Doujins
    issuer:                                   # JWKS/issuer trust, inline — no separate auth.orgs[]
      uri:      https://doujins.example
      jwks_uri: https://doujins.example/.well-known/jwks.json
      # public_keys: [...]                     # alternative: static keys (manual rotation only)
      audiences: [openrails]
      role: owner                              # owner = wildcard * over THIS merchant only
    providers:
      - type: stripe
        account_id: acct_123
        secrets:
          secret_key:     { vault: stripe/doujins/secret_key }
          webhook_secret: { env: DOUJINS_STRIPE_WEBHOOK_SECRET }
    profile:
      display_name:  Doujins
      support_email: support@doujins.example
```

No `auth:` section. No `users`, `password`, `global_roles`, or operator-named `orgs`.

## Deleted (hard cut, no compat)

- `auth.users`, `password` (all modes), `global_roles` from the manifest — and the whole apply path that seeded them. This dissolves the password-clobber and `reset_required`-re-fire footguns by construction.
- Local OpenRails login / local admin accounts.
- Top-level operator-facing `auth.orgs[]`; org is derived and hidden.
- The `merchant.slug == org.slug` coupling — issuer is declared *inside* the merchant.
- Every-boot reconcile — replaced by first-run-only + explicit manual CLI.
- Global admin / platform-superadmin in the self-hosted build (already disabled; remove the seams).
- ~~The `/v1/admin/*` operator surface~~ — RETRACTED (see Decisions): `/v1/admin` is the embedded-host admin surface + a public embedding API; only the standalone issuer→owner path (`/v1/merchant-admin/*`) is the new mechanism. `/v1/admin` is kept.

## Apply semantics

- **Startup: first-run only.** Gate on a `bootstrap_applied` marker row written only after a fully successful, transactional apply — NOT on "does any merchant exist" (that mis-detects a crash mid-apply as done). If the marker is present, skip entirely: reboots never re-run, so a stale/malformed manifest can only fail the *first* boot, never brick a restart of a provisioned deployment.
- **Manual re-apply** via CLI, three tiers:
  - default: **additive** — create missing, never touch existing.
  - `--overwrite`: re-assert manifest values over existing. Footgun for rotated secrets (the manifest is a seed, not ongoing truth) — explicit + manual only, never on startup.
  - `--prune`: disable/remove entities absent from the manifest. Full per-level spec below.

## CLI apply modes (`push-merchant-config`)

Destructive behavior is opt-in and manual; startup is always additive + first-run-only (never `--overwrite`/`--prune`). Modes compose (`--overwrite --prune` = full reconcile-to-manifest). `--dry-run` prints the plan without mutating.

| level | default (additive) | `--overwrite` | `--prune` |
|---|---|---|---|
| merchant | create if missing | update name/tier/region/webhook | absent from manifest → **disable** (status), never hard-delete |
| backing org + issuer | create/register if missing | update jwks_uri/public_keys/audiences/role/enabled | issuer absent → `enabled=false` (revoke trust); org never hard-deleted |
| provider_account | create if missing | update account_id/role/status | account absent → remove account + its secrets |
| secret value | write only if absent (seed-once) | re-encrypt to manifest value (**reverts a rotated secret** — manifest is a seed, not truth) | secret key absent → delete |
| profile | set if unset | overwrite to manifest profile | n/a |

- Hard-deleting a merchant (with its rows / payment history) is NOT what `--prune` does — it disables. A true delete is a separate explicit destructive command with confirmation, never wired to bootstrap.
- `--overwrite` on secrets is the documented footgun: use only when the manifest IS the new source of truth (re-keying from a fresh file); otherwise rotate via the admin API.

## Secrets

- Provider secrets are **encrypted (reversible), never hashed** — OpenRails must present the real key to Stripe. Backend: Vault when a live client is wired, else envelope-encrypted DB (per-merchant DEK), else plain DB (dev only).
- **Seed-once.** After ops rotates a secret via the admin API → OpenRails writes it to Vault/DB; OpenRails is the writer of record. The manifest value is the initial seed; `--overwrite` reverts to it (documented hazard).
- OpenRails' own token-signing key is an **optional** boot mount, never required (DECIDED). In delegated-only mode (all identity arrives as host-app-issued delegated tokens) OpenRails runs **keyless as a pure verifier** and boots with zero mounted secrets. It retains the *capability* to mint tokens — mount a signing key only when a deployment has login-capable users OpenRails itself authenticates — but startup must function fully without one.

## Authority model (AuthKit companion issue required)

- Federated issuer tokens: authority claims stripped; authority = stored org membership + perms.
- Issuer registered as `owner` member of the merchant's org → wildcard `*` → full admin of that one merchant.
- `/v1/merchant-admin/*` and `/v1/service/*` authorized via issuer→org→merchant mapping (no per-user grant). The per-user `HasAdminPermission(org, userID)` path and the `/v1/admin/*` operator surface are REMOVED (DECIDED) — merchant admin is the issuer→owner path only.
- **Identity vs attribution.** Tokens arrive as host-app **delegated tokens** carrying `delegated_sub` (the acting end-user). *Authority* is resolved at the **issuer** level (owner of the merchant's org), not per delegated user — but `delegated_sub` is recorded so audit trails attribute each action to the specific host-app user. The host app decides which of its users it mints delegated tokens for; OpenRails keeps no per-user grants.
- **Issuer key rotation.** With `jwks_uri`, AuthKit re-fetches the host app's keys automatically (including a forced refetch on unknown-kid) — host-app key rotation needs no OpenRails action. Static `public_keys` require a manual `--overwrite` re-apply. Prefer `jwks_uri`; treat hardcoded keys as the exception.

## Org ↔ merchant link must be DB-enforced 1:1 (not name-matching)

Today the link is fragile: `merchants.owner_org_id` is plain `text` (no FK), has only a NON-unique index, the schema comment says "one org owns MANY merchants … never used to resolve a merchant from a token," and the link is established purely by slug coincidence (`ensureManifestOwnerOrg` makes an org named like the merchant). Yet `merchantForIssuer` resolves the merchant from the issuer's org expecting "the org that owns exactly one active merchant" — an assumed 1:1 that nothing enforces.

Fix:
- Dedicated backing org per merchant, created in the SAME transaction as the merchant (part of the unified `ProvisionMerchant`).
- `UNIQUE(owner_org_id)` — **NULLABLE, not NOT NULL** (Postgres allows many NULLs): merchant-less orgs (HuggingFace/GitHub on tensorhub) and embedded-without-AuthKit (`owner_org_id` NULL) must remain valid. Uniqueness bites only on non-null values → DB-enforced 1:1 for real merchants. `owner_org_id` is injective, not bijective.
- Derive the org slug deterministically from the merchant (slug or UUID) so the link can't drift on a typo.
- Resolution chain becomes all-FK, no names: `validated iss → remote_application.OrgID (#77 hard FK) → owner_org_id UNIQUE → merchant`. Simplify `merchantForIssuer` to a direct unique lookup.
- Cross-schema real FK (`openrails.merchants.owner_org_id → profiles.orgs.id`) is optional hardening (same DB in self-hosted; couples migration domains). The robustness win is UNIQUE + atomic creation; otherwise app-level cascade on delete.

Issuer → `owner` role is ALREADY wired: `ProvisionOrg` calls `AddRemoteApplicationMember(org.Slug, ra.ID, issuer.Role)` and each remote_application has a single `OrgID`. The issuer gets the **`owner`** role (DECIDED) — auto-seeded with wildcard `*` → full authority over its merchant, nothing else. No `merchant-admin` role is introduced. AuthKit must ensure `owner` is assignable to a remote_application member (#88).

## Signing key → optional (verify-only by default)

`controlplane.New` (internal/controlplane/service.go) currently always builds a key via `jwtkit.NewAutoKeySource()` (env → /vault/auth → dev-generated; errors if none). Make it optional:
- AuthKit: explicit verify-only Service — `Config.Keys == nil` means NO signer (not auto-discover). Minting methods return a typed `ErrNoSigner`; JWKS serves an empty set.
- OpenRails: presence of a discovered key IS the enablement signal (same pattern as `CaptchaConfig.IsEnabled`). Found → mint-capable. Not found → construct verify-only and log it, instead of failing. Keep dev auto-generation. Gate: mint-capable ⟺ (signing key present AND `auth.issuer` set) — so `auth.issuer` becomes optional (absent forces verify-only). No `minting: on/off` knob.

## Embedded mode (cozy-art, tensorhub) — accommodated

Embedded hosts (cozy-art; tensorhub via the #468 unified SDK) do NOT use the manifest/first-run path. Their bootstrap is `embed.Options` at `embed.New` (embed/embed.go:50):
- `Merchant` binds ONE merchant slug per engine (the host itself); "to serve another tenant, construct another engine/client." One client instance ↔ one merchant. CONFIRMED.
- `PaymentProviders []PaymentProvider` + `MerchantConfiguration` (#524) pass provider secrets + profile programmatically (Go args) — no `/etc/openrails/bootstrap.yaml`, no first-run marker (host controls construction).
- `PGXPool`/`Redis` reuse host infra; runs in-process in the host's DB schema.

tensorhub's shape fits: sole `tensorhub` merchant + backing org; HuggingFace/GitHub register orgs but NEVER merchants — correct, because merchants are host-gated/privileged and orgs are free (most orgs have no merchant; see injective `owner_org_id` above).

Auth in embedded (matches the standalone "host owns identity, OpenRails owns authority" split, different mechanism):
- In-process unified-Client calls are HOST-TRUSTED — the host authenticated before calling; OpenRails does no token check on that path.
- The mounted HTTP surface (admin / self / tenant-admin) uses the host's `Authenticator` (gin-free) / `DelegatedAuthenticator` (#339), returning the explicitly-mapped `{tenant, subject, permissions}` principal. The standalone issuer-as-owner JWKS model is NOT used here → the merchant entity's `issuer` is OPTIONAL (present standalone, absent embedded).
- HARDENING (point 3, DECIDED strict): today a missing authenticator fail-CLOSES silently (self surface unmounts; base handler builds a control-plane default). Change to fail LOUD — a privileged mount requires an EXPLICITLY-resolved Authenticator/DelegatedAuthenticator; the control-plane default is opt-in, never silent. No resolvable boundary → construction error from `embed.New`/`Handler`, never a per-request 500 or silent unmount.

The unified `ProvisionMerchant` therefore serves BOTH paths: standalone (manifest, with issuer) and embedded (Options, issuer omitted), creating merchant + backing org (+ providers + config) atomically; the issuer-as-owner step is the optional standalone-only part.

## Auth boundary: portable + mandatory (point 3)

A host embedding OpenRails must NOT be able to expose a privileged route without an auth boundary — no matter what. Two principles:

1. **Portable, not AuthKit-bound.** The boundary is a framework-neutral INTERFACE — `billingauth.Authenticator` (gin-free) + `billingauth.DelegatedAuthenticator` (#339), returning an explicitly-mapped `{tenant, subject, permissions}` principal. AuthKit is the DEFAULT implementation, not a requirement; a host may plug any scheme (its own JWT verifier, session store, mTLS, …) by implementing the interface. OpenRails never hard-depends on AuthKit for embedded auth.
2. **Mandatory on privileged routes.** OpenRails owns route→required-permission (its permission catalog: `openrails:admin`, `openrails:catalog:write`, …); the host's Authenticator owns identity→principal(+permissions). OpenRails ALWAYS enforces the route's required permission against the authenticated principal — including in embedded mode.

The hardening (DECIDED — strict): a privileged route group (self / tenant-admin / merchant-admin) REQUIRES an EXPLICITLY-resolved Authenticator — host-supplied, OR an explicit opt-in to the control-plane default (no silent implicit fallback). If none is resolved, `embed.New` / `Handler` returns an error AT CONSTRUCTION — it never mounts an unauthenticated privileged route and never defers to a per-request 500 or a silent unmount. The only routes mountable without an Authenticator are an explicit PUBLIC allowlist (checkout init; webhooks, which carry their own signature + IP auth) — public-by-allowlist, never public-by-omission. "Accidentally exposing an unauthenticated privileged route" is therefore structurally impossible, while the scheme stays pluggable. (Changes behavior for hosts that relied on the implicit default — they must now opt in explicitly.)

In-process unified-Client calls remain host-trusted (the host authenticated before calling) and are outside this HTTP-surface rule.

## Implementation plan (files, migration, sequencing)

Files — OpenRails:
- `config/merchants.example.yaml` — rewrite to the single-entity shape (no `auth:`; inline `issuer`, `provider_accounts`, `profile`).
- `internal/bootstrap/bootstrap_manifest.go` — delete `AuthBootstrapManifest`/`AuthBootstrapOrg`/`AuthKit()`/user+global-role+api_key parsing+validation; `BootstrapManifest = {Version, Merchants[]}`; `ManifestMerchant` gains `Issuer`.
- `internal/bootstrap/merchant_manifest.go` — `ReconcileMerchantManifestData` becomes the unified atomic `ProvisionMerchant` (one tx: create-org → register-issuer-as-owner → merchant(owner_org_id) → secrets → profile); replace slug-matching `ensureManifestOwnerOrg` with deterministic per-merchant org + the unique link.
- `cmd/openrails/bootstrap_apply.go` — drop the `HasAuthBootstrap` branch + `printAuthBootstrapResult`; add the first-run marker gate; add `--overwrite` / `--prune` CLI flags.
- `cmd/openrails/main.go` — startup apply fatal only on first run (no marker); logged, not fatal, once provisioned.
- `internal/controlplane/service.go` — optional signer / verify-only (consumes AuthKit #87).
- `internal/controlplane/issuer_registry.go` — simplify `merchantForIssuer` to a direct unique `owner_org_id` lookup.
- `internal/http/server.go` + `internal/http/routes_admin.go` + `internal/auth/policy/admin*.go` — remove the `/v1/admin/*` operator surface and the per-user `HasAdminPermission` break-glass path.
- `internal/http/embedhttp/embedhttp.go` + `embed/embed.go` + `pkg/embedded/embedded.go` — fail-loud auth boundary on privileged mounts (the section above).
- `config/config.go` — `auth.issuer` optional (absent ⇒ verify-only); update the retired-config warning strings (lines ~1342-1349 still tell operators to seed "admin users"/"API keys" under `auth`, which no longer exists) to point at `merchants[].issuer`.

Migration (OpenRails):
- `owner_org_id` → `UNIQUE` but **NULLABLE** (NOT `NOT NULL UNIQUE` — supersedes the earlier phrasing; Postgres allows many NULLs, so merchant-less orgs and embedded-without-AuthKit stay valid; uniqueness bites only on non-null = DB-enforced 1:1 for real merchants). Drop the non-unique `idx_merchants_owner_org_id`. Optional cross-schema FK → `profiles.orgs(id)` (same DB) as hardening.
- New `openrails.bootstrap_state` single-row marker (or equivalent) for first-run detection; written only after a fully successful apply tx.

AuthKit companion issues: **#87** (verify-only Service / optional signer), **#88** (tx-aware provisioning + `owner` role on the RA + invariant lock-in), **#89** (bootstrap-user password seed-once — independent, not blocking).

Sequencing:
1. AuthKit #87 + #88 → tag + bump OpenRails go.mod.
2. OpenRails migration (nullable-unique `owner_org_id` + drop old index + `bootstrap_state` marker).
3. OpenRails bootstrap rewrite (manifest schema, atomic `ProvisionMerchant`, first-run gate, CLI tiers) — serves standalone + embedded.
4. `controlplane` verify-only wiring + `merchantForIssuer` simplification + fail-loud auth boundary.
5. `config.go` + example + docs.

## Tasks

OpenRails:
- [x] New unified manifest schema: `merchants[].issuer` + `merchants[].provider_accounts[]` + `merchants[].profile`; deleted `auth.*` parsing (bootstrap_manifest.go hard cut).
- [ ] `ProvisionMerchant` provisions backing org + (optional) issuer-as-owner + merchant + secrets + profile in one transaction (one rollback unit). Serves BOTH standalone (manifest, with issuer) and embedded (Options, issuer omitted).
- [x] Robust 1:1: dedicated backing org per merchant (slug-derived via `provisionMerchantOrg`/`ProvisionOrg`); migration 011 `UNIQUE(owner_org_id)` NULLABLE + dropped the non-unique index. (`merchantForIssuer` still loops — now guaranteed ≤1 by the constraint; explicit simplification a minor follow-up.)
- [ ] Embedded parity: `embed.Options` (Merchant + PaymentProviders + MerchantConfiguration) drives the same `ProvisionMerchant`; no manifest/first-run-marker on the embedded path.
- [ ] Auth-boundary guarantee (portable + mandatory): privileged route groups require a resolvable `billingauth.Authenticator`/`DelegatedAuthenticator` (framework-neutral interface; AuthKit is one impl, not required). Missing boundary on a privileged mount is a construction error (fail loud), never a per-request 500 / silent unmount. Public routes are an explicit allowlist (checkout, signature-authed webhooks).
- [x] First-run gate: `openrails.bootstrap_state` marker (migration 011); checked under the advisory lock before loading the manifest; written on success; skip otherwise. (Dead `AnyMerchantProvisioned` left in place — harmless, minor cleanup.)
- [~] CLI apply modes: additive default (seed-once); `--overwrite` (re-assert secrets); `--prune` (delete secrets absent from manifest); `--dry-run` reports mode; never destructive on startup. Provider-account/issuer/merchant pruning still TODO (only secrets pruned).
- [x] Secrets seed-once (existing secret left untouched unless `--overwrite`); DB store path. (Vault writer path unchanged this session.)
- [x] Signing key optional: standalone boots keyless as a pure verifier; minting capability retained but never required at boot (no signing key in the required-config path). DONE — `controlplane.New` verify-only on no key (authkit v0.38.0 #87).
- [x] Delete local-user/password/global-role bootstrap code and the `auth.users`/`api_keys` seeding paths superseded here (supersedes the operator-key direction in #525). DONE — auth machinery removed from bootstrap_manifest.go + bootstrap_apply.go.
- [x] ~~Remove `/v1/admin/*`~~ — RETRACTED: investigation showed it is the embedded-host admin surface + public `embgin.RegisterAdminRoutes` API (its handlers are reused by the delegated `/v1/merchant-admin/*`). Removing it breaks embedded hosts; kept. The `HasAdminPermission` checker also backs admin-gated catalog reads, so it stays too.
- [ ] Docs + example manifest rewritten to the new single-entity shape.

AuthKit (companion — filed in authkit/agents/progress.md; shipped in v0.38.0):
- [x] **#87** verify-only Service (optional signer; `ErrMissingSigner` on mint; empty JWKS).
- [~] **#88** owner-assignable-to-remote_application CONFIRMED + test; tx-aware provisioning DEFERRED (OpenRails uses compensating-delete); claim-stripping/`enabled` regression tests + optional helper still TODO.
- [x] **#89** bootstrap-user password seed-once + `reset_required` idempotency.

## Decisions (resolved)

- **Issuer role = `owner`** (machine-owned credential). Auto-seeded wildcard `*` = full authority over its merchant. No `merchant-admin` role introduced.
- **Auth-boundary = strict.** Privileged mount needs an explicitly-resolved Authenticator; no silent control-plane fallback; missing boundary → construction error.
- **`/v1/admin/*` = RETAINED (removal retracted).** Investigation 2026-06-19 found `/v1/admin` is NOT break-glass: it is the substantive admin surface (subscriptions, refunds, entitlements, off-channel payments) AND a public embedding API (`embgin.RegisterAdminRoutes`) that embedded hosts (cozy-art/doujins/tensorhub) serve via `rt.Handler()`, authorized by the host's own AuthKit. `/v1/merchant-admin/*` reuses the same handlers via the delegated issuer→owner path for standalone. Removing `/v1/admin` would break embedded hosts; in standalone it is redundant-but-harmless (unreachable without local users). Kept.
- **CI/script machine credentials = out of scope here.** If ever needed, a per-merchant API key (AuthKit `api_keys`, #86) — additive, non-blocking.
- **Catalog/product seeding = out of scope.** Stays a separate `push-merchant-catalog` concern.

---

# #525: Use API-key terminology in bootstrap authority seeding

**Completed:** yes

OpenRails bootstrap should use `api_keys` for long-lived machine credentials instead of exposing AuthKit's internal/service-token terminology.

AuthKit issue #86 owns the primitive and manifest alias. OpenRails owns the bootstrap YAML/docs/examples that seed OpenRails authority through AuthKit.

Target bootstrap shape:

```yaml
auth:
  orgs:
    - slug: local-stack
      api_keys:
        - name: local-stack-operator
          permissions:
            - openrails:admin
            - openrails:catalog:write
          resources:
            - kind: openrails.merchant
              id: local-stack
          output:
            file: ./.secrets/openrails/local-stack-operator.key
```

Prefix direction:
- AuthKit #86 now exposes the public `APIKeyPrefix` name while preserving the existing `<prefix>_st_<key_id>_<secret>` resolver mechanics.
- OpenRails still uses the existing OpenRails brand prefix/wire format for generated keys until the deeper AuthKit key-format migration is done.
- Prefix is not the merchant/org/user and has no authorization meaning.
- Do not put tenant identifiers or secrets in the prefix.

**Tasks:**
- [x] Update bootstrap parsing/examples from `service_tokens` to `api_keys`.
- [x] Add an OpenRails compatibility shim that accepts `auth.orgs[].api_keys` and converts it to the pinned AuthKit provisioning field, while rejecting mixed `api_keys` + `service_tokens`.
- [x] Update `config/merchants*.yaml`, docs, CLI text, and tests to say API key.
- [x] Keep any accepted legacy `service_tokens` alias undocumented and temporary.
- [x] Ensure generated outputs use `.key` or similarly clear filenames, not `.token`.
- [x] Tests: example bootstrap manifest with `api_keys` parses and produces the same AuthKit provisioning request.

---

# #524: embedded merchant configuration startup parity

**Completed:** yes

DONE 2026-06-19: embedded startup now has parity with standalone merchant-config seeding. `embed.Options.MerchantConfiguration` accepts the public SDK merchant configuration shape, registers the bound merchant first, then seeds `merchant_configurations` through `Runtime.Client().SetMerchantConfiguration`. Public `openrails.MerchantConfigurationInput` now includes `Profile`, and both remote and embedded client adapters write profile plus delegated wasted-spend windows.

Embedded hosts should be able to seed the same merchant-scoped configuration that standalone/SaaS OpenRails seeds through `openrails push-merchant-config`.

Original gap: `merchant_configurations.config.profile` is the correct home for sender/display branding, and the service HTTP route already supported it, but `openrails.Client.SetMerchantConfiguration` only exposed delegated wasted-spend windows and `embed.New` only registered the merchant row.

Lazy plan:
- [x] Add profile to the public `openrails.MerchantConfigurationInput`.
- [x] Wire profile through remote and embedded `SetMerchantConfiguration`.
- [x] Add one `embed.Options.MerchantConfiguration` field so embedded startup can seed all current merchant-configuration fields after the merchant slug is registered.
- [x] Test embedded startup profile seeding and SDK conformance for profile writes.

---

# #519: Rebuild standalone CORS from merchant allowed origins

**Completed:** no

**Reopened 2026-06-19:** the earlier plan removed stale `merchant_cors` / `cors_origins` config, but framed `remote_application.allowed_origins` as delegated-request authorization through AuthKit. That is the wrong security model. `Origin` is spoofable outside browsers, so post-JWT Origin checks are not application security. Application security is JWT signature/issuer/audience/permissions, merchant ownership, and merchant/provider credential isolation.

`remote_application.allowed_origins` should feed browser CORS policy. CORS is browser security, not API authentication.

## Correct model

- Private standalone: preflight has no JWT and no merchant context, so OpenRails should build one process-wide browser CORS allow-list from the union of enabled merchant issuer `allowed_origins`.
- Embedded: the host app may supply/own CORS. OpenRails embedded helpers should not force allow-all on the host; if OpenRails mounts its own CORS layer, it should use the same explicit allow-list model.
- OpenRails SaaS: CORS can be tenant-specific because Host/subdomain resolves the merchant before preflight. That SaaS-specific work belongs in `~/openrails-saas`, not this issue.
- Webhooks and server-to-server routes do not need browser CORS policy as a trust boundary.
- Post-JWT delegated request `OriginAllowedForIssuer` is at most browser defense-in-depth. It must not be documented as an auth/security boundary; once real CORS union enforcement exists, remove it or clearly label it non-authoritative.

## Tasks

- [x] Keep the already-finished removal of `merchant_cors` and `cors_origins` from runtime config and stale examples.
- [ ] Add a standalone CORS origin source backed by AuthKit/OpenRails state: union all enabled merchant remote_application `allowed_origins`.
- [ ] Replace standalone `ginmw.CORS(nil)` allow-all with explicit CORS derived from that union; deny unknown browser origins by default.
- [ ] Keep development/test escape hatches explicit and visibly named; no silent production allow-all.
- [ ] Ensure CORS applies only where browser access matters. Do not use CORS as a webhook/service-token security layer.
- [ ] Update docs/examples: `issuer.allowed_origins` is exact browser origin allow-list input, not webhook URL, mount path, wildcard list, or application authorization.
- [ ] Remove or relabel the delegated request `OriginAllowedForIssuer` path so docs/tests do not imply spoofable `Origin` is real API security.
- [ ] Add HTTP integration tests for allowed and denied preflight/actual browser requests using two merchant issuers, and prove non-browser requests still rely on JWT/permission checks rather than Origin.

---

# #513: admission-and-holds-redis-simplification

**Completed:** yes
**Status:** DONE 2026-06-18. The whole admission path now runs on the Redis `spendgate` + the #512/#514 ledger; the Postgres-with-locks machinery is gone. Shipped: (A) budget windows moved off `budget_window_state`/`budget_inflight_holds` onto the Redis limiter and those tables dropped (migration 007); `internal/modules/holds` deleted, `internal/modules/budgets` gutted to `types.go`. (B) cap taxonomy collapsed to TWO symmetrical spend limits via #517 (`PayerSpendLimits` platform/tier + `InvokerSpendLimits` payer/scope); the dead `spend_policy.go` enforcement + `money_spend_limits` removed (B1, migration 005). (C) balance cache removed after #512 Phase H — admit does one O(1) `GetAdmissionCapacity` read; policy config stays cached (15-min TTL, tier-only; invoker grants read LIVE). (D) one atomic admit/capture/release Lua gate wired into `admitter.go` + `pkg/service`. (E) capture writes the durable #512 ledger off the hot path, no outbox. (F) money_windows credit-window API + table deleted (migration 012). Windows are fixed-only (#517 removed cadence). DROPPED as unneeded (do NOT build later): the `held`-gauge reconciliation sweep and the optional "max concurrent in-flight $" cap (both noted inline). The dead throughput/concurrency surface was removed (fleet fairness = consumer scheduler's job). `go build ./...` + `go vet -tags integration ./...` clean. Remaining tail is outside this repo: tensorhub SDK lockstep (#517 already did the rename + Admit rewire; verify build/tests against local openrails).
**Update 2026-06-18:** balance cache REMOVED after #512 Phase H made balance an O(1) account-counter read. Admit now does a direct `GetAdmissionCapacity` point lookup (balance + held + billing mode + credit limit) and then the same Redis `spendgate` EVAL. Policy config cache stays. There is no `admission.BalanceCache`, no runtime field, and no `CachedBalance` spendgate input anymore.

The north star is decision 8 of #512 (the five-step model: balance-check → Redis hold → capture actual → release; failures record wasted-spend in Redis). This issue generalizes that to the *whole* admission system, including budget/spend caps and rate windows.

## Why (the current hot path is heavy and muddled)

Traced in `internal/modules/admission/admitter.go` `Admit` — one DELEGATED admit does:
1. `money.GetTier` → Postgres read.
2. `policies.GetTierPolicy` → Postgres read (cacheable).
3. FX convert estimate → policy currency (`internal/integrations/fx`).
4. wasted-spend gate → **Redis** (`WastedSpendGuard` on `ratelimit.Limiter`) — already efficient ✅.
5. `buildBudgetScopes` → Postgres reads of `budget_policies`.
6. `budgets.ReserveScopes` → `budgets.Reserve` → **Postgres tx, `budget_window_state` locked `FOR UPDATE`, INSERT `budget_inflight_holds`**.
7. `money.AuthorizeAndHold` → **Postgres tx, `customers` row locked `FOR UPDATE`, derive balance by SUM(`money_blocks`)+`money_windows`** → then the Redis hold.

So one admit = ~3 Postgres reads + **two Postgres write-txns each taking a `FOR UPDATE` row lock**; capture (`CaptureAuthorized`/`spendBalanceThenOwedTx` + `budgets.CaptureByCoords`) adds more locked txns. The two row locks (customer row + window-state row) serialize a payer's concurrent requests — the throughput ceiling.

Two structural problems:
- **Policy and accounting are muddled.** Policy = read-mostly config (what the caps/tiers are: `money_settings`, `money_spend_limits`, `budget_policies`, `tier_policies`, `tier_schedules`). Accounting = hot counters (how much used this window: `budget_window_state`, `budget_inflight_holds`, balance derivation, the Redis hold, the Redis wasted-spend). Accounting belongs in Redis; policy belongs in a versioned in-memory cache. Today budget accounting is the one axis still in Postgres-with-locks while throughput + wasted-spend already ride Redis.
- **The cap taxonomy has ~4 overlapping shapes** for "max $X over window W for scope S": `money_settings.max_spend_per_day/month`, `money_spend_limits` (per invoker), `budget_policies` (hierarchical scopes), `tier_policies` (per-tier windows). There also appear to be TWO generations of spend-cap code: the newer `budget_policies`/`tier_policies` path in `admitter.go` and an older `money_settings`/`money_spend_limits` path in `spend_policy.go` (`CheckSpendAllowed`/`getSpendLimit`).

## Guiding principles

1. **Policy is cached config; accounting is Redis counters.** The hot path does one O(1) Postgres capacity/config read on misses, but no Postgres locks or write-side budget reservations.
2. **One windowed-cap primitive.** Reuse the `ratelimit.Limiter` fixed-window Redis counter that throughput + wasted-spend already use; every cap (throughput / budget-$ / wasted-$) becomes the same counter.
3. **Atomicity without locks.** Admit and capture are each a single Redis `EVAL` (Lua), atomic by Redis's single thread — no `FOR UPDATE`, no races.
4. **Bounded over-admission is acceptable** (decision 8, #512): Redis/cache loss relaxes caps temporarily; the worst case is bounded over-spend, never ledger corruption. The durable truth is the #512 ledger, written off the hot path.
5. **Simplify aggressively, but map before cutting** — confirm each retired mechanism is dead/covered before deleting.

## Metadata

- Category: architecture
- Status: done
- Passes: true

## Work breakdown

### A. Unify windowed caps onto the existing Redis limiter
- [ ] Move money-budget windows off `budget_window_state` + `budget_inflight_holds` (Postgres, `FOR UPDATE` per request) onto `ratelimit.Limiter`. Reserved (holds) + used (captured) become two counter contributions per (scope, window) key, TTL = window. (touch: `internal/modules/budgets/*`, `internal/modules/admission/admitter.go`)
- [ ] Delete `budgets.Reserve/ReserveScopes/Capture/Release/CaptureByCoords` locked-tx machinery once windows are Redis-backed.
- [ ] Drop tables `budget_window_state` and `budget_inflight_holds` (migration); window history that reporting needs comes from the durable spend events (#512 ledger), not these.

### B. Collapse the cap taxonomy to one shape
What the "spend-policy cap" IS (for the record): a TIME-WINDOWED ceiling on cumulative ACTUAL spend — per-customer max spend per UTC calendar DAY + per MONTH (`money_settings.max_spend_per_day/month`), the same per-invoker (`money_spend_limits`), plus an arrears OUTSTANDING-owed ceiling (deny codes `daily_cap`/`monthly_cap`/`invoker_*_cap`/`outstanding_cap`). It is NOT a concurrent-in-flight-hold cap — it bounds spend velocity, orthogonal to the balance gate ("even with $10k, this API key can't burn >$100/day").
VERIFIED DEAD 2026-06-17: the `spend_policy.go` enforcement (`AuthorizeSpend` → `CheckSpendAllowed` → `evaluateSpend`) has ZERO live callers — no HTTP route, nothing. `admitter.Admit` never reads it; day/month/outstanding caps are now expressed via `budget_policies`/`tier_policies` windowed scopes. `money_spend_limits` is still WRITTEN (`SetSpendLimit` API wired) but enforced nowhere on the hot path.
- [ ] One policy shape: a list of `{scope, window, limit}`, scope ∈ {payer, invoker, role, tier}; effective policy = merge of matching scopes, DENY if any window over. Tiers just *name* a policy. Day/month become fixed-calendar windows; outstanding-owed stays a balance-side ceiling.
#### B1. DECIDED 2026-06-17 (Paul): delete the dead spend-limit feature in full. It is a MIX of dead + live-shared, so surgical, not a wholesale file delete:
- [x] Dead-only logic — DELETED 2026-06-17: `pkg/service/spend.go` `AuthorizeSpend`/`AuthorizeSpendRequest`/`AuthorizeSpendResult` + `SetSpendLimit` (service wrapper); `internal/modules/money/spend_policy.go` `CheckSpendAllowed`, `evaluateSpend`, `capInput`, `CapResult`, `dayWindow`, `monthWindow`, `SetSpendLimit`, `getSpendLimit`, `spentInWindow`, `activeHoldsTotal`, `ActiveHeldForCurrency`, and the cap deny-codes `DenyInvokerDailyCap`/`DenyInvokerMonthlyCap`/`DenyDailyCap`/`DenyMonthlyCap`/`DenyOutstandingCap`.
- [x] PRESERVED the live-shared bits 2026-06-17: kept `BillingModePrepaid`/`BillingModeArrears` consts + `SpendDecision` in `spend_policy.go` (no relocation needed — the file still hosts the live account-settings code). `SpendDecision` trimmed to `{Allowed, HardStop, DenyCode}` — the only fields `authorize.go`/`admitter.go` set or read; dropped `Caps`/`AlertCode`/`RetryAfterSeconds`/`NextAllowedAt`.
- [x] `pkg/service/spend.go` `DenyInsufficientBalance` REMOVED 2026-06-17 (had only test callers; live deny is `money.DenyInsufficientBalance`). Updated `authorize_and_hold_integration_test.go` to reference `money.DenyInsufficientBalance`.
- [x] DONE 2026-06-17 (unblocked once the #512/#514 agent finished): added forward migration `005_drop_money_spend_limits.up.sql` (`DROP TABLE IF EXISTS openrails.money_spend_limits`, mirroring their `004_drop_single_entry` pattern; `001` baseline + the `merchant_aware_schema_test.go` table list left intact, same convention they used for `money_blocks`/`money_transactions`).
- [x] DONE 2026-06-17 — removed the `money_spend_limits` queries from `money_accounts.sql` (`Get/Upsert`) + `merchant_lifecycle.sql` (`Count/Purge`) and ran `sqlc generate` (vet DB built from migrations via `scripts/sqlc-vet-db.sh` on the compose Postgres). Dropped the gen methods/structs (`Get/Upsert MoneySpendLimit`, `Count/Purge MerchantRowsMoneySpendLimits`, `gen.OpenrailsMoneySpendLimit`) + the already-orphaned `SumSpentInMoneyWindow`. NOTE: the same regen SYNCED the #512/#514 agent's stale staged gen (`grants.sql.go`/`ledger.sql.go`/`money_ledger.sql.go`/`models.go`) to their current `.sql` — whole-repo `go build` + integration-tag `go vet` green across grants/ledger/money/admission/merchants/service, so the sync is correct (it's what CI's `sqlc generate + diff` enforces).
- [x] Removed the `money_spend_limits` cascade entries from `internal/merchants/delete.go` (the list + count + purge switch arms) 2026-06-17. The `merchant_lifecycle.sql` query removal + regen is folded into the STAGED sqlc pass above.
- [x] Deleted the dead tests 2026-06-17: `internal/modules/money/spend_policy_eval_test.go`, `spend_policy_integration_test.go`; pruned the two dead `AuthorizeSpend` funcs in `pkg/service/spend_integration_test.go` (kept `TestGetCreditAccount_Snapshot`). `money_in_integration_test.go`/`reconcile_integration_test.go` had NO dead refs (the original note was off). Re-homed the shared `strptr` helper (was defined in the deleted `spend_policy_integration_test.go`, used by 6 money tests) to money `main_test.go`.
- [x] No HTTP route exposes `SetSpendLimit`/`AuthorizeSpend` (re-verified — only test callers).
- [x] After removal: `go build ./...` + integration-tag `go vet` clean 2026-06-17. The per-invoker spend cap it provided is covered by `budget_policies` scope=invoker windows (Phase A/B unified policy).
- [~] DROPPED 2026-06-18 (do NOT build): a "max concurrent in-flight $" cap is explicitly NOT openrails' job — fleet fairness/concurrency belongs to the consumer's scheduler (see protection 2; tensorhub WFQ), and the openrails throughput/concurrency surface was already deleted. Concurrency stays implicitly bounded by available balance. Recorded here so it isn't re-built as a "gauge window."

### C. Cache policy config; read balance directly
- [~] Balance cache REMOVED 2026-06-18 per Paul after #512 Phase H: balance/held are O(1) account-counter reads, and admission uses one `GetAdmissionCapacity` point lookup instead of a staleness-tolerant cache.
- [x] Policy cache: load `tier_policies`/`budget_policies`/tier ladders into memory, invalidate on the existing `policy_version`/`schedule_version`/`config_version` bumps. (These version columns already exist — they were built for this.)
- [ ] Tier resolution from cache, not `money.GetTier` Postgres read; tier graduation stays a background recompute (on deposit), it just selects the policy.

### D. One atomic admit, one atomic capture (Lua)
BUILT + INTEGRATION-TESTED 2026-06-17 (8/8 green against real Redis via testcontainers): `internal/modules/admission/spendgate/` (`policy.go` + `gate.go` + `gate_integration_test.go`) — the `{scope,cadence,duration,limit}` model + `EffectiveWindows` merge + 3 `redis.NewScript` scripts (admit/capture/release) + `Gate.Admit/Capture/Release/HeldAmount`. Decisions baked in (Paul, 2026-06-17):
- **Direct O(1) account balance, not Redis `:bal`.** `Admit` takes the caller's ledger snapshot as `AccountBalance` ARGV; the affordability gate is `accountBalance − held − cost ≥ −creditLimit`. Only the shared `held` gauge + per-request `hold:<reqID>` + window counters live in Redis. NO `:bal` key, NO balance-refresh job/cache.
- **Anchored, estimate-based windows.** Cadence = session|fixed (per-user-anchored, #337); the Lua stores the per-(payer,scope,window) anchor in Redis and buckets relative to it (`floor((now−anchor)/dur)` for fixed; session = one key, TTL=dur set at open, never refreshed). Windows count the ESTIMATE; **capture does NOT true the window up to actual** (only the caller-side balance/#512 ledger trues up); **release frees the estimate from the windows**. Tested: prepaid + arrears-credit floor, fixed-window reset across a bucket boundary (fake clock), idempotent replay, multi-scope deny-on-any, capture-keeps-window vs release-frees-window.
- **Wire contract:** clean break + lockstep (Paul) — the eventual admit/capture/release HTTP+SDK shapes may change freely; tensorhub + embedders move in lockstep.
`held`-gauge reconciliation sweep: DROPPED 2026-06-18 (do NOT build) — the Redis `held` gauge can drift on a crash/EVAL-loss, but per decision 8 (#512) bounded over-admission is acceptable and the durable #512 ledger is the truth, so a periodic reconciliation sweep is unnecessary complexity. Cadence also no longer exists (#517 made windows fixed-only), so the loader carries no cadence. DONE since the original draft: Postgres→`spendgate.Policy` loader (tier_policies + budget_policies → scoped windows), direct O(1) admission capacity read, and the admitter/`Service.{Admit,CaptureHold,ReleaseHold}` rewire that deletes `internal/modules/holds` + the `budgets` Postgres machinery + `money.AuthorizeAndHold`'s capacity read.

**WINDOW-ANCHORING GAP — found AND RESOLVED 2026-06-17.** The original draft `spendgate` bucketed windows by GLOBALLY-ALIGNED time, but the live budgets engine (`internal/modules/budgets/budgets_service.go`, #337) uses PER-USER-ANCHORED windows (each opens at the payer's first charge; staggered resets; 5h session / 7d fixed — SHIPPED behavior tensorhub/cozy-art rely on). FIXED in the rebuilt `gate.go`: the Lua stores the per-(payer,scope,window) anchor in Redis and buckets relative to it (`floor((now−anchor)/dur)` for fixed; session = one TTL-bounded key opened at first reserve). The integration test exercises the fixed reset across a bucket boundary. Cadence (session|fixed) maps straight from `BudgetWindowPolicy.Cadence`; the loader (TODO) carries it through. Per Paul, windows are estimate-based (no per-bucket capture true-up), which is what made the faithful port tractable. Until the wire lands, the budgets Postgres path stays the live engine.

SUBSUMES (hard-cut: delete on switch-over, do NOT coexist): `spendgate` replaces THREE current mechanisms that today split one decision — (1) the live Redis hold store `internal/modules/holds` (`Place`/`Release`/`ActiveAmount`, used by `pkg/service/service.go` + `admission.go`); (2) the Postgres money-capacity calc in `money.AuthorizeAndHold`; (3) the `budgets` Postgres window path (Phase A). All three collapse into the one Lua gate. After the cut, `internal/modules/holds` is deleted and `AuthorizeAndHold`'s capacity read is gone — there must NOT be two Redis-hold implementations (`holds.Store` + `spendgate`) running side by side.
- [x] Admit = one `EVAL`: check `account_balance − Σ holds ≥ cost` AND every window `used + reserved + cost ≤ limit`; if all pass, place the request-id hold + bump reserved. One Redis round trip after the O(1) DB capacity/config reads.
- [ ] Capture = one `EVAL`: move reserved→used by ACTUAL cost, drop the hold, and enqueue the durable spend (off hot path). 
- [ ] Fail/release = one `EVAL`: drop hold + reserved, bump the wasted-spend counter for abuse.
- [x] Holds in SHARED Redis keyed by the provider/tensorhub request-id (cross-instance agreement); no balance cache remains.

### E. Durable-spend seam (to #512, off the hot path)
RESOLVED 2026-06-17 (Paul): crash-loss of in-flight captures is acceptable — NO durable outbox. Keep it simple.
- [x] Capture releases the Redis hold and calls the #512 durable ledger writer (`CaptureAuthorized`). No balance cache or background drainer remains; no at-least-once outbox/WAL was added.

### F. money_windows (prepaid bulk reservation) — DELETE (verified zero consumers)
VERIFIED 2026-06-17: `money_windows` is API-exposed (#335: `POST /v1/service/credits/windows`, `/:id/refill`, `/:id/close`, settle) but NO consumer uses it — grep of tensorhub, doujins, cozy-art (incl. cozy-art's embedded in-process calls) found zero `OpenWindow`/`credits/windows` usage. Under the hard cut it's a dormant public API → remove it entirely, don't re-express it.
- [x] DONE 2026-06-18: deleted the credit-window API end to end — routes (`credits.POST /windows|/settle|/:id/refill|/:id/close`), handlers (`service_credit_window.go`), service (`pkg/service/windows.go`), money methods (`internal/modules/money/windows.go`), SDK interface + DTOs (`client.go`/`remote.go` `OpenWindow`/`SettleWindowItems`/`RefillWindow`/`CloseWindow` + `CreditWindow`/`OpenWindowRequest`/`WindowSettle*`), and the embed transcription (`embed/client.go` window block). Also removed the now-dead `HoldExpiryWorker` (durable money-hold sweep — holds are Redis-only per #505): `internal/river/jobs_hold_expiry.go` + its registration/periodic job + `hold_expiry_worker_test.go`. The kept admit-batch bound was renamed `maxWindowBatchItems`→`maxAdmitBatchItems` and re-homed (it had lived in the deleted credit-window handler).
- [x] DONE 2026-06-18: dropped `money_windows` via migration `012_drop_money_windows.up.sql` (frozen-`001`-baseline + new-drop-migration pattern, same as `004`/`005`/`007`; `001` and `merchant_aware_schema_test.go`'s table list keep it — sqlc reads all migrations so the final gen state has no `MoneyWindow` model). Removed its sqlc queries + `SumActiveMoneyHeld`; `deriveBalance`/`GetCreditAccount` now report `HeldBalance: 0` (held = spendgate Redis only).
- [x] DONE 2026-06-18: `go build ./...` + `go vet -tags integration ./...` clean; no orphaned `MoneyWindow`/`money_windows`/credit-window refs (conformance + reconcile + e2e tests updated). NOTE: the e2e `ledgerRows`/`rawBalanceRow` helpers still target the #512-dropped `money_transactions`/`money_blocks` — flagged in `db_helpers.go` as a #512 follow-up, NOT #513.

### G. Tests
- [ ] Admit/capture/release atomicity under concurrency (Redis EVAL, no lost updates, no double-spend).
- [ ] Cap-merge correctness across payer/invoker/role/tier scopes; deny when any window over.
- [~] Balance-cache staleness bound + deposit invalidation — N/A after 2026-06-18 cache deletion; funded payer is visible on the next direct O(1) ledger-account read.
- [ ] Redis-loss behaviour: caps relax, over-admission stays bounded, ledger stays correct.
- [ ] Migration: `budget_window_state`/`budget_inflight_holds` removed; in-flight reservations drained or expired cleanly.

## What stays (don't lose) — and a VERIFIED inventory of the three protections (2026-06-17)

The three protections spend limits exist for, mapped to their LIVE system (all SEPARATE from the dead `spend_policy.go`):
1. **Abuse (request velocity / low-trust)** — LIVE: edge rate-limit middleware mounted globally (`internal/http/server.go:473` `ginmw.RateLimitWithChallengeStore`, per-IP/route + captcha + attack-mode), card-abuse guard (`abuse/card_abuse.go`), velocity guard (`abuse/velocity.go`), wasted-spend guard (`abuse/wasted_spend.go`, in `admitter.Admit`), and the low-default tier for new accounts (#300).
2. **Fairness (rich payer can't crowd out others)** — RESOLVED 2026-06-17, NOT openrails' job: fleet fairness is owned by the CONSUMER'S SCHEDULER, not openrails. Verified in tensorhub (`~/cozy/tensorhub/internal/orchestrator/grpc/fairness.go`): a work-conserving weighted-fair-queue dispatches the shared GPU fleet — L1 per-payer / L2 per-user nesting, a per-payer in-flight **$** soft cap that scales by platform tier, weighted by the OpenRails hold ESTIMATE (#486), never idles a slot, never rejects. OpenRails owns the HARD money decision (affordability/hold) + $-budget windows + abuse; tensorhub owns scheduling/reordering and the live in-flight tally. This is the right split — openrails can't fairly schedule a fleet it doesn't see. CONSEQUENCE: openrails' throughput/concurrency surface (`ThroughputPolicy` written-not-read, `AcquireConcurrency` zero callers, the `ratelimit/concurrency.go` primitive) is dead-on-PURPOSE — tensorhub explicitly stopped sending throughput units (`invoke_admission.go:66`). Do NOT wire it; REMOVE it (same treatment as `spend_policy.go`), and document the contract: openrails = money truth + $-budget + abuse; the consumer's scheduler owns fleet fairness. The edge per-IP limiter stays as the DoS backstop for schedulerless consumers (doujins/cozy-art/direct API).
3. **Delegated spend-control (cap what another invoker spends + how often)** — LIVE: `budget_policies` scope=invoker/role/invoker_tier windowed `$` caps, built in `buildBudgetScopes` and reserved in `admitter.Admit`. This is strictly MORE general than the dead per-invoker day/month caps. (Subject-wide windowed cap = the payer's own velocity limit, distinct from balance: exists as config `ScopeSubject`/tier BudgetWindows but `buildBudgetScopes` skips subject — verify whether it's enforced anywhere on admit, or fold it into the one shape.)

Keep through the simplification:
- Hierarchical delegated-spend SEMANTICS (an invoker may spend the payer's money up to a cap) — keep; only the accounting + policy-shape change.
- FX conversion for cross-currency policy comparison (`internal/integrations/fx`) — pure function on the estimate.
- Tier graduation from cumulative paid (`tier_schedules`) — background; selects the policy.
- Wasted-spend abuse gate — already Redis; it becomes one consumer of the unified primitive.
- [x] REMOVE the dead fairness/throughput surface from openrails (NOT wire it — see protection 2): DONE 2026-06-17. Deleted `ratelimit/concurrency.go` (`AcquireConcurrency`/`ReleaseConcurrency`/`ConcurrencyCount`, zero callers) + `ratelimit/units.go` (#472 weighted queue pools `AcquireUnits`/`AcquireQueue`/`ReleaseQueueByRequest`, zero callers) + both integration tests; renamed `models.ThroughputPolicy` → `models.TierMoneyPolicy` (the misnomer carried no throughput axis). `AdmitRequest` had no throughput-unit field to drop. Fleet fairness is the consumer scheduler's job (tensorhub WFQ). `valuewindow.go` (wasted-spend `AddWindowValue`/`WindowValue`/`ClaimOnce`) + `limiter.go` `Check` (edge rate-limit) are LIVE and kept.

## Non-goals

- Not changing WHAT the caps mean or the delegated-spend model — only where state lives and how it's checked.
- Not building the durable ledger here (that's #512); this issue only enqueues to it.

## HARD CUT — no legacy, no backward compatibility (decided 2026-06-17, Paul)

Move to the new system completely; do NOT leave old code or compat shims around.
- NO feature flag toggling old-vs-new admission; NO dual-write / shadow mode; NO parallel "Postgres-budget AND Redis-budget" running side by side. One path, the new one.
- DELETE, don't deprecate: `budget_window_state`, `budget_inflight_holds`, the `budgets.Reserve/ReserveScopes/Capture/Release` locked-tx machinery, `spend_policy.go` enforcement + `money_spend_limits`, and the dead throughput/concurrency surface (`ThroughputPolicy`, `ratelimit/concurrency.go`) all go.
- NO on-the-wire backward-compat window on the admit/capture contract: if the API shape changes, change it outright. Consumers (tensorhub, doujins, cozy-art) move in LOCKSTEP via a coordinated openrails version bump — same model as the #403/#404 hard cut that already deleted the legacy broker.
- Exit gate: after the cut, `grep` finds no reference to the deleted tables/types/functions anywhere (no orphaned readers, no commented-out fallbacks); `go build ./... && go vet ./...` clean.

## Lockstep cutover (consumers) — verified 2026-06-17

Hard cut = every consumer moves to the new contract in ONE coordinated openrails version bump; no compat window, and APIs with zero consumers are deleted outright. Verified coupling + surface each consumer touches:

- **tensorhub** — standalone HTTP/SDK client (openrails SDK pinned, currently v0.36.0). Calls the USAGE surface: `AdmitHold` (admit), capture-on-job-result, `ReportWastedSpend`, `/credits/deposit`, `credit-types`, `/billing/v1/self/*`. **Most affected** — the admit/capture/wasted contract changes under #513. Lockstep: bump SDK, update the admit/capture/release/wasted call sites in `internal/orchestrator/http/invoke_admission.go` + `internal/billing/openrailsclient`.
- **doujins** — EMBEDS openrails in-process (`go.mod` `open-rails/openrails v0.39.0`; mounts routes in `internal/server/server.go` + `internal/di/builder.go`). Uses the SUBSCRIPTION surface, not admit/holds/windows/spend-limits (grep clean). Lockstep: bump the module dep + recompile — a compile break is the signal if it touches any removed symbol; subscription API is unchanged by #512/#513.
- **cozy-art** — EMBEDS openrails in-process; frontend hits `/billing/v1/me/subscriptions|payments|payment-methods`, `/checkout`, `/products`, `/stripe/portal`. Subscription-only; does NOT touch admit/holds/credits. Lockstep: bump the module dep + recompile; frontend endpoints unchanged.
- **Zero-consumer APIs being deleted — break nobody:** the credit-window API (#335 / `money_windows`) and the spend-limit API (`SetSpendLimit`/`money_spend_limits`) have NO consumer in any of the three repos → delete with no consumer migration.
- **#512 (ledger) is internal** — the consumer-facing deposit/charge/balance/subscription wire shapes (amounts already minor units) should NOT change; verify the API responses are byte-stable so #512 needs no consumer change beyond the embedded-dep bump.

## Efficiency (before → after)

- Admit: ~3 PG reads + 2 PG txns w/ `FOR UPDATE`  →  1 Redis `EVAL` + in-memory policy/balance.
- Capture: 1–2 locked PG txns  →  1 Redis `EVAL` + async durable enqueue.
- Per-payer concurrency: serialized by 2 row locks  →  lock-free.
- Cap surface: 4 policy shapes + 2 PG accounting tables  →  1 policy shape + 1 Redis counter primitive.

## References

- Current code: `internal/modules/admission/admitter.go`, `internal/modules/budgets/{budgets_service,scopes}.go`, `internal/modules/abuse/wasted_spend.go` (the Redis pattern to copy), `internal/modules/money/{authorize,unified_spend,windows,spend_policy}.go`, `pkg/service/{admission,spend,windows}.go`.
- Tables: `budget_window_state`, `budget_inflight_holds`, `budget_policies`, `tier_policies`, `tier_schedules`, `money_spend_limits`, `money_settings`, `money_windows`.
- Relationship: realizes decision 8 of #512 across the whole admission path; #512 Phase C defers here for the admission/holds detail.

---

# #330: nmi-immediate-subscription-checkout-stuck-pending

**Completed:** no
**Status:** IN_PROGRESS 2026-06-08: immediate NMI checkout activation patch is implemented; package tests and focused mock-provider regression pass; configured-account Mobius recurring test has been added and compiles/skips locally because `MOBIUS_PRODUCTION_KEY` is not set. Remaining work: run real Mobius credential test and repair/replay Paul2 after fixed code is deployed.

Fix the NMI/Mobius subscription checkout path where an immediately approved recurring checkout is persisted as pending, leaving host apps such as Doujins stuck on the pending-subscription screen even though NMI accepted the transaction.

## Metadata

- Category: bug
- Status: in_progress
- Passes: false

## Live findings from Doujins/Paul2 on 2026-06-08

- OpenRails received the checkout request and `POST /v1/self/checkout` returned HTTP 200.
- NMI/Mobius accepted the card vault and transaction: payment vault `314825442`, provider transaction `12162933364`, provider subscription `12162933429`.
- Local checkout session `019ea96b-221b-7274-a341-8cdc85cb72d6` was marked `succeeded` with processor `mobius` and amount `2300 usd`.
- Local subscription `019ea96b-223b-75b1-926b-35956d10eba5` stayed `pending`, with no current period start/end timestamps.
- Local payments only had a pending attempt row keyed as `nmi_sub_attempt:sub_cb204b034d2c3ec46be93b0470ff44df`; there was no completed payment row keyed by the real NMI transaction id.
- No billing entitlement rows were created for the user, so Doujins correctly kept showing the pending-subscription state.

## Root cause

`processNMISubscription` called `AddRecurringSubscription` successfully, but `completeNMISubscriptionRegistration` intentionally created a local pending subscription and returned a pending checkout response. The initial NMI response was not used to synchronously activate an immediate subscription, set the current billing period, create a completed payment, or grant entitlements. Webhooks should not be required for the initial happy path when NMI immediately approves the transaction; delayed/future starts can remain pending.

## Desired behavior

For immediate NMI/Mobius subscription approvals, OpenRails should finish the local checkout atomically from the direct provider response: mark the subscription active, set current period timestamps, record the completed payment against the real provider transaction id, grant entitlements, and return a succeeded checkout response. Delayed-start subscriptions and genuinely asynchronous provider states should remain pending and rely on follow-up provider events/reconciliation.

**Tasks:**
- [x] Capture live-stack evidence for the Paul2 failure: NMI accepted the transaction, checkout succeeded, subscription stayed pending, payment stayed as a pending attempt, and no entitlement was granted.
- [x] Identify root cause in the NMI checkout finalization path.
- [x] Patch immediate NMI subscription finalization to activate the subscription, set period dates, create the completed payment, and grant entitlements without waiting for a webhook.
- [x] Preserve pending behavior for delayed/future-start NMI subscriptions.
- [x] Fix stale integration-test helpers that still query billing tables by `user_id` instead of `tenant_subject_id`.
- [x] Add/validate focused integration coverage proving an immediate NMI subscription checkout grants the premium entitlement synchronously.
- [x] Add an actual NMI test-account integration path so the live provider contract is exercised, guarded by Mobius/NMI test credentials.
- [ ] Run the actual Mobius/NMI configured-account recurring-subscription integration with `MOBIUS_PRODUCTION_KEY` set.
- [x] Run focused OpenRails checkout/module tests and NMI regression tests.
- [ ] After deployment/restart, repair or replay the affected live pending subscription row for Paul2 if it still exists.

---

# #328: robinhood-coinbase-usdc-funding-sessions

**Completed:** no
**Status:** PARTIAL 2026-06-08: Implemented Solana-only USDC funding session APIs, persistence, config, Coinbase hosted-session adapter with CDP JWT auth, Coinbase Hook0-signed Onramp webhook/status ingestion, Robinhood launch-template handoff, provider eligibility gates, self-service routes, idempotency, structured insufficient-USDC funding context on checkout errors, backend Solana USDC balance verification, focused tests, and DB-backed self-service API tests for create/get, tenant/user isolation, idempotency, and unsupported provider/network rejection. Retained for future provider integration work; current Doujins UX uses manual Robinhood/Coinbase links plus connected-wallet balance checks instead of OpenRails provider sessions. Remaining: real Robinhood partner adapter/status docs and access.

Plan and implement OpenRails-owned USDC funding sessions for host apps that need users to buy or transfer USDC into their own live Solana wallet before completing a Solana wallet checkout.

## Metadata

- Category: feature
- Status: partial
- Passes: false

## Goal

- OpenRails should expose a provider-backed funding-session API for Robinhood and Coinbase only. Host apps such as Doujins can ask OpenRails for a funding URL, send the user to the provider in a new tab or popup, then resume checkout after OpenRails and/or the host app verifies that the user's live Solana wallet has enough USDC.

## Product Behavior

- The user already has or creates a Robinhood/Coinbase account on the provider site.
- OpenRails does not custody funds and does not collect provider KYC; the provider handles account login, payment method, buy/transfer, KYC, and compliance.
- Provider redirect/return means the provider flow ended; it is not proof that the wallet is funded.
- Completion must be based on provider status/webhooks when available plus Solana wallet-balance verification.
- Only offer a provider when it can fund USDC on Solana. Coinbase/Base and all EVM chains are out of scope.

## Scope

- Implement Robinhood and Coinbase integration surfaces only for Solana USDC.
- Do not implement Ramp, Transak, MoonPay, PayPal, Venmo, Base, Ethereum, Polygon, Arbitrum, Optimism, or bridge paths for this issue.
- Keep provider abstraction narrow but extensible enough that more providers could be added later without changing the host-app contract.

**Tasks:**
- DESIGN:
- [x] Define the OpenRails funding-session contract for browser self-service callers: provider preference, wallet address, asset, network, requested amount, checkout_session_id, return_url, and idempotency key.
- [x] Define provider statuses and normalize them into OpenRails statuses such as created, opened, pending_provider, pending_settlement, funded, failed, expired, and cancelled.
- [x] Define Solana-only compatibility rules for USDC funding.
- [x] Decide whether funding amount comes from the checkout-session shortfall, an explicit requested amount, or both with server-side validation. Implemented explicit requested amount with optional checkout_session_id context.
- [x] Decide how provider ranking is configured per tenant: Robinhood preferred, Coinbase fallback. Implemented default provider order with Robinhood first and Coinbase second.
-
- DATA MODEL:
- [x] Add a funding/onramp session table with tenant_id, user_id, checkout_session_id, provider, wallet_address, asset, network, requested_amount, provider_session_id, provider_url, status, return_url, idempotency key, timestamps, and provider metadata.
- [x] Add indexes for tenant/user lookup, checkout_session_id lookup, provider_session_id lookup, and idempotency.
- [x] Store provider secrets/config in OpenRails config, never in host apps.
-
- API:
- [x] Add `POST /v1/self/usdc-funding-sessions` to create a Robinhood/Coinbase funding session for the authenticated browser user.
- [x] Add `GET /v1/self/usdc-funding-sessions/:id` to return normalized funding status and provider URL/status details safe for frontend polling.
- [x] Add `GET /v1/self/usdc-funding-options` to list eligible Robinhood/Coinbase options for wallet, network, asset, amount, and optional checkout_session_id.
- [x] Add provider webhook/status callback endpoints where Coinbase supports them. Implemented signed Coinbase Onramp webhook ingestion on the existing provider webhook route; Robinhood remains blocked on partner docs/access.
- [x] Enforce self-service auth, tenant boundaries, and idempotency on funding-session create/read routes.
-
- PROVIDERS:
- [x] Implement a Coinbase provider adapter that creates a hosted onramp URL/session with destination wallet, network, asset, amount, return URL, and partner/user reference, including short-lived CDP JWT bearer generation from Coinbase secret API keys.
- [x] Implement Coinbase status/webhook handling and map provider lifecycle into OpenRails funding-session status. Coinbase success maps to pending_settlement; only live Solana wallet-balance verification can mark funded.
- [ ] Implement a Robinhood provider adapter after partner docs/access are available, supporting external handoff into Robinhood Connect and funding into the user's live wallet.
- [ ] Implement Robinhood status/webhook handling if exposed by partner API; otherwise rely on return handling plus on-chain wallet-balance verification.
- [x] Add provider availability checks so unsupported network/asset combinations are hidden rather than offered.
-
- WALLET VERIFICATION:
- [x] Reuse existing Solana USDC balance-checking code where possible to verify the funded wallet before marking a session funded for Solana checkout.
- [x] Do not add Base/EVM balance verification for this issue; Solana is the only supported chain.
- [x] Ensure returning from a provider only triggers polling/checking; it must not mark the session funded by itself.
-
- CHECKOUT INTEGRATION:
- [x] Allow a funding session to reference the checkout session that produced an insufficient-USDC state.
- [x] Ensure insufficient-USDC API errors expose enough structured amount/network/wallet context for host apps to create a funding session. Added `error.metadata.usdc_funding` with Solana network, USDC asset, wallet, decimal amount/balance/shortfall, and base-unit values.
- [x] Keep final subscription/payment creation in the existing checkout confirmation path after the wallet is funded.
-
- VERIFY:
- [x] Add unit tests for provider eligibility and network compatibility gates.
- [x] Add API tests for create/get funding session, tenant isolation, idempotency, and unsupported-provider/network rejection.
- [x] Add provider adapter tests with mocked Coinbase responses.
- [x] Add wallet-balance verification tests proving redirect alone is insufficient through status semantics and frontend polling contract.
- [x] Document the host-app integration contract for Doujins in config.example.yaml and the tracker issue.

---


# #108: admin-user-search

**Completed:** no

Sophisticated user search for admin dashboard

## Metadata

- Category: feature
- Status: not_started
- Passes: false

**Tasks:**
- STEPS:
- [ ] Design search API:
- [ ]   - GET /v1/admin/users?q=...&filters... - search users with billing data
- [ ] Search fields:
- [ ]   - email (partial match from subscriptions/payments)
- [ ]   - user_id (exact match)
- [ ]   - processor_subscription_id (exact match for support lookups)
- [ ]   - transaction_id (exact match for payment support)
- [ ] Filters:
- [ ]   - has_subscription=true/false
- [ ]   - subscription_status=active/cancelled/past_due/etc
- [ ]   - processor=nmi/ccbill/solana
- [ ]   - has_entitlement=premium/etc
- [ ]   - created_after, created_before (date range)
- [ ] Sorting:
- [ ]   - sort_by=newest/oldest/last_payment/subscription_start
- [ ] Pagination:
- [ ]   - page, page_size, cursor-based pagination for large results
- [ ] Performance:
- [ ]   - Add database indexes for search fields
- [ ]   - Consider full-text search for email
- [ ]   - Rate limit search queries
- NOTE: Billing service searches its own data. Full user profile search (username, etc) lives in main host-app API.

---

# #111: admin-rate-limiting

**Completed:** no

Add rate limiting to admin endpoints to limit blast radius of compromised JWT

## Metadata

- Category: security
- Status: not_started
- Passes: false

**Tasks:**
- STEPS:
- [ ] PROBLEM: If admin JWT is leaked, attacker could cancel/refund thousands of users before detection
- [ ] Implement per-admin-user rate limiting using Redis (keyed by admin user ID from JWT)
- [ ] Define rate limits for destructive operations:
-     - Cancellations: 5/minute, 10/hour, 50/day
-     - Refunds: 5/minute, 10/hour, 50/day
-     - Entitlement revocations: 5/minute, 10/hour, 50/day
- [ ] Define rate limits for bulk/expensive operations:
-     - Extend operations: 3/minute
-     - Off-channel payments: 10/minute
-     - Admin grants: 10/minute
- [ ] On rate limit exceeded: lock out admin for extended period (e.g., 1 hour) and alert
- [ ] Add alerting/notification when rate limits are approached (e.g., 80% threshold)
- [ ] Create admin rate limit middleware that wraps destructive endpoints
- [ ] Allow super-admin or manual override to unlock rate-limited admin if legitimate
- [ ] Log all rate limit events for security audit
- BENEFIT: Limits blast radius - attacker can only affect ~5-10 users before getting locked out

---

# #118: admin-dashboard-metrics-overhaul

**Completed:** no

Overhaul admin dashboard metrics to provide useful business intelligence

## Metadata

- Category: feature
- Status: not_started
- Passes: false

## Details

- api_design: {"dashboard_summary":{"endpoint":"GET /v1/admin/metrics/summary","returns":"Key KPIs for dashboard cards (MRR, active subs, churn rate, etc.)"},"revenue_over_time":{"endpoint":"GET /v1/admin/metrics/revenue?start=DATE&end=DATE&granularity=day|week|month","returns":"Time series of revenue data"},"subscriptions_over_time":{"endpoint":"GET /v1/admin/metrics/subscriptions?start=DATE&end=DATE&granularity=day|week|month","returns":"Time series of subscription counts"},"churn_analysis":{"endpoint":"GET /v1/admin/metrics/churn?start=DATE&end=DATE","returns":"Churn breakdown by reason, cohort retention"},"processor_breakdown":{"endpoint":"GET /v1/admin/metrics/processors","returns":"Revenue and counts by processor"}}
- implementation_notes: ["MRR calculation: sum(price.amount / (price.billing_cycle_days / 30)) for active subs","Need to handle different billing cycles (monthly, quarterly, annual)","Solana one-time payments are not subscriptions - track separately","Consider caching expensive aggregations (refresh every 5 min)","Use PostgreSQL window functions for period-over-period comparisons","May want ClickHouse for historical analytics at scale"]
- problems: ["Limited metrics - only subscription counts, no revenue or business KPIs","Confusing naming - 'without_auto_renew' actually means 'cancelled but still in period'","No revenue metrics - MRR, ARR, total revenue, average revenue per user","No churn metrics - churn rate, retention rate, LTV","No Solana support - processor metrics exclude Solana one-time payments","No period comparisons - can't compare this week/month vs previous","No cohort analysis - can't track retention by signup month","No conversion funnel - signups to first payment to recurring","Tests are minimal - just verify response structure, not actual data accuracy"]
- proposed_metrics: {"revenue_metrics":["MRR (Monthly Recurring Revenue) - sum of active subscription monthly values","ARR (Annual Recurring Revenue) - MRR * 12","Total revenue this period - sum of all payments in date range","Revenue by processor - breakdown by CCBill, NMI, Solana","ARPU (Average Revenue Per User) - total revenue / active users"],"subscription_metrics":["Total active subscriptions","New subscriptions this period","Cancelled subscriptions this period (with breakdown by reason)","Net subscriber change (new - cancelled)","Subscriptions by status (active, past_due, cancelled)","Subscriptions by product/tier"],"churn_metrics":["Monthly churn rate - cancelled / active at start of month","Voluntary churn - user/merchant initiated","Involuntary churn - payment failures","Retention rate by cohort - % of month-N signups still active"],"payment_metrics":["Successful payments this period","Failed payments this period","Payment success rate","Refunds issued","Chargebacks received","Average payment amount"],"one_time_purchases":["Solana payments count and value","One-time purchase revenue (non-subscription)"]}

**Tasks:**
- STEPS:
- [ ] Define exact metrics needed for admin dashboard UI
- [ ] Design API response formats for each endpoint
- [ ] Implement MRR/ARR calculation logic
- [ ] Implement churn rate calculation
- [ ] Add revenue aggregation queries
- [ ] Add one-time purchase tracking (Solana)
- [ ] Add period comparison support (vs last period)
- [ ] Add caching layer for expensive queries
- [ ] Update existing metrics endpoints or create new ones
- [ ] Write comprehensive tests with seeded data
- [ ] Document all metrics definitions

---

# #126: test-architecture-improvements

**Completed:** no

Tests should use real structs and functions from production code, not invent test-specific abstractions

## Metadata

- Category: testing
- Status: not_started
- Passes: false

## Details

- current_problems: ["SubscriptionOptions helper struct uses different field names than models.Subscription (PeriodStart vs CurrentPeriodStartsAt)","Tests can pass while using wrong field names because helpers translate between them","When a model field is renamed/removed, tests may not break because helper abstracts it away","Developers get confused about which struct to use and what fields are available"]
- example_bad: {"code":"suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{PeriodStart: now})","problem":"SubscriptionOptions.PeriodStart doesn't exist on models.Subscription"}
- example_good: {"code":"sub := &models.Subscription{CurrentPeriodStartsAt: now, ...}; suite.CreateTestSubscription(sub)","benefit":"Uses real model, compiler catches field name errors"}
- philosophy: {"principle":"Tests should verify actual production code behavior, not test-specific wrappers","goal":"When production code changes, tests should break if they're testing the affected behavior","anti_pattern":"Test helper structs that diverge from production models hide API changes and create maintenance burden"}
- recommendations: ["Use models.Subscription directly in tests instead of SubscriptionOptions","Use api.ListResponse[T] for parsing API responses instead of map[string]interface{}","Import and use the actual request/response structs from handlers","If a helper is needed, it should take the real model struct as input, not a parallel struct","Test assertions should use strongly-typed response parsing"]

**Tasks:**
- REFACTORING:
- [ ] Audit all test helper structs (SubscriptionOptions, PaymentMethodOptions, etc.)
- [ ] Replace helper structs with direct model usage where possible
- [ ] Update CreateTestSubscriptionWithOptions to accept *models.Subscription
- [ ] Use json.Unmarshal with actual API response types instead of map[string]interface{}
- [ ] Add linter rule or CI check to discourage map[string]interface{} in tests

---

# #158: ccbill-dunning-grace-entitlements

**Completed:** no

Adjust CCBill subscription entitlement logic to model paid term + retries (dunning) + end-of-term expiration. Use CCBill webhook fields like nextRetryDate/nextRenewalDate to drive subscription status and finite entitlement windows, and cap any grace extensions to avoid unlimited free access.

**Tasks:**
- SPEC:
- [ ] Verify CCBill webhook sequence in sandbox: cancel mid-term -> Expiration at end-of-term (and timing)
- [ ] Decide grace policy during retries: disabled vs enabled; cap strategy (extend-to-nextRetryDate vs fixed extra days)
- [ ] Decide policy for CCBill `Cancellation.source=failedRB`: treat as terminal end (no more retries) vs still paid-through end-of-term
-
- DATA MODEL:
- [x] Ensure we can store current_period_ends_at (from nextRenewalDate) for CCBill subscriptions
- [x] Store next_retry_at (from nextRetryDate) and optional grace_ends_at (policy derived)
- [x] Decide whether to reuse existing `subscriptions.next_retry_at` fields for CCBill (recommended) vs add CCBill-specific columns
-
- WEBHOOK -> STATE MACHINE:
- [x] NewSaleSuccess: set paid-term end; grant entitlements for [now, paid_term_end)
- [x] RenewalSuccess: extend paid-term end; extend entitlement windows
- [x] RenewalFailure: set past_due/dunning; persist next_retry_at from webhook; optionally extend entitlements within grace cap
- [x] Cancellation: mark cancelled but keep access until paid_term_end (do NOT revoke immediately)
- [x] Expiration: mark ended/expired and end/revoke entitlements
-
- CODE CHANGES (expected touch points):
- [x] `internal/services/webhook_ccbill.go`: parse and apply `nextRenewalDate` on `NewSaleSuccess` and `RenewalSuccess` (don’t rely solely on `price.billing_cycle_days`)
- [x] `internal/services/webhook_ccbill.go`: on `RenewalFailure`, parse `nextRetryDate` and update `subscriptions.status=past_due` + `subscriptions.next_retry_at` (avoid calling `SubscriptionLifecycleService.FailMembership`, which schedules NMI-style retries)
- [x] `internal/services/webhook_ccbill.go`: on `Cancellation`, call `CancelMembership` with `RevokeAccess=false` (today it passes true)
- [x] `internal/services/lifecycle_service.go`: update `CreateMembership` entitlement grant behavior for CCBill to create finite windows ending at `current_period_ends_at` (instead of indefinite `end_at=NULL`)
- [x] `internal/services/lifecycle_service.go`: update `RenewMembership` for CCBill to extend entitlements to match the new `current_period_ends_at` (today renewal doesn’t extend entitlements at all unless a downgrade changes entitlement set)
- [x] `internal/db/repo/entitlement.go`: make `IsEntitled` / “active entitlement” queries explicitly exclude `deleted_at` (don’t rely on implicit soft-delete behavior)
- [x] `config/config.go`: add a feature flag / config for CCBill grace behavior (enable/disable + max cap)
-
- ANALYTICS:
- [x] Ensure ClickHouse subscription_events reflect dunning/cancelled/expired transitions for CCBill
-
- TESTS:
- [x] Add integration test: NewSaleSuccess -> RenewalFailure(nextRetryDate) keeps access until paid_term_end (+ optional grace) -> RenewalSuccess extends window
- [x] Add integration test: Cancellation mid-term does NOT revoke immediately; entitlement ends at paid_term_end; verify Expiration handling if CCBill sends it
- [x] Add unit test for `EntitlementRepo.IsEntitled` to ensure deleted rows are excluded (regression guard)

---


# #164: cloudflared-managed-dev-tunnel

**Completed:** no

Make `Config.Cloudflared` an actual supported dev feature: OpenRails can (optionally) run and manage a Cloudflare Tunnel for local/dev, so a full local stack (e.g., multiple host apps + billing) can be accessed from an external device (phone) over HTTPS.

## Metadata

- Category: devx
- Status: planned
- Passes: false

## Goal

- With `cloudflared.tunnel_token` configured, a developer can start OpenRails and get a stable public hostname that routes to the local billing instance, usable from a phone browser/app.

## Non-goals

- Do not require Cloudflared in production.
- Do not make Cloudflared a hard dependency for boot.

## Notes

- This is for local/dev convenience (public ingress), not webhook signature bypass.
- Prefer explicit opt-in (config flag or separate command) so production never spawns subprocesses unexpectedly.

**Tasks:**
- DESIGN:
- [ ] Decide opt-in mechanism: (A) standalone CLI subcommand `openrails cloudflared up` that supervises both OpenRails + cloudflared, or (B) OpenRails spawns cloudflared when `cloudflared.tunnel_token` is set and `env=dev`
- [ ] Decide what constitutes “success”: tunnel established + /health/ready reachable via public hostname
-
- IMPLEMENTATION (if spawning subprocess):
- [ ] Add a small `pkg/cloudflared` supervisor that starts `cloudflared tunnel run` with the configured token and captures stdout/stderr into structured logs
- [ ] Ensure clean shutdown: propagate SIGINT/SIGTERM, kill child process group, avoid zombies
- [ ] Add a readiness check that probes the configured `cloudflared.public_hostname` (or local route) and reports status
-
- CONFIG / DOCS:
- [ ] Clarify config semantics in config.example.yaml (token is secret; hostname is non-secret; tunnel name optional)
- [ ] Document a phone-access workflow in docs (prereqs: cloudflared installed or image available; set APIURL/CORS origins appropriately)
-
- SECURITY / SAFETY:
- [ ] Ensure the service API (port 8060) is not accidentally exposed publicly by default; only expose the public API unless explicitly configured
- [ ] Ensure `api_key` and auth verification are still required for protected routes
-
- VERIFY:
- [ ] Add a lightweight unit test for supervisor command construction (no subprocess exec)
- [ ] Manual dev verification: start stack + tunnel and hit /health/ready from an external device

---

# #288: processor-routing-and-fallback-policy

**Completed:** no

Add deterministic processor selection and fallback policy so checkout can choose the best available rail for a tenant/product/tier/user context before creating a one-processor checkout session.

## Metadata

- Category: feature
- Status: planned
- Passes: false

## Motivation

- Hyperswitch wins on payment orchestration. OpenRails should not chase broad smart-routing, but customers do need basic processor redundancy and product/tenant-aware routing, especially high-risk merchants with Stripe plus NMI/Mobius/CCBill/Solana.
- Keep checkout sessions one-processor-at-a-time; route before session creation.

## Non-goals

- No ML routing.
- No 200-connector orchestration platform.
- No automatic retry to a second processor after a successful authorization attempt unless the processor contract makes that safe.

**Tasks:**
- DESIGN:
- [ ] Define routing inputs: tenant_id, product_id, price_id, tier_group, amount, currency, billing cycle, user country/state when known, processor availability, processor capability metadata (#291), and explicit client preference.
- [ ] Define routing outputs: selected processor, fallback candidates, reason, and policy version.
- [ ] Decide precedence: explicit price/provider config > tenant policy > product/tier_group policy > global default.
- [ ] Decide failure classes that can trigger fallback before checkout finalization: processor unavailable, unsupported capability, credential missing, sandbox/live mismatch, hard validation failure. Do not fallback after a successful charge.
-
- DATA MODEL / CONFIG:
- [ ] Add routing policy representation in DB or catalog manifest: allowed processors, preferred order, disabled processors, and optional per-tier overrides.
- [ ] Extend catalog-as-code manifest only if needed; prefer using existing provider lists as the first version.
- [ ] Record selected processor and routing reason on checkout_sessions for auditability.
-
- IMPLEMENTATION:
- [ ] Add a ProcessorRouter service used before checkout session creation and legacy checkout flows.
- [ ] Integrate with processor health/capability metadata so unsupported processors are filtered with explicit errors.
- [ ] Add dry-run endpoint/CLI to explain routing for a price/user/tenant without creating a checkout.
- [ ] Ensure idempotency keys bind to the resolved processor/policy so retries do not unexpectedly switch rails.
-
- VERIFY:
- [ ] Unit-test policy precedence and fallback filtering.
- [ ] Integration-test Stripe primary -> Mobius fallback when Stripe is disabled/unavailable before charge.
- [ ] Integration-test product constrained to Mobius does not route to Stripe even if Stripe is configured.

---

# #290: provider-certification-matrix

**Completed:** no

Publish and maintain a provider certification matrix for Stripe, NMI/Mobius, CCBill, and Solana that records exactly which customer-visible flows are supported, sandbox/devnet tested, live/test-mode tested, and known-limited.

## Metadata

- Category: product
- Status: planned
- Passes: false

## Motivation

- OpenRails' strongest differentiator is deep support for real non-Stripe rails, especially NMI-compatible high-risk gateways like Mobius. Customers need confidence that the specific flows they care about actually work.
- This should become both documentation and an executable certification harness where practical.

**Tasks:**
- MATRIX DESIGN:
- [ ] Define provider capabilities and certification statuses: not_supported, manual_only, unit_tested, integration_tested, sandbox_certified, live_test_mode_certified, devnet_certified, production_certified.
- [ ] Define flows to track: catalog/product push, price/recurring-plan push, one-time checkout, recurring checkout, vault/tokenization, rebill, cancellation, deferred cancellation, refund, dispute/chargeback, webhook handling, subscription sync/backfill, catalog drift detection.
- [ ] Include processor-specific notes: NMI product/prices are local while recurring prices push as NMI recurring plans; CCBill catalog actions may be manual; Solana recurring requires on-chain readback/devnet certification.
-
- DOCS:
- [ ] Add docs/providers.md or equivalent with the current matrix and exact tested commands.
- [ ] Add NMI-compatible gateway guidance: required security_key, Collect.js/tokenization key if needed, direct/query endpoints, test_mode behavior, and how Mobius/NMI white-label accounts map to the same interface.
- [ ] Add troubleshooting for common provider failures: bad endpoint, key belongs to different gateway user, sandbox URL not supported, recurring plan query mismatch, webhook signature failure.
-
- EXECUTABLE CERTIFICATION:
- [ ] Add or formalize focused integration tests for NMI/Mobius sale, vault, recurring plan create/readback, and query API.
- [ ] Add Stripe test-mode catalog command + subscription sync certification steps.
- [ ] Add Solana devnet read-back certification for plan accounts and recurring lifecycle.
- [ ] Add CCBill manual-action verification path so unsupported remote catalog operations surface as pending_manual_actions rather than errors.
-
- PROCESS:
- [ ] Make certification matrix updates part of provider-related PRs.
- [ ] Record last verified date, environment, and command for each provider flow without exposing secrets.

---

# #291: processor-capability-metadata

**Completed:** no

Expose processor capability metadata in code, APIs, catalog planning, checkout validation, and admin/provider status so OpenRails can explain what each rail supports instead of relying on scattered processor-specific conditionals.

## Metadata

- Category: architecture
- Status: planned
- Passes: false

## Motivation

- Stripe, NMI/Mobius, CCBill, and Solana have different lifecycle semantics. Customers need predictable errors and routing decisions. OpenRails also needs a shared capability source for routing/fallback, catalog-as-code, checkout validation, and the provider certification matrix.

**Tasks:**
- CAPABILITY MODEL:
- [ ] Define ProcessorCapabilities with booleans/enums for recurring, one_time, vault/tokenization, hosted checkout, redirect checkout, direct sale, catalog push, recurring plan push, refund, dispute, cancel immediate, cancel deferred, remote subscription listing, remote dedup check, webhooks, drift enumeration, and manual actions.
- [ ] Add capability details for current processors: stripe, mobius/NMI-backed, ccbill, solana.
- [ ] Distinguish processor class capabilities (NMI-backed) from named provider overrides (Mobius).
-
- INTEGRATION POINTS:
- [ ] Use capabilities in checkout validation instead of hard-coded processor switches where practical.
- [ ] Use capabilities in catalog planning/reconciliation to decide provider actions and pending_manual_actions.
- [ ] Use capabilities in routing/fallback policy (#288) so unsupported rails are filtered before checkout.
- [ ] Surface capabilities through admin/provider status endpoints and docs.
-
- ERRORS / UX:
- [ ] Return structured unsupported-capability errors with processor, capability, and suggested alternative when possible.
- [ ] Ensure user-facing errors are clean while admin/debug surfaces retain processor detail.
-
- VERIFY:
- [ ] Unit-test capability metadata for each supported processor.
- [ ] Regression-test known special cases: CCBill manual catalog actions, NMI recurring plan push, Stripe remote dedup check, Solana one-off/recurring distinction.

---

# #320: Add Hyperswitch payment vault support for cloud and self-hosted deployments

**Completed:** no

Add Hyperswitch as an optional OpenRails payment provider and payment-method vault integration, covering both Hyperswitch Cloud and self-hosted Hyperswitch. This is payment vaulting/tokenization, not HashiCorp Vault merchant-secret storage. OpenRails should store only opaque Hyperswitch customer/payment-method identifiers plus non-sensitive metadata, while PAN/card collection stays in Hyperswitch-hosted/client-side tokenization flows or equivalent PCI-scoped Hyperswitch surfaces.

This issue must reconcile with future issue #297 (`deplatforming-resilient-card-vault`): Hyperswitch should not be positioned as the default adult/high-risk deplatforming-resilient vault until its contractual/export/compliance posture is explicitly certified. Hyperswitch Cloud may still be useful for lower-risk deployments or as an optional provider, and self-hosted Hyperswitch may be a break-glass/advanced deployment path if the operator accepts and satisfies the PCI requirements.

This should integrate with the provider capability and routing work in #288, #290, and #291: Hyperswitch can be selected as a processor/vault provider when the tenant/provider config says it is available, and OpenRails can route checkout/setup flows through it without treating it as a generic secret vault.

**Tasks:**
- [ ] Research the current Hyperswitch Cloud and self-hosted API surfaces needed for customers, payment methods, payment/setup intents, saved payment methods, refunds/voids, webhooks, connector routing, and vault/tokenization modes; record any version assumptions in docs.
- [ ] Reconcile the implementation plan with future issue #297: document whether Hyperswitch is an optional provider, a lower-risk deployment choice, or a PCI-heavy break-glass/self-hosted vault path; do not make it the default portable adult/high-risk vault without explicit certification.
- [ ] Define the OpenRails provider config shape for `hyperswitch`: cloud vs self-hosted mode, API base URL, optional vault/base URL split if Hyperswitch requires it, merchant/profile/account identifiers, API key secret reference, webhook secret reference, return/callback URLs, and test/live mode.
- [ ] Store Hyperswitch credentials in the existing tenant secret store path; do not store processor API keys in bootstrap YAML, database rows, logs, or generated frontend config.
- [ ] Extend provider capability metadata (#291) so Hyperswitch can advertise supported flows: payment-method vault/tokenization, one-time checkout, recurring/setup/mandate behavior if supported, refunds, webhooks, processor-side routing, remote payment-method listing, and manual certification status.
- [ ] Add a provider adapter/client abstraction that lets NMI customer-vault IDs and Hyperswitch payment-method IDs fit the same OpenRails payment-method model without hard-coding NMI semantics into checkout/subscription flows.
- [ ] Implement Hyperswitch customer/payment-method setup flow using an opaque token or hosted/client-side Hyperswitch collection result; persist only tenant subject, provider, external customer ID, external payment method ID, brand/last4/expiry metadata, and status.
- [ ] Implement checkout/subscription charge paths that can use a saved Hyperswitch payment method and reconcile the resulting payment/subscription state back into OpenRails records.
- [ ] Implement webhook verification and event handling for successful payment, failed payment, refund/void, payment-method updates/deletes, and any recurring/mandate lifecycle events OpenRails relies on.
- [ ] Add self-hosted operational docs: required Hyperswitch services, base URL/TLS requirements, webhook reachability, secret injection, health checks, and local compose/dev smoke path if practical.
- [ ] Add cloud operational docs: required Hyperswitch Cloud credentials, webhook setup, provider certification checklist entry (#290), and tenant bootstrap/provider-link examples.
- [ ] Add tests with a mocked Hyperswitch server/client for tokenization/setup, saved-method charge, failure mapping, webhook signature verification, idempotency, tenant isolation, and no-sensitive-card-data persistence/logging.
- [ ] Validate with focused Go tests for provider/checkout/vault/webhook code, compile-only full package coverage, `task build`, and an optional live sandbox/self-hosted smoke test when credentials or local Hyperswitch are available.

---

# #331: Tune card-abuse rate-limit windows (#371): 15-min 3->captcha/6->block, 24h 10->block per user+ip, keep global 100->attack-mode

**Completed:** no

The #371 CardAbuseGuard (internal/modules/abuse/card_abuse.go), middleware subject-key plumbing, checkout hook, and integration tests already landed on master. This refines the policy to the agreed windows. Card-testing is defined by volume over time, so pair a short BURST window with a rolling DAILY cap, both keyed per user AND per IP. Final thresholds (user-specified): a 15-MINUTE rolling window per (user+ip) where 3 failed card charges -> future attempts require captcha, and 6 failed charges -> cut off (blocked) for the remainder of that 15-min window; PLUS a 24-HOUR rolling cap of 10 failed charges per (user+ip) -> cut off for the day. Keep the existing site-wide GLOBAL cap of 100 failed charges / 24h -> attack mode (captcha for every request from everyone).

**Tasks:**
- [ ] DefaultCardAbuseConfig: short window 15 min, CaptchaAfter=3, BlockAfter=6 (block lasts the remainder of the window)
- [ ] Add a second per-subject rolling 24h window with a cap of 10 failed charges -> block; the Limiter already returns multiple windows, so configure the daily window + add the second-window check in RecordChargeFailure
- [ ] Keep both subject keys (user: and ip:) so a logged-in attacker rotating cards and a bot rotating accounts behind one IP are both caught
- [ ] Keep the global 24h/100 attack-mode (captcha for everyone) unchanged
- [ ] Extend the abuse integration tests (real Redis) to cover the 15-min 3/6 thresholds and the 24h/10 daily cap; go build ./... + go vet ./... clean

---

# #333: admin-e2e-suite-needs-control-plane-wiring

**Completed:** no
**Status:** OPEN 2026-06-10 (Claude): diagnosed during the #332 failure review; not started. All other e2e failures from that review are fixed (see completed #332 + commits 9fe72d4 openrails, 68cb6a1 tensorhub, b72f61e8 cozy-art).

The admin e2e tests (tests/admin_payments_test.go, admin_metrics_test.go, admin_entitlements_source_test.go, admin_offchannel_payments_test.go) fail with 500 {'message':'authorization unavailable'} — the nil-admin-checker fail-closed path. DIAGNOSIS (2026-06-10): pre-existing #312 bit-rot, documented in the suite itself (testcontainer_suite.go Auth config comment: 'Admin-route integration tests therefore require the embedded control plane wired with the test admin granted openrails:admin'). The #312 hard-cut moved admin authority from JWT claims (tests still mint operatorAdminClaims() tokens via test_helpers.go) to the LIVE control-plane permission check (routes_admin.go only sets AdminPermissionChecker when s.controlPlane != nil; the suite never enables Auth.ControlPlane -> checker nil -> admin_neutral.go/ginmw admin gates 500 fail-closed).

WHAT THE FIX NEEDS:
1. suite.Config.Auth.ControlPlane = &config.ControlPlaneConfig{Enabled: true} (ginboot embcp.Attach then builds it; authkit profiles schema is already applied by migrate.RunPostgres).
2. The admin test users must EXIST in authkit profiles.users with ids equal to the JWT subs the suite mints, then be made members + granted the admin role (controlplane Bootstrap/AddMember/AssignRole or authkit core APIs — NOT raw SQL into roles). authkit core.CreateUser(email, username) generates its own id, so either (a) resolve the verifier's user mapping (issuer+sub -> user) and mint tokens for created users' real ids, or (b) add a test-only authkit user-provisioning hook.
3. operatorAdminClaims()/CreateAdminToken in tests/test_helpers.go are then vestigial — admin authority comes from the role grant, not claims.

NOTE: the admin-METRICS tests' 'Table test_analytics.daily_metrics does not exist' failures were a STALE TEST ENV artifact (migratekit records the ClickHouse ledger in postgres; reusing a postgres DB across runs against fresh CH containers skips re-apply). On a fresh env CH applies fine and those tests fail at the same admin-auth gate instead. Squashed CH baseline (cb9200b) is NOT at fault (daily_metrics present; migrations/clickhouse schema_test passes).

**Tasks:**
- [ ] Enable Auth.ControlPlane in testcontainer_suite config and verify embcp.Attach builds it in ginboot.
- [ ] Provision admin test users in authkit profiles.users matching the JWT subs (or mint tokens from created users' ids); AddMember + AssignRole openrails:admin via control plane / authkit core APIs.
- [ ] Re-run tests/admin_*.go on a fresh env; remove vestigial operatorAdminClaims if green.
- [ ] (env hygiene) consider failing loudly or re-validating when the migratekit CH ledger says applied but the CH database lacks the tables (stale-ledger detection).

---


# #340: e2e: 5 tier/lifecycle tests fail on master (pre-existing, surfaced by #334 verification)

**Completed:** no
**Status:** open (filed 2026-06-10 during #334 verification)

TestTierGroupDetection, TestEntitlementChangesOnTierChange, TestScheduledDowngrade, TestRenewMembershipDuplicateTransactionIsNoOp, TestLifecycleServiceUsesMockClock fail identically on origin/master (bun era) and on the sqlc-migration branch — verified via worktree runs on 2026-06-10 during #334 final verification, so they are NOT migration regressions. Observed modes: duplicate key on uq_subscriptions_tenant_subject_tier_group_active across subtests (CleanupSubscriptionsForUser resolves non-UUID test user ids to uuid.Nil via identity.TenantSubjectIDFromString, so per-user cleanup deletes nothing), and mock-clock/lifecycle assertion failures. Distinct from the admin_* family (#333). Suspect shared-suite state + the broken per-user cleanup; fix the cleanup to resolve the tenant subject through billing.tenant_subjects (issuer 'openrails:legacy-user') instead of pure-parsing.
