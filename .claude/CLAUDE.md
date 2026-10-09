# OpenRails — project guide for Claude

OpenRails is a multi-merchant billing/payments platform (Go). It runs **standalone**
(hosted SaaS, many merchants) and **embedded** (a host app embeds it for one merchant).
One merchant ↔ one controlling org (deliberately 1:1). The repo is **source-available** —
keep secrets and customer/account-specific identifiers OUT of editable committed
files (code, trackers, this file). Neutral examples in this repository use `host-one`
through `host-four`; these are placeholders, not customer or repository names.

## v1 is frozen
- From v1.0.0 three contracts change only by adding to them (`docs/compatibility.md`):
  the Go API (`openrails`, `billing`, `catalog`, `adapters/*`, `web/admin`), the HTTP API,
  and the database schema.
- Each has a generated snapshot: `api/go.txt` (`TestGoAPISurface`), `api/openapi.json`
  (`TestGeneratedContractIsFresh`), `api/schema.txt` (`TestSchemaSnapshot` in `ci/`).
  `go run ./scripts/contracts -write` rewrites all three; the schema needs
  `OPENRAILS_E2E_DSN` naming a disposable PostgreSQL 18. Never hand-merge them.
- A removed or changed snapshot line is a break. Don't make one without the owner.

## Public surface
- The root package is the interface: one `*openrails.Client` from `openrails.New` (in
  process) or `openrails.NewRemote`, flat methods (`Get/List/Create/Update/Delete/Set/Ensure`),
  `ctx` first, request structs, typed IDs (`billing.CustomerID`, `billing.PSPID`, …) and
  typed enums. Types live in `billing`; the catalog document in `catalog`. Everything
  else is `internal/`. `Config` is plain data; anything that reaches outside the process
  is in `Deps`.
- The Go client is the merchant API: each merchant route has one `*Client` method.
  Customer self-service (`/v1/me`) is for browsers through `sdk/billing-ui`.
- Every route is declared once in the route catalog (`internal/http/routes`, one file
  per resource) with its tier, permission, request, responses and error codes. The
  catalog mounts the route and generates `api/openapi.json`, the TypeScript wire types
  of billing-ui and the console, and `docs/api/routes.md` / `error-codes.md`.
- Route groups: checkout, customer (`/v1/me`), one merchant group (`/v1/merchant`,
  gated by permission, `Routes.Merchant`), webhooks, and the standalone control
  plane and platform.
- Wire: lists are `{data, next_cursor}` (cursor only); DELETE answers 204; nulls are
  present; times are RFC 3339 UTC; unknown request fields are refused; error codes
  come from the registry (`billing.ErrorCodes()`); IDs are prefixed (`psp_`, `chk_`, …).
- Vocabulary: customer (not payer or user); subject (the native account acted as), invoker (the party actually acting, may be foreign), credential (how it was proven); `canceled`.

## Money
- All amounts are **micros** (millionths of a currency unit) for USD; every currency's
  scale is in the registry (`GET /v1/currencies`). Not cents, not millicents.
- In Go an `int64`, on the wire a decimal string named `amount` beside a `currency`;
  never a currency in a field name.
- A double-entry ledger is the source of truth for money; a separate grant ledger tracks
  credit lots. FX is forbidden inside the ledger (no cross-currency transfers).

## Rails and PSPs
- TERMINOLOGY (frozen): a **rail** is the gateway KIND (nmi/ccbill/stripe/solana — the
  `billing.Rail` enum). A **PSP** (payment service provider) is a merchant's concrete
  ACCOUNT on a rail (e.g. "mobius", "paykings" on nmi): credentials + `account_id` + key
  (`psps.key`). A PSP is NOT the acquiring bank. Solana is the self-custody wallet slot.
- PSP routes are `/v1/merchant/psps` (`ListPSPs`, `CreatePSP`, `UpdatePSP`, `ArchivePSP`);
  rails are read at `/v1/merchant/rails`. Provider callbacks land on
  `/v1/webhooks/{rail}/{account_id}`.
- A table that stores both `rail` and `psp_id` keeps them in agreement by a composite
  foreign key to `psps`.
- A provider-owned subscription is never moved or canceled by OpenRails; it stays on its
  (possibly archived) PSP until it drains.
- ALL outbound Stripe HTTP goes through the choke-point client `internal/integrations/stripeapi`
  (readonly mode blocks writes at the transport). It pins the Stripe API version via
  `stripeapi.APIVersion`: ONE const drives the outbound `Stripe-Version` header AND the
  webhook-endpoint registration. Bump it deliberately; don't float.
- ALL NMI HTTP goes through `internal/integrations/nmi`.

## PSP identity
- `billing.psps` is an OPERATOR-DECLARED catalog. There is NO runtime "whoami" and NO
  account-mismatch guard. `account_id` is an opaque, operator-declared label.
- A merchant declares a PSP as `psps.<key>: {rail: nmi, account_id: …, settings: …}`
  (`openrails.PSPConfig`; `merchants.<slug>.psps.<key>` in a standalone manifest), with
  credentials under `psps.<key>.secrets`. The merchant-secret name is
  `psps/<rail>/<environment>/<account_id>/<key>`. The retired manifest keys
  (`rail_merchant_accounts`, `provider_accounts`) fail loudly with a rename error.
- Per rail, the declared `account_id` is:
  - **NMI** — the dashboard **"Gateway ID"**, which IS the merchant account id. It is NOT
    the reseller/ISO, and is NOT fetchable from the `security_key`.
  - **Stripe** — `acct_…`, operator-declared like every other rail (there is no
    `GET /v1/account` call in the tree; see docs/rails/stripe.md).
  - **CCBill** — `clientAccnum-clientSubacc`, dash-joined like `999999-0000`.
  - **Solana** — DERIVED from the signer public key (a declared `account_id` is ignored
    with a warning); the payout destination is `settings.recipient_wallet`.
  - Don't derive these from credentials at runtime: `account_id` is a SEGMENT of the
    secret path, so fetching it needs the credential, which needs the path. Circular.
  - Elsewhere the rule is the opposite: **the less a merchant configures, the better**.
    Where an authoritative source exists, derive from it and delete the knob.

## Catalog
- One document model, `catalog.Application` (`catalog.ParseApplicationYAML`); a host
  whose file is the truth sets `Config.Catalog`, and `New` applies it.
- The **pull** reconciliation job (`internal/river/jobs_catalog_reconciliation.go`) is
  ALERT-ONLY: it never mutates providers.
- The **push** path is the provider adapter, e.g. `AutoCreate` in
  `internal/service/catalog_provider_stripe.go`. Entitlements are plain strings: the
  keys of a product's `entitlements_spec`.

## Schema / DB
- SQL is authored in `billing` (`config.DefaultSchema`) and runs there verbatim; any
  other schema is reached by one token-aware rewriter (`internal/sqlschema`) that moves
  schema references only.
- One baseline, `internal/migrate/postgres/0001_schema.up.sql`, immutable after v1.0.0;
  a schema change is a new numbered migration, and `api/schema.txt` shows it.
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
- Text + CHECK, never Postgres enums. SQL lives in sqlc queries (`TestNoInlineSQL`), and
  `TestQueryAudit` plans every one.
- Every table has a retention class (`internal/retention`: permanent, partitioned, rows,
  state), stated in its table comment; `docs/operations.md` lists the periods.
  `usage_events` and `admission_operations` are partitioned by month, so every read of
  them names a time range.
- External writes are `provider_intents` and `provider_mutation_logs`; per-PSP state is
  `psp_customers` and `psp_refresh_watermarks`; upstream compute cost is
  `cost_qualifications` and `cost_observations`. A `checkout_attempts` row (`chk_`) is a
  provider attempt; a `checkout_sessions` row (`ocs_`) is what a browser pays.
- A new table also goes in the merchant archive's `ownedTables` and the restore guard.

## Layer altitude
- A layer earns its existence by doing work at its own altitude. Modules talk to sqlc `gen`
  directly; repo-style wrappers exist only where they carry logic and live IN the owning
  module. Handlers may call `gen` for orchestration-free reads. Never add a wrapper just
  to "complete" a layer.

## Trackers (issues)
- The tracker is the separate `open-rails/tracker` repository. OpenRails issues are one
  file each: active `openrails/<id>.md`, parked `openrails/future/<id>.md`, completed
  `openrails/completed/<id>.md`. `openrails/README.md` owns the shared `next_id` counter.
- CONCURRENT-EDIT SAFE: only edit the issue you own and its index entries.

## Docs
- `docs/` is public documentation for integrators and operators; design history lives in
  the tracker. No tracker ids, no host names. `TestDocsNameWhatExists` fails a document
  that names a route, Go identifier, table, permission or error code that does not exist.

## Tests
- Package tests (`go test ./...`) are guards, contracts and focused regressions; they
  need no database.
- `ci/` is the end-to-end suite (build tags `e2e,integration`; `scripts/e2e.sh` with
  `OPENRAILS_E2E_DSN` naming a disposable PostgreSQL 18). Every test gets its own schema.
  A deliberate behaviour change must sweep it: `grep ci/` for the codes, constants and
  statuses you changed. Green-in-my-package is not green.
