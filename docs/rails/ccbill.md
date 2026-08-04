# CCBill

> Which flows are supported on this rail, and how well each one is verified:
> [rail certification matrix](certification-matrix.md). Read it before relying on
> CCBill refunds — that wire is modeled, not verified.

CCBill is a hosted-checkout payment processor commonly used by high-risk and
adult/subscription businesses. In OpenRails it is a **reserved gateway**: the
rail and the PSP name are both `ccbill` (unlike NMI, where a PSP gets its own
name such as `mobius`). CCBill owns the card vault and the rebill schedule —
OpenRails never sees card data; it redirects the buyer to a CCBill FlexForm
and consumes webhooks.

### Account structure

CCBill identifies a merchant by a **client account number** (`clientAccnum`,
e.g. `999999`) plus a **subaccount** (`clientSubacc`, e.g. `0000`). OpenRails
declares this pair once, as the dash-joined `account_id`:

```
account_id: "999999-0000"   # clientAccnum-clientSubacc — dash, never a slash
```

Config validation rejects a slash and derives the pair by splitting on the
first dash; there is no separate accnum/subacc setting.

From CCBill (dashboard or merchant support) you need:

- `clientAccnum` + `clientSubacc`
- the **salt** used to sign FlexForm URLs
- a **DataLink** username/password (optional — enables merchant-initiated
  cancels/refunds and reconciliation; without it those paths cannot execute)
- per price: a **FlexForm** (`flex_id` + `form_name`) created in CCBill's
  FlexForms admin with matching pricing

### PSP manifest entry

Under `merchants.<slug>.psps` (env overlay `BILLING_MERCHANTS_<M>_PSPS_…`;
secret-store prefix `psps/…`):

```yaml
psps:
  ccbill:               # PSP key — reserved gateway, must be "ccbill"
    ccbill:             # rail
      environment: live # or "test"
      # clientAccnum-clientSubacc, dash-joined:
      account_id: "999999-0000"
      secrets:
        salt: replace-with-ccbill-flexform-salt
        # Optional pair — declare both or neither:
        datalink_username: replace-with-ccbill-datalink-username
        datalink_password: replace-with-ccbill-datalink-password
```

That is the whole surface: no API key, no webhook signing secret (CCBill has
no webhook HMAC — see below), and no per-merchant IP allowlist.

### Catalog: linking prices to FlexForms

CCBill prices are **link-only** — OpenRails never auto-creates provider
objects on CCBill. Each price that sells over CCBill carries the FlexForm it
maps to:

```yaml
prices:
  - currency: usd
    unit_amount: 9990000        # micros ($9.99)
    duration: 30d
    auto_renew: true
    psps: [ccbill]
    psp_links:
      ccbill:
        flex_id: "d7d6d9a5-..." # FlexForm GUID from CCBill admin
        form_name: "premium"
```

The FlexForm's own pricing must match the catalog price: webhooks validate
billed amounts against the catalog (2% tolerance) and reject a
`flexId`/`formName` that doesn't match the price's link.

### Checkout flow

CCBill is redirect-only and **subscription-only** (one-time purchases are
rejected). `POST /v1/checkout` (or `/v1/me/checkout`) with
`payment.rail: "ccbill"` plus billing details (`email`, `first_name`,
`last_name`, `address1`, `city`, `state`, `zip`, `country`) returns
`requires_action` with a redirect URL:

```
https://api.ccbill.com/wap-frontflex/flexforms/{flex_id}?clientAccnum=…&clientSubacc=…&formName=…&username=…&email=…&reservationId=…&signature=…
```

- The buyer must have a **verified email and a username** — the webhook
  resolves the user by username.
- `reservationId` carries the OpenRails checkout-session id; the
  `NewSaleSuccess` webhook echoes it back and marks the session `succeeded`.
- `signature` is `sha256(username + salt)`, added when the salt is configured.
- Tier upgrades reuse the same mechanism: a FlexForm URL carrying
  `originalSubscriptionId` (via `POST /v1/me/subscriptions/{id}/change-tier`).

There is no return-trip trust: the subscription is created only when the
webhook arrives.

### Webhook registration

Point CCBill's Webhooks admin at:

- `POST /v1/webhooks/ccbill` (single-merchant), or
- `POST /v1/merchants/{slug}/webhooks/ccbill` (multi-merchant/embedded)

The `eventType` must arrive as a query parameter and, when the body also
carries one, the two must match. Payloads may be form-encoded or JSON.

Verification, as implemented (CCBill has no HMAC):

- **Source IP allowlist** — CCBill's documented provider-wide ranges
  (`64.38.212.0/24`, `64.38.215.0/24`, `64.38.240.0/24`, `64.38.241.0/24`)
  are built in; nothing to configure. Any OTHER source must be listed
  explicitly in `ccbill_webhook_ip_allowlist` (a CIDR list; `/0` is refused),
  and even then is accepted only under sandbox posture (`test_mode: sandbox`)
  while the PSP catalog proves no live CCBill PSP exists anywhere. If that
  cannot be proven — probe error, no DB — the entry is refused. There is no
  "test_mode accepts any IP" bypass.
- **Account match** — the payload's `clientAccnum`/`clientSubacc` must equal
  the pair derived from the declared `account_id`.
- A merchant with no armed CCBill account rejects the webhook with a 5xx
  (fail closed; CCBill redelivers).

Consumed events: `NewSaleSuccess`, `NewSaleFailure`, `RenewalSuccess`,
`RenewalFailure`, `UpgradeSuccess`, `UpgradeFailure`, `Cancellation`,
`Expiration`, `BillingDateChange`, `CustomerDataUpdate`, `UserReactivation`,
`Refund`, `Void`, `Chargeback`. Unknown event types are rejected as
non-retryable. Events are deduplicated by transaction id / payload hash.

### Sandbox testing

Set the global `test_mode: sandbox` (and typically `environment: test` on the
PSP entry). Sandbox posture routes FlexForm URLs to
`https://sandbox-api.ccbill.com/wap-frontflex/flexforms/...` instead of
`api.ccbill.com`. To post webhooks from a local harness, declare its source
explicitly, e.g. `ccbill_webhook_ip_allowlist: ["127.0.0.1/32"]` — sandbox
posture alone accepts nothing extra.

### Rebill and cancellation semantics

- CCBill owns the rebill schedule; OpenRails follows the roster via webhooks
  (`RenewalSuccess` extends, `Cancellation`/`Expiration` end access at the
  paid-through boundary).
- User cancels queue like every other rail (`POST
  /v1/me/subscriptions/{id}/cancel` → `202 queued`). The remote leg is a
  durable intent executing DataLink's `cancelSubscription`
  (verify-then-execute: a status read first — already-not-rebilling counts as
  success; ambiguous outcomes re-verify rather than decline). CCBill keeps
  the subscriber's access through the paid period on its own side. CCBill
  cancels are destructive: no resume.
- DataLink (`datalink.ccbill.com`) also powers merchant-initiated refunds
  (void-or-refund) and transaction-export reconciliation — all gated on the
  `datalink_username`/`datalink_password` pair.
- CCBill subscriptions cannot be reassigned to another payment method;
  payment-method changes go through a new checkout.
