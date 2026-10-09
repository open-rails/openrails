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

All NMI HTTP goes through one client. It talks to:

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
   Not needed with `card_entry: server` (below).
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
        rail: nmi
        account_id: "1234567"  # dashboard "Gateway ID"
        settings:
          tokenization_url: https://secure.networkmerchants.com/token/Collect.js
          tokenization_key: replace-with-live-nmi-tokenization-key   # public
        secrets:
          security_key: replace-with-live-nmi-security-key
          webhook_signing_secret: replace-with-live-nmi-webhook-secret
```

Store real secret values in Vault (or the encrypted DB store) and overlay them;
never commit them. A PSP declares no environment: the deployment-level
`test_mode` decides, and every PSP in the deployment follows it.

### Card entry: browser or server

Where a new card is typed is a PSP setting, `settings.card_entry`:

- `browser` (the default): Collect.js fields on your page send the card to
  NMI; OpenRails receives a single-use `payment_token`. Your servers never
  see a card number: PCI **SAQ A**.
- `server`: your page posts the card to OpenRails, which vaults it at NMI.
  Checkout options advertise driver `card`, billing-ui renders plain card
  inputs, and no NMI script loads in the browser. The checkout, saved-card
  and card-update bodies take
  `"card": {"number", "exp_month", "exp_year", "cvc"}` (never beside
  `payment_token`).

`server` puts the servers that receive the request — your proxy, the host
application embedding OpenRails, OpenRails — in PCI scope: **SAQ D**, with
its network segmentation, logging, and quarterly scan obligations. Declare it
only with that assessment in hand.

What OpenRails does in `server` mode:

- The card is vaulted with the classic Customer Vault call
  (`customer_vault=add_customer` / `add_billing`, `ccnumber`, `ccexp`, `cvv`)
  under a vault id and billing id OpenRails derives from a durable operation.
  Charges, subscriptions and rebills then use the vault exactly as for a
  token-vaulted card.
- Nothing about the card is stored except brand, last four and expiry: no
  number or security code, hashed or not, in any table, operation record,
  idempotency record, log line or error. The card is one type that prints
  and encodes as `[card]` and is wiped once NMI has it.
- A vault call whose answer is lost is settled by reading the vault id the
  operation named; if NMI does not show it, the buyer enters the card again.
  A card is never sent twice.
- In live posture the `card` field is refused over plain HTTP: TLS must reach
  OpenRails, or a trusted proxy (`trusted_proxies`) must say
  `X-Forwarded-Proto: https`.
- Every other request field is still scanned for card numbers, and a PSP left
  on `browser` refuses `card` outright.

```yaml
      mobius:
        rail: nmi
        account_id: "1234567"
        settings:
          card_entry: server
```

Boot refuses `card_entry: server` on any rail other than NMI and on a PSP
whose cards a custodian holds.

### Catalog prices

NMI prices are engine terms: no NMI plan is created or required.

- One-time: a direct gateway sale on the Collect.js token or saved vault
  card. Declaring the PSP on a one-time price (`psps: [mobius]`) needs no
  link.
- Recurring: OpenRails charges each renewal from the vaulted card (manual
  rebill). An explicit `plan_id` link is accepted only for provider-owned
  legacy schedules, and must match the price.

A price is offered on NMI when the PSP is armed.

### Webhook registration

In the NMI dashboard, register a webhook endpoint pointing at your OpenRails
deployment:

- URL: `https://<your-host>/v1/webhooks/nmi/{account_id}` — the rail, not the PSP key, then
  the PSP's Gateway ID; the payload's Gateway ID must agree with it
- Signing secret: exactly the value you declared as `webhook_signing_secret`

OpenRails verifies every delivery: HMAC-SHA256 over `<timestamp>.<body>` from a
`Webhook-Signature: t=<unix-ts>,s=<hex>` header, with a 5-minute replay window.
Unsigned or mis-signed deliveries are rejected, and an account with no
`webhook_signing_secret` configured rejects all webhooks.

Enable these event types (what the handler consumes):

- `recurring.subscription.add` / `.update` / `.delete`
- `transaction.sale.success` / `.failure` / `.unknown`
- `transaction.refund.success` / `.failure`
- `transaction.void.success` / `.failure`
- `chargeback.batch.complete` — auto-reconciled: refund recorded, subscription
  canceled
- `acu.summary.automaticallyupdated` / `.closedaccount` / `.contactcustomer`
  (Automatic Card Updater). An updated card is re-read from the vault, and
  memberships waiting on it retry at the next due pass. A card reissued under
  another brand instead loses its stored-credential agreements: it is charged
  off-session only after a customer-initiated charge anchors one again, and its
  members are asked to act. A closed account parks the card. Both closed and
  contact-customer ask the member for a new card. Each notice is recorded once
  in `payment_method_updates`.

Subscription-state events are treated as wake-up signals only: OpenRails marks
the subscription dirty and converges from freshly *fetched* gateway truth, so a
lost or reordered webhook cannot corrupt state.

Attempt details come from the Query API too. Every hour, NMI attempts at least
10 minutes old are filled from the transaction report, 50 transaction ids per
query and each attempt once: card BIN and brand, AVS/CVV the reply lacked, the
issuer's raw answer (`processor_response_code`) and whether a network token was
used. Attempts found by a provider read are filled from that read at once.

NMI's own history is kept too, as monthly aggregates per NMI PSP
(`nmi_history_months`): the numbers `openrails nmi decline-report` prints,
through the same read. Once a day each PSP's transaction report is read, one
calendar month per query in pages of 1000: 25 months the first time, then from
the month before the last read. A read replaces the months it covers in one
transaction; a failed one keeps them and is tried again the next hour. It only
reads NMI. The history cannot tell initial sales from retries of declined
rebills, so one-off sales mix both; scheduled rebills are NMI's own charges.
Payments → Health shows it beside OpenRails' own attempts.

For local development, tunnel a stable public hostname to your dev server —
see `docs/dev/local-webhooks.md`.

### Sandbox testing

Run the deployment with `test_mode: sandbox` (env `TEST_MODE=sandbox`). Each
NMI PSP then declares which NMI deployment its credentials belong to, in
`settings.endpoint_deployment` (explicit; OpenRails never tries both):

- `gateway` — a regular gateway account switched to test mode
  (`secure.nmi.com` / `secure.networkmerchants.com`). Verified with the
  read-only Query API `report_type=test_mode_status`; only a single
  well-formed `test_mode_enabled=true` arms it. Never a financial request.
- `sandbox` (default when omitted) — NMI's dedicated sandbox service
  (`sandbox.nmi.com`). NMI documents no read-only test-mode signal there, so it
  is verified with one authorization on the non-issued test card: a simulator
  approves it (then voided), a live account declines it.

```yaml
      mobius:
        rail: nmi
        account_id: "1234567"           # dashboard Gateway ID
        settings:
          endpoint_deployment: gateway  # or sandbox
          tokenization_url: https://secure.networkmerchants.com/token/Collect.js
          tokenization_key: public-collectjs-key
        secrets:
          security_key: ...
          webhook_signing_secret: ...
```

Verification runs once per loaded credential set (startup, credential
create/rotate), bound to merchant + PSP + endpoint + credential fingerprint.
Every NMI client (checkout, card save, rebills, refunds, pulls,
custodian proxies) is built from that same PSP scope, so all of them share the
one verdict; see [sandbox posture](../sandbox-posture.md). A false, unknown or
unavailable verdict disarms the PSP: every NMI mutation is refused before
anything is sent, reads still work, and the `openrails_psp_posture` probe fails
until a background retry succeeds.

Sandbox test card: `4111 1111 1111 1111`, expiry `10/29`. Enter it
only into Collect.js fields. Sandboxes generally cannot advance time, so
scheduling behavior is proven with deterministic transports, not by waiting
for a provider billing period.

Your own end-to-end tests need no NMI account: package
`openrailstest/nmimock` is a fake gateway on loopback. Set
`Config.ProviderSandbox.NMIGatewayURL` to its `URL()` under
`TestMode: openrails.Sandbox`, declare an NMI PSP with any credentials, and
give the gateway the clock you pass as `Deps.Clock`, so a renewal is a clock
step. `Tokenize(card)` is the Collect.js token a customer saves or pays with;
`SetDecline(last4, nmimock.InsufficientFunds)` declines that card's next
charge, which sends a renewal into dunning; `Ledger`, `Charged`, `LastSale`
and `LastDecline` read what it was charged. The package comment has the
wiring.

### Quirks worth knowing

Verified against the live gateway (the docs at docs.nmi.com diverge in places);
OpenRails already routes around all of these, listed so gateway behavior
doesn't surprise you:

- **v5 auth is the bare key.** The security key is sent as the entire
  `Authorization` header value — no `Bearer`/scheme.
- **Classic Direct Post survivors.** Subscription enrollment stays on
  `recurring=add_subscription` (atomic first-charge + enroll + delayed start;
  v5 has no equivalent). Stored-card sales and manual rebills also use Classic
  so every authorization carries NMI's `initiated_by` and
  `stored_credential_indicator`; every subsequent CIT/MIT additionally carries
  its agreement's initial NMI transaction ID whenever OpenRails captured it.
  Implicit incomplete combinations fail before network I/O. Subscription updates stay on Classic because the
  documented `PATCH /v5/subscriptions/{id}` returns `E_ROUTE_NOT_FOUND` on the
  live gateway. Transaction search stays on `query.php` (v5 has no list/search).
- **Every later charge names its first.** A merchant-initiated charge carries
  the agreement's initial NMI transaction id; a card with no recorded anchor is
  not charged off-session (an import raises `life.import.no_recurring_anchor`).
  A renewal stops for the member and an invoice's collection stops with failure
  code `stored_credential_required` until a customer-initiated charge anchors
  one. Replacing a card keeps only the recurring agreement its verification
  anchored.
- **Confirm recurring classification per processor.** OpenRails sends
  `billing_method=recurring` on the initial recurring CIT and later recurring
  charges, matching NMI's Credential on File guide. The Classic API reference
  also documents `initial_recurring` and says processor support varies; obtain
  written confirmation from the ISO/acquirer before changing this value for a
  merchant account.
- **Canceled subscriptions are deleted.** NMI tombstones canceled recurring
  records; `GET /v5/subscriptions/{id}` answers 404. OpenRails treats "gone at
  NMI" as a terminal state, not an error.
- **Duplicate-transaction window.** The gateway rejects a repeat of the same
  card + amount + order_id within its duplicate window. OpenRails randomizes
  probe amounts for that reason.
- **"Processor" in webhook payloads** (`processor_id`,
  `transaction_was_declined_by_processor`) is NMI's backend *acquirer*, not
  anything you configure in OpenRails.
- **Dashboard refunds follow `provider_refund_access`** (merchant settings;
  same rule on every rail): `revoke_on_full` (default) ends the charge's access
  once it is fully refunded, `revoke_on_any` on any refund, `keep` never. An
  NMI-billed membership that ends this way also has its NMI schedule deleted
  (held, with a `life.provider_cancel.held` finding, while destructive actions
  are disarmed). Refunds made through OpenRails follow their `revoke_access`.

### Duplicate-transaction refusals

NMI refuses a sale or verification whose card and amount match one it just
processed (`response=3`, code 300, "Duplicate transaction"), whatever the order
id. The refused request charged nothing: tier-change prorations, one-off sales
and card saves answer `409 payment_duplicate_refused` (try again in a few
minutes), a customer-present enrollment fails, and a replacement-card
verification retries on its own. A scheduled renewal or dunning recovery is
different: the matching charge may be this period's payment (for an
NMI-scheduled subscription, NMI's own schedule), so it stays unknown and is
never re-sent. An engine renewal completes if its own order shows the charge;
otherwise it raises `life.submission.unresolved`, naming the vault's matching
charges under other orders. A recovery waits for the operator
(`openrails intents resolve`).

### Exactly-once renewals

Every attempt to pay one period (subscription + period) shares one NMI order
id, so a read by order answers "was this period paid?" across attempts; a
later attempt reads it first and completes from an earlier attempt's charge
without sending. The idempotency key stays per attempt.

A submitted engine renewal with no receipt is re-sent only when, after a
five-minute settle, both the order read and a vault-wide read (any
transaction on the `customer_vault_id`, any order or status, since the
submission) are empty. The resend reuses the order and sends `dup_seconds`
covering the time since the first submission, so NMI refuses it if the
original charged but was not yet searchable. Any unreadable or contradictory
read means no resend and a `life.submission.unresolved` finding.

Immediately before every charge request (engine renewals on NMI and Stripe,
dunning recoveries), the executor re-reads its claim on a fresh connection:
the same claim, with a lease still running. A lost claim sends nothing and
returns the operation to verification. This narrows, but cannot close, the
window of a replica that stalls after that check (NMI has no remote fence):
such a late request is caught only by the account's own duplicate window, so
keep NMI's duplicate check on.

A submitted charge with no receipt is settled only from NMI's record under its
order: a definitive decline ends it unpaid, and an empty search proves nothing.
Until NMI shows an outcome, a `life.tier_change.proration_unresolved` finding
names `openrails intents resolve --intent <id> --step proration --receipt
<transaction id>`; `--not-executed` is refused.

### Verifying unverified subscriptions

A schedule whose renewal OpenRails has not seen (its period lapsed with no
charge or decline recorded) becomes `unverified`, with access kept. It is read
from NMI as soon as that commits:

- Up to 50 schedules per request: one Query API recurring report and one
  transaction query (`subscription_id` takes a comma-separated list), plus a
  v5 read only for a schedule the report no longer lists.
- Above 200 unverified schedules (typically right after an import), the
  account is read in bulk: one paged v5 roster read and a paged transaction
  read by date range. Each transaction page is recorded, and the pass resumes
  from its last page after a crash.

A payment renews exactly the periods it paid, a decline enters dunning, and a
schedule NMI ended is canceled. A row with no evidence stays `unverified`. The
`life.unverified.backlog` finding reports each account's count and oldest age;
a row still unverified after three days raises `life.unverified.unresolved` for
the operator.
