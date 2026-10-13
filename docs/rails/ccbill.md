# CCBill

> Which flows are supported on this rail, and how well each one is verified:
> [rail certification matrix](certification-matrix.md). Read it before relying on
> CCBill operations. Automatic refunds are unavailable; see the
> [CCBill refunds](ccbill-refund-qualification.md).

CCBill is a payment-page payment processor commonly used by high-risk and
adult/subscription businesses. In OpenRails it is a **reserved gateway**: the
rail and the PSP name are both `ccbill` (unlike NMI, where a PSP gets its own
name such as `mobius`). CCBill owns the card vault and the rebill schedule:
OpenRails never sees card data. It sells a subscription on the price's
FlexForm through an order, usually as a card PSP's fallback, and follows
every CCBill subscription through its webhooks.

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
  cancels and reconciliation; without it those paths cannot execute)
- per price: a **FlexForm** (`flex_id` + `form_name`) created in CCBill's
  FlexForms admin with matching pricing

### PSP manifest entry

Under `merchants.<slug>.psps` (secrets via YAML overlays; secret-store prefix
`psps/…`):

```yaml
psps:
  ccbill:               # PSP key — reserved gateway, must be "ccbill"
    rail: ccbill
    # clientAccnum-clientSubacc, dash-joined:
    account_id: "999999-0000"
    settings:
      # Offered only when mobius can't take the order (see below).
      fallback_for: [mobius]
      fallback_after_failures: 2   # optional; 2 by default
    secrets:
      salt: replace-with-ccbill-flexform-salt
      # Optional pair — declare both or neither:
      datalink_username: replace-with-ccbill-datalink-username
      datalink_password: replace-with-ccbill-datalink-password
```

That is the whole surface: no API key, no webhook signing secret (CCBill has
no webhook HMAC — see below), and no per-merchant IP allowlist.

### Fallback

`fallback_for` makes CCBill (or any PSP) a fallback for the PSPs it names.
An order lists it in `payment_options` (with `fallback: true`) only while
none of them can take the order (unarmed, disarmed by posture, archived, or
not selling its lines), or once the order has `fallback_after_failures`
failed attempts through them. Paying through it otherwise is refused with
`409 payment_option_withheld`. A PSP without `fallback_for`, Solana
included, is a peer and is always offered. billing-ui's `OrderPayment` shows
a fallback under "Card payment isn't working?".

### Catalog: linking prices to FlexForms

CCBill prices are **link-only** — OpenRails never auto-creates provider
objects on CCBill. Each price that sells over CCBill carries the FlexForm it
maps to:

```yaml
prices:
  premium-monthly:
    amount: 9.99 USD
    access_duration: 30 days
    billing_interval: 30 days
    psps: [ccbill]
    psp_links:
      ccbill:
        flex_id: "d7d6d9a5-..." # FlexForm GUID from CCBill admin
        form_name: "premium"
```

The FlexForm's own pricing must match the catalog price exactly: a new
sale must bill the order's total, and renewals are validated against the
catalog (2% tolerance). A `flexId`/`formName` that doesn't match the price's
link is rejected. A FlexForm sells subscriptions only: catalog application
refuses a one-time price only CCBill could sell (`price_not_sellable`).

### New sales

An order of one recurring line (one seat) whose price links a FlexForm can
be paid on CCBill:

- `POST /v1/me/orders/{id}/pay` with `payment: {psp_id: <ccbill>,
  return_url}` opens an order attempt and answers the order awaiting the
  customer: `next_action` is `redirect_to_url`, a signed FlexForm link
  carrying the attempt id as `reservationId`, the customer's verified email
  and username (from their credential, else the merchant's directory; else
  `400 customer_email_required`). The payment page follows it in its frame.
  Point the FlexForm's approval and denial URLs at the page customers return
  to; the order completes from the webhook, not the return.
- An order has one live CCBill page: paying again answers it. It lapses after
  an hour; another payment then closes it.
- `NewSaleSuccess` naming the attempt completes the order and opens the
  subscription CCBill bills (`collection_policy: provider`, its
  `rail_subscription_id`) with its access, in one transaction. The sale must
  bill the order's currency and total on the attempt's FlexForm and username.
  A redelivery changes nothing.
- `NewSaleFailure` naming the attempt fails it: the order takes another
  payment with `last_payment_error`.
- A sale the order no longer takes is never granted: the order was paid
  otherwise, a second sale on the same page, the customer already holds the
  membership, or the sale contradicts the attempt. It is refused
  (`ccbill_sale_unhonored`) with a `consistency.ledger.unbooked` finding:
  refund and cancel it at CCBill. A sale on a lapsed page still pays an order
  nothing else paid. One for an order that closed meanwhile revives it, or,
  when what it buys is held, is recorded for refund with a finding to cancel
  it at CCBill.
- A `NewSaleSuccess` no OpenRails attempt issued is refused
  (`ccbill_new_subscription_unsupported`) with the same finding.

### Upgrades

A customer's tier upgrade (`POST /v1/me/subscriptions/{id}/change`)
answers `requires_action` with a `redirect_to_url` next action, a FlexForm URL
carrying the subscription being upgraded:

```
https://api.ccbill.com/wap-frontflex/flexforms/{flex_id}?clientAccnum=…&clientSubacc=…&formName=…&originalSubscriptionId=…&signature=…
```

- The customer must have a **verified account email and a username**: OpenRails
  takes the email from the authenticated server-side identity, not the browser,
  and the webhook resolves the user by username.
- `signature` is `sha256(username + salt)`, added when the salt is configured.
- There is no return-trip trust: the upgrade applies only when the
  `UpgradeSuccess` webhook arrives. Downgrades are not offered.

### Webhook registration

Point CCBill's Webhooks admin at:

- `POST /v1/webhooks/ccbill/{account_id}` (standalone; the configured account resolves its merchant), or
- `POST <prefix>/v1/webhooks/ccbill/{account_id}` (embedded, e.g. `/billing/v1/...`)

`account_id` is the configured `clientAccnum-clientSubacc` identity and must match the payload. Accountless webhook URLs are not registered.

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

Set the global `test_mode: sandbox` — it is the only environment switch.
Sandbox posture routes FlexForm URLs to
`https://sandbox-api.ccbill.com/wap-frontflex/flexforms/...` instead of
`api.ccbill.com`. To post webhooks from a local harness, declare its source
explicitly, e.g. `ccbill_webhook_ip_allowlist: ["127.0.0.1/32"]` — sandbox
posture alone accepts nothing extra.

### Rebill and cancellation semantics

- CCBill owns the rebill schedule; OpenRails follows the roster via webhooks
  (`RenewalSuccess` extends, `Cancellation`/`Expiration` end access at the
  paid-through boundary).
- User cancels work like every other rail (`POST
  /v1/me/subscriptions/{id}/cancel` answers the subscription). The remote leg is a
  durable intent executing DataLink's `cancelSubscription`
  (verify-then-execute: a status read first — already-not-rebilling counts as
  success; ambiguous outcomes re-verify rather than decline). CCBill keeps
  the subscriber's access through the paid period on its own side. CCBill
  cancels are destructive: no resume.
- DataLink (`datalink.ccbill.com`) also powers transaction-export
  reconciliation, gated on the `datalink_username`/`datalink_password` pair.
  Once the merchant is armed for enforcement, a member DataLink lists as
  active while the local row is not raises a `pull.ccbill.active_without_payment`
  finding. A roster is not payment: only a RenewalSuccess restores access.
- Webhooks mirror CCBill through the lifecycle machine, one event per post on
  the locked row. BillingDateChange never moves the paid period;
  UserReactivation resumes only inside a paid period (else a
  `life.ccbill.reactivation_unapplied` finding); a RenewalSuccess on a canceled
  row is recorded for refund review.
- Automatic CCBill refunds are unavailable. The admin API refuses full and
  partial requests; combined cancel-and-refund refuses before cancellation.
  Existing unresolved refund intents retain their reserved balance and evidence
  for operator verification. Confirmed inbound refunds and manual accounting
  remain supported. See [CCBill refunds](ccbill-refund-qualification.md).
- An imported CCBill charge names CCBill's transaction id, which is digits.
  One declared without an amount is kept as an attribution to its
  subscription; no reader resolves CCBill attributions yet (DataLink is the
  planned source), so it records no payment until one does.
- CCBill subscriptions cannot be reassigned to another payment method;
  payment-method changes go through a new purchase.
