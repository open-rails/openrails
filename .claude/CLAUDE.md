# OpenRails — project guide for Claude

OpenRails is a multi-merchant billing/payments platform (Go). It runs **standalone**
(hosted SaaS, many merchants) and **embedded** (a host app embeds it and declares its merchants in `Config.Merchants`).
One merchant ↔ one controlling org (deliberately 1:1). The repo is **source-available** —
keep secrets and customer/account-specific identifiers OUT of editable committed
files (code, trackers, this file). Neutral examples in this repository use `host-one`
through `host-four`; these are placeholders, not customer or repository names.

## Modules
- Root `github.com/open-rails/openrails`: the embedded library. It imports AuthKit's
  standard-library-only `auth` and `userinfo` contract packages, versioned in the
  same AuthKit module. `scripts/check-embedded-auth-boundary.sh` forbids AuthKit's
  runtime implementation from the embedded package graph; hosts opt into that
  implementation themselves. There is no separate contract module or version.
- `server/` is its own module, `github.com/open-rails/openrails/server`: package `server`,
  the binary `server/cmd/openrails`, everything AuthKit-backed under `server/internal/`
  (controlplane, operator, hostconfig, …) and `server/ci/paymentpage`, a payment-page
  fixture server nothing runs. It may
  import the root's `internal/` packages; the root never imports it. In the repo it builds
  against the root's source (`replace … => ../`).
- `web/` is the browser code, in the root module: `web/billing-ui` (the npm library
  `@openrails/billing-ui`), and the payment page `web/pay` and admin console `web/console`,
  which each `go:embed` their own `dist/` (only `dist/.gitkeep` is committed).
- `examples/` and `adapters/authkit` are modules too. Check each on its own (`GOWORK=off`;
  `scripts/check.sh build` builds all four).
- `scripts/release` releases root, server and the AuthKit adapter in lockstep: `prepare vX.Y.Z`
  opens a PR pinning their requires of the root; once it is green, `publish vX.Y.Z` fast-forwards
  master to it and pushes the `vX.Y.Z`, `server/vX.Y.Z` and `adapters/authkit/vX.Y.Z` tags with it.

## Branches
- Open RC → master → tag. `OPEN.md` names the one open RC, `rc/X.Y.Z` for the version it becomes (cut from master, deleted once merged); work lands on it, never on master.
- Push to the RC after a local build (rebase, never force). The push check runs the light tier over the pushed range (`Push check`, never `CI Gate`); it blocks nothing, and whoever's push turns it red fixes it first. A newer push cancels the older check. PRs into the RC and its merge queue are optional.
- A ready PR into `rc/**` runs the light tier: the suites `scripts/ci-scope.py` selects from its changed paths (a deletion or rename selects all; docs-only runs the guards alone), with no `critical/` and no gosec. Draft PRs run nothing; `CI Gate` needs every selected job. `gh pr merge N --squash` (or `--auto --squash`) puts it in the merge queue, which runs the light suites over each group's combined changes on top of the RC and the groups ahead of it; `--admin` skips the gate and the queue.
- Merge groups also run the whole critical suite once `QUEUE_RUNS_CRITICAL` in `scripts/ci-scope.py` is on. It stays off until a full run of the RC is green: while the RC is red every group would fail, the fixes included.
- The RC → master PR, and a `Validate` dispatch with no selection, run everything once: every suite, gosec, and the critical suite on `CRITICAL_SHARDS` runners balanced by `critical/durations.json`; `Critical coverage` proves from the shards' reports that every scenario ran once. Dispatch full or focused runs on the RC itself (`gh workflow run Validate --ref rc/X.Y.Z [-f critical="J24 OR-J04-S80"]`): each dispatch is its own concurrency group, cancels nothing and reads the RC's warm caches; a `verify/` branch can read only its own and master's.
- Only pushes (to `rc/**` and master) save the Go caches, one entry per family and branch: pull requests restore their base's.
- No bot opens dependency PRs: `scripts/deps` lists the advisory feed's open alerts; fixes go in one batch per RC.
- Master takes PRs only from `rc/X.Y.Z` and `scripts/release`'s `chore/release-vX.Y.Z` (`Source branch` check).
  A release is a tag on master; not every master commit is one.

## v1 is frozen
- From v1.0.0 three contracts change only by adding to them (`compatibility/README.md`):
  the Go API (`openrails`, `billing`, `catalog`, `adapters/*`, `web/console`, `web/pay`, `server`), the
  HTTP API, and the database schema. `api/go.txt` lists both modules.
- Each has a generated snapshot: `api/go.txt`, `api/openapi.json`, `api/schema.txt`, and the
  event store's `api/clickhouse.txt`. `go run ./scripts/contracts` verifies them (CI runs it with
  `-dsn` and `-clickhouse`); `-write` rewrites them with `compatibility/contract.json` and the files
  generated from the route catalog. The schema lists need `-dsn` (or `OPENRAILS_E2E_DSN`) naming a
  disposable PostgreSQL 18 and `-clickhouse` a disposable ClickHouse 26.8.
  Never hand-merge them.
- A removed or changed snapshot line is a break. Don't make one without the owner.

## Public surface
- The root package is the interface: `openrails.New` (in process) returns the `*Engine`
  (workers, routes, every declared merchant), and `engine.Merchant(id)` the `*Client` bound
  to one merchant; over HTTP, `openrails.NewRemote` (a credential per merchant id) and
  `Remote.Merchant(id)` return the same `*Client`. Flat methods (`Get/List/Create/Update/Delete/Set/Ensure`,
  or an action verb from the closed list in `internal/apisurface/lint.go`), `ctx` first,
  request structs, typed IDs (`billing.CustomerID`, `billing.PSPID`, …), typed enums and
  `catalog.Currency`. A positional `string` is only a host-chosen `key`, `reference`,
  `requestID(s)` or `idempotencyKey`. Types live in `billing`; the catalog document in `catalog`. Everything
  else is `internal/`. `Config` is plain data; anything that reaches outside the process
  is in `Deps`.
- Settings are one type: `openrails.Config` is `billing.Config` (the API's one alias),
  which internal packages use directly; its `yaml` tags are the standalone file's keys.
  `billing.DefaultConfig()` is the one defaults table (`New` fills zero fields from it);
  an optional section is a value, on once set. The binary's file decodes one tree into
  `server.Config` (engine keys inline) and its own connection keys; a key neither reads
  refuses boot. No mirror config types or converters.
- The Go client is the admin API, generated into `client_gen.go` from the route catalog
  (`Name`, `Doc`, `Args`, `Query`, `Cache`): one `*Client` method per admin, catalog-edit and
  merchant-config route. `HandWritten` routes and the methods no route has live in
  `client_handwritten.go`; any other exported method fails generation. Customer
  self-service (`/v1/me`) is for browsers through `web/billing-ui`.
- Every route is declared once in the route catalog (`internal/http/routes`, one file
  per resource) with its tier, level (an admin route: read or write) or permission,
  request, responses and error codes. The catalog mounts the route and generates
  `api/openapi.json`, the TypeScript wire types of billing-ui and the console, and
  `docs/api/routes.md` / `error-codes.md`.
- Route groups: public, customer (`/v1/me`) and webhooks are always mounted. The staff
  groups (`Routes.RouteGroups`: `Admin`, `Catalog`, `MerchantConfig`, `Metrics`) are off
  unless the host turns them on; `Programmatic` (`/v1/app`) admits applications.
  Customer support uses `/v1/admin`, Metrics uses `/v1/metrics`, Catalog uses `/v1/catalog`,
  MerchantConfig uses `/v1/merchant`, including findings, proposals and PSP imports.
  No public route reads the catalog: a host's server reads it with the Go client and puts products in its
  own pages (`billing.BuyerPrices`). Customer-support permissions
  are `merchant:customer:read` and `merchant:customer:manage` (or their `root:` twins); catalog, config
  and metrics remain separate capabilities. There are no legacy-path aliases.
  No route registers a merchant or manages a
  team, API keys or the merchant directory: those are `server` Go methods and
  the `openrails` CLI; a hosted product builds its own routes on them.
- A merchant route acts on the merchant its credential is bound to: its group, the
  declared `Issuer`, or an OpenRails object. The request names none; anything else is
  403 `merchant_unresolved`.
- Permissions are OpenRails' own: `billing.Permission` constants, always the full
  `persona:resource:action` (`read`/`manage`), listed by `billing.Permissions()`. Each
  route declares a pair: the gate admits its `merchant:` name in the bound merchant's
  group or its `root:` name in the Authenticator's root scope (`authkit/auth.Root`).
  Hosts build their roles from the constants and pass none; `Mount` fails for a route
  neither of whose names the Authenticator knows. The in-process `Client` checks none.
- OpenRails admin and operator changes never require step-up or a recent sign-in;
  `AuthMerchant` routes must not be `Sensitive`. Their authority is the route's
  permission. AuthKit retains its own recent-sign-in checks for root-group,
  OAuth-client and remote-application mutations; customer and customer-network
  policies are unchanged.
- Wire: lists are `{data, next_cursor}` (cursor only); DELETE answers 204; nulls are
  present; times are RFC 3339 UTC; unknown request fields are refused; error codes
  come from the registry (`billing.ErrorCodes()`); IDs are prefixed (`psp_`, `oat_`, …).
- Customer messages: OpenRails holds each customer's contact (read live from the host's directory, or the SCIM copy in `customer_contacts`) and emails customers their billing messages (`internal/modules/messages`, through `Deps.Email`); the merchant's `messages` document turns each event type on or off. In-app notices go to AuthKit's inbox (`Deps.Notifications`); OpenRails keeps no inbox. Removing customer email is the owner's call, never a side effect of other work.
- Vocabulary: customer (not payer or user); subject (the native account acted as), invoker (the party actually acting, may be foreign), credential (how it was proven); `canceled`.

## Money
- All amounts are **micros** (millionths of a currency unit) for USD; every currency's
  scale is in the registry (`GET /v1/config`'s `currencies`; a merchant's own units, such as
  credits or in-game coins, are in `GET /v1/me/config` and never in the general ledger). Not cents, not millicents.
- In Go an `int64`, on the wire a decimal string named `amount` beside a `currency`;
  never a currency in a field name.
- A double-entry ledger is the source of truth for money; a separate grant ledger tracks
  credit lots. FX is forbidden inside the ledger (no cross-currency transfers).
- Every provider money boundary has a test pinning the exact value sent on the wire
  (micros, cents or major units), beside a typed conversion.

## Rails and PSPs
- TERMINOLOGY (frozen): a **rail** is the gateway KIND (nmi/ccbill/stripe/solana — the
  `billing.Rail` enum). A **PSP** (payment service provider) is a merchant's concrete
  ACCOUNT on a rail (e.g. "mobius", "paykings" on nmi): credentials + `account_id` + key
  (`psps.key`). A PSP is NOT the acquiring bank. Solana is the self-custody wallet slot.
- PSP routes are `/v1/merchant/psps` (`ListPSPs`, `CreatePSP`, `UpdatePSP`, which also archives);
  rails are read in `GET /v1/config` (`rails`). Provider callbacks land on
  `/v1/webhooks/{rail}/{account_id}`.
- A table that stores both `rail` and `psp_id` keeps them in agreement by a composite
  foreign key to `psps`.
- A provider-owned subscription is never moved or canceled by OpenRails; it stays on its
  (possibly archived) PSP until it drains.
- Solana subscriptions are engine-owned: its program only authorizes a pull. The
  specialized cranker freezes its accepted agreement before signing, qualifies
  the exact chain receipt, and atomically settles the bill/payment/attempt and
  access period. The generic saved-card collector never admits Solana.
- ALL outbound Stripe HTTP goes through the choke-point client `internal/integrations/stripeapi`
  (readonly mode blocks writes at the transport). It pins the Stripe API version via
  `stripeapi.APIVersion`: ONE const drives the outbound `Stripe-Version` header AND the
  webhook-endpoint registration. Bump it deliberately; don't float.
- ALL NMI HTTP goes through `internal/integrations/nmi`.
- Rails evidence: CI's deterministic NMI and Stripe transports are regression coverage, not
  provider qualification. A sandbox or live run qualifies only the operation exercised; its
  record names posture, request, response and date, never credentials or amounts. A change to
  a rail's wire behaviour is unverified until exercised against the provider again.

## PSP identity
- `billing.psps` is an OPERATOR-DECLARED catalog: `account_id` is the operator's label.
  Before a PSP is armed its credentials are probed (`internal/providerposture`): the
  rail's live/test environment, and on Stripe that the key belongs to the declared
  account (`Mismatched` otherwise). NMI has no such read: nothing ties a Gateway ID to its key.
- A merchant declares a PSP as `psps.<key>: {rail: nmi, environment: live|test|sandbox, account_id: …, settings: …}`
  (`billing.PSPConfig`; `merchants.<slug>.psps.<key>` in a standalone manifest), with
  credentials under `psps.<key>.secrets`. Merchant configuration is that file (read-only)
  or, with `vault.kv_mount`, Vault: one document per object
  (`<scope_prefix>/merchants/<merchant-id>/psps/<key>`); never Postgres. The retired
  manifest keys (`rail_merchant_accounts`, `provider_accounts`) fail loudly with a rename error.
- Each rail names its account in its own terms (joined with `-` into the internal
  account its id and webhook path use):
  - **NMI** — `gateway_id`, the dashboard **"Gateway ID"**, which IS the merchant account
    id. It is NOT the reseller/ISO, and is NOT fetchable from the `security_key`.
  - **Stripe** — `account_id`, `acct_…`, declared by the operator and checked against the
    key's own account (`GET /v1/account`, `internal/integrations/stripeapi/posture.go`).
  - **CCBill** — `client_accnum` and `client_subacc`, CCBill's own numbers.
  - **Solana** — `treasury`, where every payment lands. Plans are owned by the
    deployment's `solana.crankers`, never by a PSP.
  - Elsewhere the rule is the opposite: **the less a merchant configures, the better**.
    Where an authoritative source exists, derive from it and delete the knob.

## Catalog
- One document model, `catalog.Application` (`catalog.ParseApplicationYAML`). Applying
  one is an operator action: `Client.ApplyCatalog` (a host's own command),
  `POST /v1/catalog/apply` or `openrails catalog apply FILE`. Startup never applies or
  mutates a catalog; don't add a boot-time apply. Files and console edits share
  fields by ownership: a field an edit changed last is not overwritten by the
  file, and the conflict is reported.
- `prices` are OpenRails-owned terms; new card subscriptions always use its billing
  cycle. `provider_prices` mirror existing NMI/Stripe objects and declare CCBill
  FlexForms, read-only. NMI and Stripe catalog objects are never created or edited.
  A recurring price of ours may publish its Solana plan on apply. Entitlements are
  plain strings: the keys of a product's `entitlements_spec`.

## Schema / DB
- SQL is authored in `billing` (`sqlschema.Authored`) and runs there verbatim; any
  other schema is reached by one token-aware rewriter (`internal/sqlschema`) that moves
  schema references only.
- One hand-written baseline, `internal/migrate/postgres/0001_schema.up.sql`: tables in
  foreign-key order, each followed by its constraints, indexes and triggers. A release has
  shipped it, so a schema change is a new numbered migration (`0002_…`, added to `sqlc.yaml`)
  and `api/schema.txt` shows it; a schema change never edits the baseline. Rewriting the
  baseline is a deliberate re-squash proven by CI's `scripts/schemaproof` step, because every
  deployed database must then be settled against it; after v1.0.0 it is immutable.
- `Migrate`/`New` settle a database that recorded a migration with other content: a schema
  equal to a fresh build is re-stamped; any other is
  refused with the difference. A `SkipMigrations` start (`migrate.Validate`) only checks, and
  refuses a missing migration or one recorded with other content (`ErrMigrationsRequired`)
  until `Migrate` has run. Only OpenRails' objects live in its schema: the comparison covers
  all of it.
- `New` applies the migrations; the pool it runs with owns every object (another owner is
  the operator's `SET ROLE`). No grants, no runtime role.
- There is NO row-level security: tenant isolation is the explicit `merchant_id` (or
  `psp_id`) predicate on every tenant query, plus composite foreign keys and
  `PRIMARY KEY (merchant_id, id)` on tenant tables.
- Session settings (GUCs) are `openrails.<what the value holds>`:
  `openrails.merchant_id`, `openrails.subscription_decision`, `openrails.retention_table`,
  `openrails.billing_restore_id`, `openrails.catalog_batch_merchant_id`. Never `app.*`
  (the host's) or the schema name; they are string literals the schema rewriter never moves.
- Instants end in `_at`. A surrogate id defaults to `uuidv7()`; `created_at`, `updated_at`
  and a table's own creation instant to `now()`. No other column defaults to a value every
  writer supplies (one stated exception: `payments.money_movement` fails closed to `'none'`).
- Absent text is NULL, and a CHECK refuses `''`; `''` is stored only as part of a key
  (`payment_attempts.step`, `nmi_history_months.reason`). No `varchar`.
- Text + CHECK, never Postgres enums. SQL lives in sqlc queries, and `sqlc vet` prepares
  every one against PostgreSQL.
- Every table has a retention class (`internal/retention`: permanent, rows, state),
  stated in its table comment; the `retention` package doc lists the periods.
- External writes are `provider_intents` and their write-ahead `operation_log`; per-PSP state is
  `psp_customers` and `psp_refresh_watermarks`. Every bill is an `orders` row (`ord_`)
  with its `order_lines`, provider-run renewals included (`billing.Order`'s doc);
  an `order_attempts` row (`oat_`) is a provider attempt; a host
  charges its own metered price with a `running_charges` row.
- A new table also goes in the merchant archive's `ownedTables` and the restore guard.
- Usage events live in ClickHouse (`Deps.ClickHouse`, `internal/eventstore`, its own embedded
  migrations); money stays in PostgreSQL. Usage posts to the ledger at each month's end in
  `accounting.timezone` and before any other movement of the balance, as the period's total
  price less earlier postings (`usage_postings`); money reads use `FINAL`. Whether
  `Deps.ClickHouse` is set decides usage: without it the event-backed routes
  (`Route.EventStore`), catalog declarations and settings answer 409 `event_store_required`.
- User-facing records soft-delete: `deleted_at`, and reads filter `deleted_at IS NULL`.
  Hard deletes only for privacy erasure and derived or short-lived rows.
- Abuse state (rate limits, lockouts, captcha, card-testing counts, replay ids) and
  caches (FX rates) live in Redis, or in process memory without Redis; never in Postgres.
  Several instances need Redis. Destructive-command admissions and holds are the
  deliberate exception (`DestructiveActionLimitsConfig`'s doc).

## Layer altitude
- A layer earns its existence by doing work at its own altitude. Modules talk to sqlc `gen`
  directly; repo-style wrappers exist only where they carry logic and live IN the owning
  module. Handlers may call `gen` for orchestration-free reads. Never add a wrapper just
  to "complete" a layer.

## Engineering rules
- Background work scales with activity, not records: watermarks, indexed due work and
  push. No routine sweep over every row.
- No evidence, no action: OpenRails never cancels, revokes or deletes on uncertain data;
  a subscription it cannot confirm is `unverified`, and `unverified` keeps access.
- Libraries never read the environment; only binaries and config loaders do. Environment
  variables are configuration and secrets, never logic gates.

## Trackers (issues)
- The tracker is the separate `open-rails/tracker` repository. OpenRails issues are one
  file each: active `openrails/<id>.md`, parked `openrails/future/<id>.md`, completed
  `openrails/completed/<id>.md`. `openrails/README.md` owns the shared `next_id` counter.
- CONCURRENT-EDIT SAFE: only edit the issue you own and its index entries.
- Standing product rules are in the tracker's `openrails/specs/principles.md`; designs live
  in tracker issues, which are the source of truth.

## Docs
- `docs/` holds the integration guide (`guide.md`, `embedded.md`, `standalone.md`,
  `client.md`), the generated API references and the architecture docs; design history
  lives in the tracker, maintainer internals in Go doc comments on what they describe.
  No tracker ids, no host names. A document names only routes, Go identifiers, tables,
  permissions and error codes that exist.
- `go run ./scripts/docscheck` (in guards) enforces it: Go and YAML fences equal their
  `// guide:<name>` regions in `examples/` (`-write` refreshes them), references and links
  resolve, and the guide names every handle method, server config key, chart value and
  CLI command, and none that does not exist. `docs/architecture/` gets the link check only.

## Tests
- The suite is `critical/`: named end-to-end scenarios over real PostgreSQL, Redis, Vault and ClickHouse (`OPENRAILS_CRITICAL_DSN`, `_REDIS_URL`, `_VAULT_ADDR`, `_VAULT_TOKEN`, `_CLICKHOUSE_ADDR`, all owned and disposable; each world creates its own PostgreSQL schema and ClickHouse database). `critical/coverage.json` lists the scenarios and `critical/executables.json` maps each to the test that runs it; a `t.Run` with subtests registers its parent too. `scripts/critical-tests.sh --case OR-Jxx-Syy` (or `--journey Jxx`) runs a selection against disposable services (`OPENRAILS_CRITICAL_*`); CI runs them all in shards (`--shard K/N`). A `Validate` dispatch with `critical: "J24 OR-J04-S80"` runs only those in CI, on one runner, and qualifies nothing.
- After a full run, refresh the shard balance: `gh run download RUN -p 'critical-results-*' -D DIR`, then `python3 scripts/critical-results.py critical/coverage.json DIR/*/events.jsonl --write-durations`. A test missing from `critical/durations.json` weighs the median.
- A behaviour change gets a scenario that fails first, and sweeps `critical/` for the codes,
  constants and statuses it changed. Green-in-my-package is not green.
- A test that needs PostgreSQL, Redis, Vault or a fixture process is tagged `integration`, like
  `critical/`; every other test needs nothing outside its process. Every PR with a Go change runs
  `scripts/check.sh vet` (each module, every build tag) and `unit` (each module's untagged tests).
- Static checks: `scripts/check.sh guards`, `scripts/check-embedded-auth-boundary.sh`,
  `sqlc vet` and `go run ./scripts/contracts`. `go run ./scripts/contracts -lint` (in guards)
  checks the Go API's conventions; it has no baseline left, so any new finding fails.
