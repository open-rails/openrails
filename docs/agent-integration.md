# Integrating OpenRails: guide for AI agents

You are an AI coding agent integrating OpenRails — a self-hostable billing/payments
engine — into a host application. This doc gives you the decision tree, the plan, and
the doc map. Follow it top to bottom.

## Non-negotiable facts

- **Money is micros** (millionths of a currency unit) everywhere in OpenRails. Never
  pass cents or dollars to an API that takes an amount.
- **Entitlements are the access truth.** The host app gates features on active
  entitlements, never by inspecting subscription rows. See
  [entitlements_timeline.md](entitlements_timeline.md).
- **Card data never touches OpenRails or the host.** Checkout is redirect or
  tokenized-vault only (SAQ-A). Do not build any flow that posts PAN/CVV to the host
  or to OpenRails.
- **Sandbox first.** All development runs `test_mode = sandbox` — every rail routes to
  its test environment and live credentials refuse to boot. Do not touch live
  credentials until the full flow is proven.
- **Vocabulary:** a *rail* is the gateway kind (`nmi`/`ccbill`/`stripe`/`solana`); a
  *PSP* is the merchant's account on a rail (manifest key `psps:`). Config keys named
  `accounts:`/`rail_merchant_accounts` are retired and fail loudly.

## Step 0 — decisions to confirm with the user

Do not guess these; ask:

1. **Mode.** Is the host app Go, and is one binary preferred? → **embedded** (engine
   in-process). Otherwise, or if multiple services/languages need billing → **standalone**
   (separate HTTP service).
2. **Rails.** Which of NMI-backed gateway / Stripe / CCBill / Solana, and does the user
   already have credentials (sandbox and live)?
3. **What is sold.** Recurring subscriptions, one-time purchases, metered usage/credits,
   or a mix — this shapes the catalog and whether the admission/hold API is needed.
4. **Frontend.** Which app renders billing UI, and does the user want the merchant
   admin console enabled?

## Plan A — embedded (Go host)

Follow [embedded-integration.md](embedded-integration.md) section by section. The
milestone order, each verifiable before the next:

1. **Migrations.** Apply OpenRails' `migrations/postgres` FS from the host's migration
   step (migratekit). Verify: the `openrails` schema exists.
2. **Boot.** Programmatic `config.Config` (explicit `Env`, `TestMode`,
   `ProviderWriteMode`), `embed.New` with the host's pgx pool. Verify: boot succeeds;
   a missing posture field refuses to boot (that is correct behavior, not a bug).
3. **Merchant + rails.** `rt.UpsertMerchantConfig` with the user's sandbox PSP entries
   (per-rail setup: [rails/](rails/)). Verify: boot logs show the rail armed; for NMI
   the sandbox probe passes.
4. **Catalog.** Author products/prices per [merchant-guide.md](merchant-guide.md);
   push at boot. Verify: catalog list routes return the products.
5. **Mount routes.** Implement `billingauth` authenticators mapping the host's existing
   auth; mount `MountHandler` under a prefix. Verify: an authenticated request to
   `GET <prefix>/v1/me/status` returns the caller's own subject.
6. **Backend calls.** Wire `rt.Client()` where the host needs admission/holds, usage,
   or entitlement reads. Verify: `AdmitBatch` + `Capture` round-trip in a test.
7. **Checkout end-to-end.** Frontend work per
   [frontend-integration.md](frontend-integration.md). Verify: sandbox checkout →
   webhook (use [dev/local-webhooks.md](dev/local-webhooks.md) for a public URL) →
   entitlement active → host feature unlocks. Then verify cancel.

## Plan B — standalone (any host language)

Follow [standalone-integration.md](standalone-integration.md). Milestones:

1. **Deploy.** Compose stack (or real infra per [operator-guide.md](operator-guide.md)).
   Verify: `GET /health/ready` (note: there is no `/health`).
2. **Provision.** Manifest with merchant + sandbox PSPs; `push-auth-bootstrap` →
   `push-merchant-config --insert` → `push-merchant-catalog --insert --overwrite`.
   Mint an API key. Verify: key works via `openrails.Verify` (Go) or an authenticated
   `GET /v1/merchant/*` call.
3. **Backend.** Go hosts: root SDK `openrails.NewRemote` + `WithAPIKey`. Other stacks:
   plain HTTP per [api/endpoints.md](api/endpoints.md).
4. **Frontend.** Delegated tokens: add ONE token-exchange endpoint to the host API,
   mint per [frontend-integration.md](frontend-integration.md) /
   [auth.md](auth.md). Never send the host's own session tokens to OpenRails.
5. **Webhooks + end-to-end.** Rails point directly at OpenRails. Verify the same
   checkout → webhook → entitlement loop as Plan A step 7.

## Doc map

| Need | Doc |
|---|---|
| Full embedded guide | [embedded-integration.md](embedded-integration.md) |
| Full standalone guide | [standalone-integration.md](standalone-integration.md) |
| Browser/UI work | [frontend-integration.md](frontend-integration.md) |
| Why the auth model is shaped this way | [auth.md](auth.md) |
| Per-rail credentials/webhooks/sandbox | [rails/nmi.md](rails/nmi.md), [rails/stripe.md](rails/stripe.md), [rails/ccbill.md](rails/ccbill.md), [rails/solana.md](rails/solana.md) |
| Catalog authoring (products/prices/entitlements) | [merchant-guide.md](merchant-guide.md) |
| Every HTTP route | [api/endpoints.md](api/endpoints.md) |
| Admin console on/off + usage | [admin-console.md](admin-console.md) |
| Day-2 ops, safety levers, cutover | [operator-guide.md](operator-guide.md), [operations.md](operations.md) |
| Vocabulary | [glossary.md](glossary.md) |

## Pitfalls that waste agent time

- Health endpoints are `/health/live` and `/health/ready` — `/health` 404s.
- `/v1/me/*` routes carry no `:user_id`; scope comes from the credential. Do not
  construct user-parameterized paths.
- Do not build a billing proxy in the host app (host route that forwards to
  OpenRails billing routes). Embedded mounts the routes; standalone is called
  browser-direct with delegated tokens.
- Checkout endpoints are tightly rate-limited by design; retry loops in tests will
  hit 429 — back off, don't raise limits.
- Embedded config is programmatic: `config.Load` never runs, so nothing is defaulted
  for you; unset `TestMode` refusing to boot is the designed behavior.
- Catalog amounts are integer micros (`12_000_000` = $12). No dollar strings in the
  catalog manifest.
