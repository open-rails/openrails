# Orders and checkout

An order is every bill: a one-off purchase, a subscription renewal (provider-run
ones included) or a period's usage, each with frozen lines and a total. Checkout
is one way to pay an order: it turns the lines a customer picks into an order and
takes its payment, through carts and previews, one-off purchases (saved or new
card, a provider's hosted page, a Solana wallet) and the payment page a pay URL
opens. Customers buy through `/v1/me` (billing-ui), a merchant's backend creates
unpaid orders for them (`/v1/app/orders`), and staff read them. Renewals are
[subscriptions](subscriptions.md), usage orders [usage billing](usage-billing.md),
and collecting what is unpaid [collection and dunning](dunning.md).

## Concepts

- **Order** (`ord_`, `orders`): frozen lines and a total in one currency for one
  customer; `origin` `customer`, `merchant`, `subscription` (a renewal) or `usage`.
  Status `open`, `processing`, `complete`, or closed unpaid as `canceled` or
  `expired`; a renewal or usage order can also end `void` or `uncollectible`.
  Only `collection_owner = engine` orders are charged here
  ([provider-managed orders](../client.md#orders-and-checkout)).
- **Line** (`order_lines`): price, product, quantity, amounts, ownership rule and
  cadence as sold; later catalog edits never reprice it.
- **Ownership claim** (`ownership_claims`): one live holder per customer and
  `product:<id>` or `tier_group:<name>`, so two buys of one unique product cannot
  both succeed.
- **Order attempt** (`oat_`, `order_attempts`): one try to pay on one PSP; at
  most one live per order (`order_attempts_order_id_live_key`).
- **Cart** (`carts`): one per customer, at most `billing.MaxCartLines` (100)
  lines, priced live like a **preview** (`billing.OrderPreview`): per-line
  refusals, the total and the payment options.
- **Pay URL**: `/pay/<order>#pus_<secret>` on the checkout origin. The secret acts
  as the order's customer, for that order alone, and only there.

## How it works

**Pricing.** `orders.Service.quote` (`internal/modules/orders/quote.go`) resolves
each line's price and product and refuses lines one by one: not for sale, a trial
or free recurring price, another currency, a bad quantity or chosen amount,
already owned or claimed (`refuseOwned`), or missing required entitlements
(`refuseUnentitled`). Preview, cart reads and create all use it.

**Create.** `Create` and `FindOrCreate` (`create.go`) run in one `MerchantTx`
under `LockCustomerForSpend`, so a customer's orders are made one at a time. They
quote, refuse a total other than `expected_total` (`order_total_changed`), insert
the order and lines and take each unique line's claim (`ClaimOwnership`). A
customer asking again for the same lines (not from a cart) gets the open order
back (`FindOrCreate`, `lines_digest`, `orders_open_lines_key`); their own unpaid
one-line order of another price of the product is canceled for the new one. A
cart order empties the cart in the same transaction (`takeCart`). A zero total
completes at once. An order in one of the merchant's own units is paid from the customer's balance in it as it is created (`payFromBalance`): each line draws the live lots, soonest expiry first, one `credit_spend` per lot at (`order`, `order_line`, line) naming the order, after the customer's unposted usage; what holds and pending refunds keep is not spent, and a balance short of the total is `insufficient_credits` with nothing written. It has no payment (`orders_complete_payment_check`), takes no `payment` or pay URL, and keeps the request's `Idempotency-Key`, which is required, so a retry answers it. Lifetimes: `orders.CustomerTTL` 24 h; merchant orders
`MerchantTTL` 30 days, at most `MaxMerchantTTL` 120 days.

**Pay.** `payment.OrderAttemptService.PayOrder` (`internal/modules/payment/order_pay.go`)
takes exactly one payment:

- *Card* (NMI, Stripe, custodian-held): one transaction re-locks customer and
  order, rechecks `orders.Payable` and `StillBuyable`, records the `nmi_sale`
  provider intent with frozen terms, and creates and starts the order attempt,
  whose id is `OrderAttemptID(merchant, order, key)`: one Idempotency-Key is one
  attempt, forever. The intent runs inline (`EnqueueOwnedAndExecute`);
  `NMISaleIntentHandler` executes NMI and Stripe sales alike, and charges and
  saves a new card in one request.
- *Hosted*: Stripe Checkout for one-time lines (`payOrderHosted`) or a CCBill
  FlexForm for one subscription (`payOrderCCBill`); the order waits with a
  `redirect_to_url` next action.
- *Solana*: a Solana Pay transaction request (`/v1/orders/{id}/solana-pay`),
  settled from the chain ([payment-providers.md](payment-providers.md)).

**Settle.** A qualified receipt completes the intent and, in that transaction,
`orders.Service.Paid` (`settle.go`): the payment, a number from `order_number_seq`,
each line fulfilled (product access, a credit lot or a subscription), claims
handed to what they produced, the receipt notice and `order.completed`. A definite
decline (`Declined`) reopens the order with `last_payment_error`; a challenge
(`Pending`) waits for `POST …/confirm`, which reads the provider (`ConfirmOrder`).

**Close.** `close` cancels or expires an open order, releases its claims and
abandons a challenged attempt at the provider (`AbandonOrderAttempt`). An order
past `expires_at` reads expired at once (`Order.AsOf`); the `openrails.order_expiry`
scan of the hourly billing pass (`OrderExpiryWorker`) writes it. Money landing on
a closed order revives it while its claims are free; otherwise it is recorded,
its full refund is queued (`intents.RefundLateOrderPayment`; CCBill's is left to a
person) and a critical `life.order.late_payment` is raised, as for a second
payment of a complete order (`PaidAgain`). `RecordExternalPayment`
(`RecordRemittance`) pays an open order with money received elsewhere: exactly the
total, no recurring line, replayed by `transaction_id`.

**Payment page** (`internal/paymentpage`). `Attach` stores the secret's sha256 on a
merchant order, with a creation receipt (`idempotency_keys`, `pay_url_creation`)
that lets only an authenticated retry of the same create recover the URL
(`ReplayURL`). `AttachMine` gives a customer's own open order an embedded pay URL,
ending the last. On the checkout origin `ResolveHTTP` resolves the bearer to one
live merchant (`ResolveOrderPayURLMerchant`) or answers `pay_url_not_found` /
`pay_url_expired`, and `paymentpage.Authenticator` acts as that customer, in person.
`RegisterPaymentPageRoutes` mounts only routes with a `PayURL` access; `payURLScope`
pins `{id}` to the secret's order. Framing follows `checkout.embed_origins`.

## Interfaces

| Caller | Routes |
|---|---|
| Customer (`/v1/me`) | `GET/DELETE /v1/me/cart`, `PUT /v1/me/cart/lines/{price_id}`, `POST /v1/me/orders/preview`, `POST/GET /v1/me/orders`, `GET /v1/me/orders/{id}`, `GET …/payment-attempt`, `POST …/pay`, `…/confirm`, `…/cancel`, `…/pay-url-failure` |
| Merchant backend | `POST /v1/app/orders/preview`, `POST /v1/app/orders`, `POST /v1/app/orders/{id}/cancel`, `GET /v1/app/orders/{id}/payment-attempt` |
| Staff | `GET /v1/admin/orders`, `GET /v1/admin/orders/{id}`, `POST /v1/admin/payments` |
| Wallet | `GET/POST /v1/orders/{id}/solana-pay` |

- Go: `Client.PreviewOrder`, `CreateOrder`, `CancelOrder`, `GetOrderPaymentAttempt`,
  `ListOrders`, `GetOrder`, `RecordExternalPayment`; `Engine.CheckoutPage`.
- Configuration: `checkout_origin` (`server.Config.CheckoutOrigin`); merchant
  settings `checkout.embed_origins`.
- Events: `order.completed` (fulfil on it), `order.payment_failed`,
  `order.requires_action`, `order.canceled`, `order.expired`.
- Guides: [payment-page.md](../client.md#orders-and-checkout), [provider-uncertainty.md](../standalone.md#provider-intents).

## Guarantees

### 1. Exactly-once provider calls
A card sale is a `provider_intents` row (`nmi_sale`, `NMISaleIdempotencyKey`)
committed with its order attempt before any provider call. `fenceSale` sets the
`sale_submitted` fence (`RecordProgressIfAbsent`) so one execution submits; after
it only a read-back runs (`Verify`, or `recoverStripeSale` on Stripe), which never
resends. A replayed key must match the stored order, attempt, customer and
request (`owns`), else `idempotency_key_reused`. A Checkout Session moves no
money; it opens after the attempt commits, with Stripe idempotency key
`order:<attempt>`. CCBill and Solana get no OpenRails write: the customer pays on
CCBill's page or signs in a wallet.

Checks:
- `payOrder` enqueues the intent and `CreateOrderAttempt` in one `MerchantTx`; a
  unique violation answers `order_payment_in_progress`.
- `Verify` returns `Retryable` for an unsubmitted sale and never calls `RunSale`.
- OR-J10-S01 (one debit per key), OR-J10-S03 (key reused for another request), OR-J10-S07 (a merchant order's key is unique per merchant, whichever customer), OR-J29-S02 (death after acceptance).

### 2. Agreement with providers
`ConfirmOrder` reads the provider, never trusting the browser. A lost sale answer
is settled by reads (`collect`, `absentSale`): only NMI's definitive decline under
the sale's own order reference ends it unpaid; anything else stays `processing`
with a high `life.sale.unresolved` finding and takes no other payment. A partial
approval settles at NMI only in full (`nmi.SettleOnlyInFull`) and never pays the
order. Stripe Checkout settles from `checkout.session.*` webhooks or confirm
(`confirmOrderHosted`). `settle` treats a replayed payment as done and refuses a
different one on a complete order (`errLateApprovalDup`).

Checks:
- OR-J09-S05 (lost NMI answer) and OR-J09-S06 (partial approval).
- OR-J28-S04 and OR-J28-S08: duplicate, old and thin Stripe events settle once.
- OR-J38-S43 (a second payment of a paid order) and OR-J37-S70 (a transfer after
  expiry): money the order no longer takes is held for refund or review.

### 3. Exact money
Totals and line amounts are `int64` micros (`orders.total`, `CHECK (total >= 0)`);
`quote` refuses overflow and mixed currencies, and `Paid` a charge whose amount or
currency differs from the order. NMI sales, Stripe sales and Stripe Checkout get
`moneyutil.NativeToRailMinorExact`, which refuses a sub-minor remainder. An
off-channel payment must equal the total (`payment_exceeds_due`).

Checks:
- OR-J09-S01 pins 29,970,000 micros as 2997 `usd` cents at Stripe; OR-J09-S06
  pins NMI's decimals (a 0.70 sale approved for 0.30 leaves 0.40).
- OR-J08-S01: cart and preview return literal line totals.

### 4. Access control and tenant isolation
`/v1/me` routes act on the scope's customer (`orderActor`); another customer's order is not found (`GetForCustomer`). Paying needs the customer in person (`billingauth.Interactive`: a session, device key, access token or pay URL of the customer itself; never an API key or personal access token), else `customer_action_required`; a customer's pay URL needs the merchant's own signed-in person (`signedInPerson`, `customer_session_required`). `/v1/app/orders` routes require `merchant:orders:manage` or `root:orders:manage` (`needOrders`); staff routes take the customer-support pair by `Level`. A pay URL secret (credential kind `pay_url`) works only on the checkout origin. Order queries name `merchant_id`; directory reads before a merchant is pinned (`ResolveOrderPayURLMerchant`, `GetOrderMerchant`, `ListOrderExpiryMerchants`, `DeleteExpiredCarts`) span merchants by design, and a lookup matching two answers nothing.

Checks:
- Each `/v1/app` route in `internal/http/routes/orders.go` has `Permission: needOrders`.
- `payURLScope` refuses another order's `{id}` and saved cards the URL does not offer.
- OR-J03-S01 to S03: a secret pays its order only; API and checkout authority stay apart.
- OR-J01-S17: a personal access token reads the order but cannot pay it; the session pays.

### 5. Contracts match the code
Every route is declared once in the route catalog; admin and app routes carry the
`Name` of their `Client` method. The in-process `Client` calls the same handlers
through the in-process transport, so both clients answer alike. Orders, lines,
attempts, carts and claims are in `api/go.txt`, `api/openapi.json` and `api/schema.txt`.

Checks:
- `go run ./scripts/contracts` passes with no removed or changed line.
- Each `Name` in `internal/http/routes/orders.go` is a `Client` method that calls
  that route.

### 6. Data lifecycle
Retention classes (`retention.Tables`): `orders` rows (unpaid closed orders that
never started an attempt go 90 days after closing, `DeleteClosedUnpaidOrders`),
`order_attempts` rows (expired never-sent attempts after 90 days), `order_lines`
with their order, `carts` 30 days after the last change, `ownership_claims` state.
None is partitioned. Carts stay out of the merchant archive, and a live order
attempt refuses an export (`preflightChecks`).

Checks:
- Each table comment carries the `Retention:` rule `retention.Tables` states.
- OR-J29-S08 (cleanup and expiry) and OR-J31-S09 (a paid hosted order exports).

### 7. Work scales with activity
Payment runs in the request; settlement comes from intents and webhooks. The only
scan is order expiry: `ListOrderExpiryMerchants` and `ListExpiredOrders` read the
partial indexes `orders_expires_at_live_idx` and `order_attempts_expires_at_expirable_idx`,
200 at a time (`orderExpiryBatch`); an idle pass costs one indexed read, and
`DueBy` reports expiry a day overdue. Reads apply expiry themselves. Cleanup uses
`orders_closed_at_unpaid_idx` and `carts_last_changed_at_idx`.

Checks:
- OR-J04-S37: an expiry backlog past one batch drains in one pass.
- OR-J08-S70, OR-J08-S71, OR-J09-S70: expired orders act expired before any pass.

### 8. Abuse protection
The `purchase` bucket ([limits.md](../standalone.md#abuse-limits)) covers `POST /v1/me/orders`,
`…/pay`, `…/confirm` and `POST /v1/orders/{id}/solana-pay` (`ClassifyBucket`), and
its captcha subjects ride into the sale (`intents.WithOrderCaptchaSubjects`). A pay
URL allows 120 reads and 10 pays a minute per order (`payURLLimit`) and 10
declines (`paymentpage.MaxDeclines`, `pay_url_attempts_exhausted`).
`GuardCardAttempt` checks the card-testing ledger before any provider call
(`card_attempts_blocked`); an Idempotency-Key holding a card number is refused
(`cardguard.ContainsPAN`). Previews, carts, cancels and reads are not limited here,
only bounded by `billing.MaxOrderLines` and `MaxCartLines`.

Checks:
- `isPurchasePath` matches exactly the routes above.
- `PayOrder` calls `GuardCardAttempt` before `payOrder`; `payURLPayment` calls
  `Exhausted` before a pay on the checkout origin.

### 9. Destructive-action safeguards
Nothing here is irreversible or bulk. Canceling or expiring an order moves no
money, and money that lands later is still settled or refunded (`Paid`). The one
checkout staff write, `RecordExternalPayment`, is limited per staff member
(`AdminOperationOffChannel`, 10 a minute, logged as an `AdminRateLimitEvent`) and
idempotent by `transaction_id`; waivers and write-offs are in [dunning.md](dunning.md).

Checks:
- The `RecordExternalPayment` route declares `Limit: middleware.AdminOperationOffChannel`.
- `RecordRemittance` replays a repeated `transaction_id` and refuses changed terms.

## Tests

Journeys:
- OR-J03-S01 to S05: pay URL capability, origin separation, iframe messages
  (`TestHostedBrowser` drives the built SDK in a browser), redacted assets.
- OR-J08-S01 to S04, S70, S71: cart and preview totals, app orders never charge,
  refused lines and stale totals, settled orders survive cancel and expiry.
- OR-J09-S01 to S06, S70: one-off charge, challenge, decline, rollback, lost
  answer, partial approval, expiry during a challenge.
- OR-J48-S04: an order in a unit is paid from the balance and grants its keys; a short balance creates nothing.
- Also: OR-J05-S05, OR-J10-S01 to S05, OR-J21-S01 to S03, OR-J28-S04, OR-J28-S08,
  OR-J04-S04, OR-J04-S37, OR-J04-S46, OR-J29-S02, OR-J29-S08, OR-J31-S09,
  OR-J37-S70, OR-J38-S43.

Other tests: none; the order, payment and payment-page packages have no unit tests.

Gaps:
- No scenario for the customer's own embedded pay URL (`AttachMine`,
  `customer_session_required`, `embed_origins_required`), Apple Pay domain
  registration, or cart expiry.
- No scenario for `pay_url_attempts_exhausted`, the pay URL rate limits, or
  `card_attempts_blocked` on an order.
- No scenario covers the refund `RefundLateOrderPayment` queues for a late NMI or
  Stripe payment; OR-J38-S43 covers only CCBill's, held for a person.
