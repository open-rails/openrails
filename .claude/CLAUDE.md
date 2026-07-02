# OpenRails — project guide for Claude

OpenRails is a multi-merchant billing/payments platform (Go). It runs **standalone**
(hosted SaaS, many merchants) and **embedded** (a host app embeds it for one merchant —
e.g. doujins, cozy-art). One merchant ↔ one controlling org (deliberately 1:1).
The repo is **source-available** — keep secrets and customer/account-specific
identifiers OUT of committed files (code, trackers, this file).

## Money
- All amounts are **micros** (millionths of a currency unit). Not cents, not millicents.
- A double-entry ledger is the source of truth for money; a separate grant ledger tracks
  credit lots. FX is forbidden inside the ledger (no cross-currency transfers).

## Rails (payment providers)
- By repo: doujins / hentai0 → mobius (NMI) + ccbill + solana; cozy-art → stripe; tensorhub → none.
- ALL outbound Stripe HTTP goes through the choke-point client `internal/integrations/stripeapi`
  (readonly mode blocks writes at the transport). It pins the Stripe API version via
  `stripeapi.APIVersion` — ONE const drives both the outbound `Stripe-Version` header AND the
  webhook-endpoint registration. Bump it deliberately (re-run the breaking-change audit); don't float.
- ALL NMI HTTP goes through `internal/integrations/nmi`.

## Rail merchant-account identity
- `openrails.rail_merchant_accounts` (#683; was provider_accounts) is an OPERATOR-DECLARED catalog (manifest `account_id`). There is
  NO runtime "whoami"/identity resolution and NO account-mismatch guard — that whole subsystem was
  ripped out (#592). `account_id` is an opaque, operator-declared label.
- Per rail, the declared `account_id` is:
  - **NMI / Mobius** — the dashboard **"Gateway ID"**. In NMI this IS the *merchant account* id
    (NMI provisions every merchant as a "gateway account"; its v4 API documents `{gateway_id}` as
    "the merchant ID"). It is NOT the reseller/ISO (e.g. MobiusPay). It is NOT fetchable from the
    `security_key` — operator must declare it.
  - **Stripe** — `acct_…` (the one rail that self-discovers, via `GET /v1/account`).
  - **CCBill** — `clientAccnum-clientSubacc`, dash-joined (e.g. `945280-0000`, #697 — never a slash).
  - **Solana** — the recipient wallet address.
  - Don't try to derive any of these from credentials at runtime.

## Catalog
- Declarative provider model. The **pull** reconciliation job
  (`internal/river/jobs_catalog_reconciliation.go`) is ALERT-ONLY — it never mutates providers.
- The **push** path (OpenRails definitions → provider) is the provider adapter — e.g. the Stripe
  adapter `AutoCreate` in `pkg/service/catalog_provider_stripe.go` (find-or-create Product + Price,
  and entitlement Features). Entitlements are plain strings: the keys of `product.EntitlementsSpec`,
  mirrored to Stripe Features (`lookup_key` = the string).

## Schema / DB
- App schema is `openrails` (configurable via execution-time SQL rewrite). RLS is enforced for the
  unprivileged `openrails_app` role, merchant-scoped via the `app.merchant_id` GUC set by MerchantTx.
- Migrations are squashed to a single baseline (`migrations/postgres/001_*`); new migrations start
  at 002. Greenfield — no numbered history to preserve.

## Layer altitude (#688)
- A layer earns its existence by doing work at its own altitude. Modules talk to sqlc `gen` directly;
  repo-style wrappers exist only where they carry logic (tx/locks/mapping/doctrine) and they live IN
  the owning module. Module services do NOT re-export their data surface as one-line forwards.
  Handlers may call `gen` for orchestration-free reads. Never add a wrapper just to "complete" a layer.

## Trackers (issues)
- `agents/{progress,future,completed}.md`. One `# #<id>:` section per issue; `next_id` counter lives
  in progress.md; one per-repo id space shared across all three files.
- CONCURRENT-EDIT SAFE: only ever edit/append YOUR OWN issue's section with a targeted string
  replacement — never rewrite the whole file (another agent may be editing it).

## Tests
- Integration tests: build tag `integration`, run against testcontainers (Postgres + Redis) or
  `OPENRAILS_TEST_DB_URL` / `REDIS_ADDR`. The full suite is self-cleaning and green.
- Known fragility: running a SINGLE integration package in isolation can hit a pre-existing
  `*_merchant_fk` fixture-seeding failure (the merchant isn't seeded for that subset). That's NOT a
  regression — the full suite seeds it correctly.
