# E2E proof — gated-premium-page against a real local stack + real NMI sandbox

Proven 2026-07-28 (tracker #825): the full six-step demo runs end-to-end against
`task docker-up`'s stack and the real NMI sandbox account — including the REAL
browser flow (Collect.js tokenized in headless Chromium via Playwright; charge
approved synchronously by NMI; `/premium` unlocked; no inbound webhook needed).

## Run it

Prereqs: repo-root `.env` with `NMI_SANDBOX_SECURITY_KEY`, `NMI_TOKENIZATION_KEY`
(and optionally `NMI_WEBHOOK_SIGNING_SECRET`, `NMI_TOKENIZATION_URL`,
`NMI_ACCOUNT_ID`); docker; `jq`; Go.

```bash
cd examples/gated-premium-page
bash e2e/provision.sh   # stack up + merchant + PSP creds + catalog + NMI plan + API key
bash e2e/run-e2e.sh     # scripted six-step proof (server-side-token variant)
```

`e2e/provision.sh` leaves state in `e2e/.state/` (issuer key, manifests,
compose override, `apikey.env`). Re-running is idempotent; each run
version-bumps the price onto a fresh randomized-amount NMI plan so repeat
purchases dodge NMI's duplicate-transaction window. Teardown: `task docker-down`.

For the interactive demo (real browser, real Collect.js):

```bash
set -a; source ../../.env; source e2e/.state/apikey.env; set +a
ISSUER_KEY_FILE=e2e/.state/issuer_key.pem go run .
# open http://localhost:8080, log in, buy with 4111 1111 1111 1111 / 10/29
```

## What is scripted vs manual

- **Scripted** (`run-e2e.sh`): steps 1–6 with ONE substitution — browser
  Collect.js tokenization is replaced by its server-side equivalent (vault the
  card directly at NMI + insert the payment-method row, the same pattern as
  `tests/nmi_live_lifecycle_e2e_test.go`), then the REAL
  `POST /v1/me/checkout` runs with `payment_method_id`. Plus a step 7: remote
  verification at NMI (`query.php`).
- **Manual/browser** (works headlessly under Playwright, verified): the actual
  `/buy` page — Collect.js iframes accept the sandbox card, the one-time
  `payment_token` goes to `/v1/me/checkout`, NMI approves synchronously,
  `/premium` serves 200. Renewals are the only part that needs the NMI webhook
  (see README).

## Deviations from the README quickstart (all forced by engine gaps)

Verified against commit `fc3ebc41`; each has a precise pointer.

1. **`openrails run-server` never loads the MODE-1 boot merchant manifest.**
   `reconcileBootMerchantManifest` exists only in
   `internal/bootstrap/serverboot/serverboot.go:109` (used by the test
   harness); the shipped CLI path (`cmd/openrails/main.go:runServer` →
   `embedded.New` → `StandaloneHandler`) applies only the AuthKit
   `bootstrap.yaml` (`cmd/openrails/bootstrap_apply.go:applyStartupBootstrap`,
   whose comment says merchant config is "never reconciled from normal server
   startup"). docs/self-hosting-mode1.md ("Loaded by standalone server boot,
   every boot") and docs/standalone-integration.md ("step 2 is simply mount
   the file and boot") describe behavior the binary does not implement.
   Consequence: MODE-1 standalone can never arm PSP secrets — the CLI push
   explicitly discards them ("secrets validate in memory only and are NOT
   persisted"). **Workaround**: the server runs `MERCHANT_SOURCE=api`; the
   push CLIs run `MERCHANT_SOURCE=manifest` (they refuse under api — while
   docs/merchant-provisioning.md claims the push CLIs seed MODE-2 stores, see
   `cmd/openrails/bootstrap_apply.go:163`); NMI credentials are then seeded
   through `PUT /v1/merchant/payment-providers/nmi` into the MODE-2 DB store.
2. **Checkout rejects PSP-key wire values — FIXED (#848).** `checkoutRailUsable`
   (`internal/http/handlers/checkout_session.go`) now resolves through
   checkout's `resolveRailTarget`, which matches a declared PSP key first and
   falls back to a rail kind only when that is unambiguous. The frozen PSP
   vocabulary (`mobius-sandbox`) is accepted on the wire, and since #829 the
   example no longer guesses it at all: `GET /v1/checkout-config` returns the
   PSP key to send.
3. **Docker image does not build.** The console stage crashes:
   `ERR_PNPM_VERIFY_DEPS_BEFORE_RUN opts2.currentPnpmfiles is not iterable`
   (pnpm 11.0.0 + `verifyDepsBeforeRun: error` in `web/admin/pnpm-workspace.yaml`,
   tripped by `COPY web/admin/ ./` after `pnpm install`). So `task docker-up`
   cannot build the image. **Workaround** (build env only): flip
   `verifyDepsBeforeRun: false` inside the console build stage before
   `pnpm run build`.
4. **`merchants.api_host` has no API/manifest/CLI surface** (Go-only
   `merchants.Service.SetHostConfig`), yet the public catalog
   (`GET /v1/products`, which `/buy` fetches) resolves the merchant from the
   request Host and 500s until it is set. **Workaround**: provision.sh applies
   the same row update via psql (`api_host='localhost'`).
5. **Issuer-key rotation via the CLI needs a server restart** — the delegated
   verifier caches registered issuer keys in process memory; a key
   (re)registered by `push-merchant-config` from another process is not
   picked up live.
6. **Catalog `psps: [mobius-sandbox]` alone cannot sell on NMI.** NMI is
   link-only: without `psp_links.<psp>.plan_id` the price lands
   `pending_manual_link` and checkout fails
   ("missing NMI plan configuration", `requireNMIPlanForTarget`,
   `internal/modules/checkout/service.go`). provision.sh creates the sandbox
   plan (`recurring=add_plan`) and links it.

## What was proven, step by step

1. `GET /` → 200 (public page).
2. `GET /login` → signed demo session cookie.
3. `GET /premium` with no entitlement → 302 `/buy`
   (`openrails.ListActiveEntitlements` over the SDK, API-key auth).
4. Purchase, twice:
   - server-side-token variant → NMI approved synchronously
     (e.g. txn `12356389049`, $5.23);
   - REAL browser (headless Chromium + Collect.js) → NMI approved
     (txn `12356402185`, $6.87), page redirected to `/premium`.
   A same-amount repeat inside NMI's duplicate window correctly surfaced as a
   declined checkout + failed payment row.
5. `GET /premium` → 200, "Premium unlocked".
6. Self surface: `/v1/me/subscriptions` → status `active`;
   `/v1/me/entitlements/active` → `premium` (source: the subscription).
   Merchant surface (API key): `/v1/merchant/payments` shows the succeeded
   payment with the NMI transaction id; `/v1/merchant/customers/{id}/entitlements`
   and `/v1/merchant/entitlements/premium/customers` both show the grant.
7. Remote state at NMI (`query.php`): transaction present
   (`pendingsettlement`, exact amount) and the recurring subscription exists
   (`rail_subscription_id` matches).

## Not provable headlessly

Nothing in the six-step demo. Only **renewals** (NMI rebill webhooks) need an
inbound tunnel — out of scope here, see docs/dev/local-webhooks.md.
