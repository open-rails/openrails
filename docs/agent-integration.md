# Integrating OpenRails: guide for AI agents

You are an AI coding agent integrating OpenRails — a self-hostable billing/payments
engine — into a host application. This doc gives you the decision tree, the plan, and
the doc map. Follow it top to bottom.

## Non-negotiable facts

- **Money is an integer in the currency's native units** — micros for USD, per the
  `GET /v1/currencies` registry — sent as a decimal string on the wire
  ([money-wire.md](money-wire.md)). Never pass cents or dollars to an API that
  takes an amount.
- **Entitlements are the access truth.** The host app gates features on active
  entitlements, never by inspecting subscription rows. See
  [entitlements_timeline.md](entitlements_timeline.md).
- **Card data never touches OpenRails or the host.** Checkout is redirect or
  tokenized-vault (SAQ-A). Do not build any flow that posts PAN/CVV to the host.
  The one exception is a PSP the merchant declared `card_entry: server`
  ([docs/rails/nmi.md](rails/nmi.md)), which puts the deployment in SAQ D; never
  enable it on your own.
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

1. **Migrations.** Call `openrails.Migrate` with the pool the engine will use.
   OpenRails owns and applies its billing schema and its River tables.
   Verify: the `billing` schema (or the configured one) exists.
2. **Boot.** `openrails.Config` (explicit `TestMode`,
   `ProviderWriteMode`), `openrails.New` with the host's pgx pool in `Deps`. Verify: boot succeeds;
   a missing posture field refuses to boot (that is correct behavior, not a bug).
3. **Merchant + rails.** Set `Config.Merchant` with the user's sandbox PSP entries
   (per-rail setup: [rails/](rails/)). Verify: boot logs show the rail armed; for NMI
   the sandbox probe passes.
4. **Catalog.** Author products/prices per [merchant-guide.md](merchant-guide.md);
   push at boot. Verify: catalog list routes return the products.
5. **Mount routes.** Pass the host's AuthKit client as `Deps.AuthKit`, or implement
   `Deps.Authenticate` (and `Authorize` for staff routes) over other auth; select
   route groups with an `openrails.Routes`, and mount with
   `openrailshttp.Mount` (or the Gin/Fiber adapter) under a prefix. Verify: an authenticated request to
   `GET <prefix>/v1/me/subscriptions` answers for the caller's own subject.
6. **Backend calls.** Use the Client where the host needs admission/holds, usage,
   or entitlement reads. Verify: `Admit` + `CaptureAdmission` round-trip in a test.
7. **Checkout end-to-end.** Frontend work per
   [frontend-integration.md](frontend-integration.md). Verify: sandbox checkout →
   webhook (use [dev/local-webhooks.md](dev/local-webhooks.md) for a public URL) →
   entitlement active → host feature unlocks. Then verify cancel.

## Plan B — standalone (any host language)

Follow [standalone-integration.md](standalone-integration.md). Milestones:

1. **Deploy.** Compose stack (or real infra per [operator-guide.md](operator-guide.md)).
   Verify: `GET /health/ready` (note: there is no `/health`).
2. **Provision.** Manifest with merchant + sandbox PSPs; `push-auth-bootstrap` →
   `push-merchant-config --insert` → `apply-catalog --merchant NAME --file PATH`.
   Mint an API key. Verify: key works via `client.GetMerchantConfiguration(ctx)` (Go) or an
   authenticated `GET /v1/merchant/configuration` call.
3. **Backend.** Go hosts: root SDK `openrails.NewRemote` + `WithAPIKey`. Other stacks:
   plain HTTP per [api/endpoints.md](api/endpoints.md) and [api/routes.md](api/routes.md).
4. **Frontend.** Access tokens: your identity provider mints DPoP-bound
   `openrails:self` tokens for OpenRails (code flow or token exchange) per
   [frontend-integration.md](frontend-integration.md) / [auth.md](auth.md).
   Never send the host's own session tokens to OpenRails.
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
| API conventions and behavior | [api/endpoints.md](api/endpoints.md) |
| Every HTTP route | [api/routes.md](api/routes.md), `api/openapi.json` |
| Moving from v0 | [migrating-to-v1.md](migrating-to-v1.md) |
| Migrating an existing subscriber base in | [batch-import.md](batch-import.md) |
| Admin console on/off + usage | [admin-console.md](admin-console.md) |
| Day-2 ops, safety levers, cutover | [operator-guide.md](operator-guide.md), [operations.md](operations.md) |
| Vocabulary | [glossary.md](glossary.md) |

## Pitfalls that waste agent time

- Health endpoints are `/health/live` and `/health/ready` — `/health` 404s.
- `/v1/me/*` routes carry no `:user_id`; scope comes from the credential. Do not
  construct user-parameterized paths.
- Do not build a billing proxy in the host app (host route that forwards to
  OpenRails billing routes). Embedded mounts the routes; standalone is called
  browser-direct with access tokens.
- Checkout endpoints are tightly rate-limited by design; retry loops in tests will
  hit 429 — back off, don't raise limits.
- Embedded config is programmatic: no file or environment is read, so nothing is
  defaulted for you; unset `TestMode` refusing to boot is the designed behavior.
- Catalog amounts are integers in native units (`12_000_000` = $12). No dollar
  strings in the catalog document.
