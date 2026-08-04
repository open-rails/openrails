# NMI (high-risk gateway) setup

> Which flows are supported on this rail, and how well each one is verified:
> [rail certification matrix](certification-matrix.md).

### What NMI is, and where your ISO fits

NMI is white-label gateway software. High-risk ISOs/resellers — MobiusPay,
PaymentCloud, PayKings, SoarPay, Zen Payments, Corepay, and many others — sell
merchant accounts that run on NMI's gateway. You sign a contract with the ISO,
but the API, dashboard, and credentials are NMI's. OpenRails speaks to the NMI
gateway directly; the ISO is a sales/underwriting layer you will mostly never
touch again after onboarding.

In OpenRails vocabulary: `nmi` is the **rail** (the gateway kind); each NMI
merchant account you hold is a **PSP** entry named whatever you like (`mobius`,
`paykings`, ...). One merchant can declare several NMI PSPs. See
`docs/glossary.md`.

All NMI HTTP goes through one client, `internal/integrations/nmi`. It talks to:

- `https://secure.nmi.com/api/v5` — the v5 JSON API (most operations)
- `https://secure.networkmerchants.com/api/transact.php` — classic Direct Post
  (a few recurring operations v5 cannot do; see quirks)
- `https://secure.nmi.com/api/query.php` — Query API (transaction search)

### Credentials to collect from your ISO

Three values, all found in (or issued for) the NMI merchant dashboard:

1. **Security key** (`security_key`) — the private API key. It is also the v5
   API credential (no separate key needed). Treat it like a Stripe secret key.
2. **Tokenization key** (`tokenization_key`) — the *public* Collect.js key.
   Your checkout page loads Collect.js with it; the raw card goes browser →
   NMI, and OpenRails only ever receives the opaque `payment_token` (SAQ-A).
3. **Gateway ID** (`account_id`) — shown in the dashboard as "Gateway ID".
   In NMI this **is** the merchant account id (NMI provisions every merchant as
   a "gateway account"; the v4 API documents `{gateway_id}` as "the merchant
   ID"). It is **not** the ISO/reseller's id, and it is **not** fetchable from
   the security key — you must read it off the dashboard and declare it.

### The PSP manifest entry

Declare the account under your merchant in the config manifest
(`config/merchants_config.example.yaml` has the full example):

```yaml
merchants:
  local-stack:
    psps:
      mobius:                 # your name for this PSP (any slug)
        nmi:                  # the rail
          environment: live   # assertion cross-checked against test_mode
          account_id: "1234567"  # dashboard "Gateway ID"
          settings:
            tokenization_url: https://secure.networkmerchants.com/token/Collect.js
            tokenization_key: replace-with-live-nmi-tokenization-key   # public
          secrets:
            security_key: replace-with-live-nmi-security-key
            webhook_signing_secret: replace-with-live-nmi-webhook-secret
```

Store real secret values in Vault (or the encrypted DB store) and overlay them;
never commit them. `environment: test|live` does not select behavior — the
deployment-level `test_mode` does — it is an assertion, and a contradiction
refuses boot.

### Webhook registration

In the NMI dashboard, register a webhook endpoint pointing at your OpenRails
deployment:

- URL: `https://<your-host>/v1/webhooks/nmi`
  (a merchant-scoped alias `/v1/merchants/<slug>/webhooks/mobius` also exists)
- Signing secret: exactly the value you declared as `webhook_signing_secret`

OpenRails verifies every delivery: HMAC-SHA256 over `<timestamp>.<body>` from a
`Webhook-Signature: t=<unix-ts>,s=<hex>` header, with a 5-minute replay window.
Unsigned or mis-signed deliveries are rejected, and an account with no
`webhook_signing_secret` configured rejects all webhooks.

Enable these event types (what the handler consumes):

- `recurring.subscription.add` / `.update` / `.delete`
- `transaction.sale.success` / `.failure`
- `transaction.refund.success` / `.failure`
- `transaction.void.success` / `.failure`
- `chargeback.batch.complete` — auto-reconciled: refund recorded, subscription
  cancelled
- `acu.summary.*` (Automatic Card Updater) — received and logged

Subscription-state events are treated as wake-up signals only: OpenRails marks
the subscription dirty and converges from freshly *fetched* gateway truth, so a
lost or reordered webhook cannot corrupt state.

For local development, tunnel a stable public hostname to your dev server —
see `docs/dev/local-webhooks.md`.

### Sandbox testing

Ask your ISO for a **sandbox/test gateway account** — most provision one on
request. Declare it as its own PSP entry (`mobius-sandbox` in the example
manifest) with `environment: test`, and run the deployment with
`test_mode: sandbox` (env `TEST_MODE=sandbox`).

An NMI sandbox is otherwise undetectable: same URLs, and the security key
carries no test marker (unlike Stripe's `sk_test_` prefix). So under
`test_mode: sandbox` OpenRails **probes each NMI account** before arming it:
one authorization on the canonical test card — a non-issued PAN no real
processor can approve. A simulator approves it (probe auth is then voided); a
decline proves live credentials and OpenRails **refuses to arm the account**
rather than move real money in a test deployment. Verdicts are cached for 12
hours, so back-to-back boots don't re-probe. The probe is harmless on a live
account (one declined auth, no money movement).

Sandbox test card: `4111 1111 1111 1111`, expiry `10/29`. Enter it
only into Collect.js fields — the E2E harness (`task e2e-nmi-live`) drives the
full flow: browser tokenization, vault save, one-off sale, subscription
enrollment, signed webhooks, remote query, cancel. Sandboxes generally cannot
advance time; for rebill testing create a 1-day plan and wait.

### Quirks worth knowing

Verified against the live gateway (the docs at docs.nmi.com diverge in places);
OpenRails already routes around all of these, listed so gateway behavior
doesn't surprise you:

- **v5 auth is the bare key.** The security key is sent as the entire
  `Authorization` header value — no `Bearer`/scheme.
- **Classic Direct Post survivors.** Subscription enrollment stays on
  `recurring=add_subscription` (atomic first-charge + enroll + delayed start;
  v5 has no equivalent), as do manual rebills and subscription updates — the
  documented `PATCH /v5/subscriptions/{id}` returns `E_ROUTE_NOT_FOUND` on the
  live gateway. Transaction search stays on `query.php` (v5 has no
  list/search).
- **Cancelled subscriptions are deleted.** NMI tombstones cancelled recurring
  records; `GET /v5/subscriptions/{id}` answers 404. OpenRails treats "gone at
  NMI" as a terminal state, not an error.
- **Duplicate-transaction window.** The gateway rejects a repeat of the same
  card + amount + order_id within its duplicate window. OpenRails randomizes
  probe amounts; the E2E harness randomizes test amounts for the same reason.
- **"Processor" in webhook payloads** (`processor_id`,
  `transaction_was_declined_by_processor`) is NMI's backend *acquirer*, not
  anything you configure in OpenRails.
- **Large refunds cancel.** A refund ≥ 80% of the subscription price
  terminates the subscription and revokes access, matching the CCBill rail's
  behavior.
