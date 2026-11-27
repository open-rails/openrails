# Invoices and checkout

An invoice is every bill: a one-off purchase, a subscription renewal (provider-run
ones included) or a period's usage, each with frozen lines and a total. Checkout
is one way to pay an invoice: it turns the lines a customer picks into an invoice and
takes its payment, through carts and previews, one-off purchases (saved or new
card, a provider's hosted page, a Solana wallet) and the payment page a pay URL
opens. Customers buy through `/v1/me` (billing-ui), a merchant's backend creates
unpaid invoices for them (`/v1/app/invoices`), and staff read them. Renewals are
[subscriptions](subscriptions.md), usage invoices [usage billing](usage-billing.md),
and collecting what is unpaid [collection and dunning](dunning.md).

## Concepts

- **Invoice** (`in_`, `invoices`): frozen lines and a total in one currency for one
  customer; `origin` `customer`, `merchant`, `subscription` (a renewal) or `usage`.
  Status `open`, `processing`, `complete`, or closed unpaid as `canceled` or
  `expired`; a renewal or usage invoice can also end `void` or `uncollectible`.
  Only `collection_owner = engine` invoices are charged here
  ([provider-managed invoices](../client.md#invoices-and-checkout)).
- **Line** (`invoice_lines`): price, product, quantity, amounts, ownership rule and
  cadence as sold; later catalog edits never reprice it.
- **Ownership claim** (`ownership_claims`): one live holder per customer and
  `product:<id>` or `tier_group:<name>`, so two buys of one unique product cannot
  both succeed.
- **Invoice attempt** (`iat_`, `invoice_attempts`): one try to pay on one PSP; at
  most one live per invoice (`invoice_attempts_invoice_id_live_key`).
- **Cart** (`carts`): one per customer, at most `billing.MaxCartLines` (100)
  lines, priced live like a **preview** (`billing.InvoicePreview`): per-line
  refusals, the total and the payment options.
- **Pay URL**: `/pay/<invoice>#pus_<secret>` on the checkout origin. The secret acts
  as the invoice's customer, for that invoice alone, and only there.

## How it works

**Pricing.** `invoices.Service.quote` (`internal/modules/invoices/quote.go`) resolves
each line's price and product and refuses lines one by one: not for sale, a trial
or free recurring price, another currency, a bad quantity or chosen amount,
already owned or claimed (`refuseOwned`), or missing required entitlements
(`refuseUnentitled`). Preview, cart reads and create all use it.

**Create.** `Create` and `FindOrCreate` (`create.go`) run in one `MerchantTx`
under `LockCustomerForSpend`, so a customer's invoices are made one at a time. They
quote, refuse a total other than `expected_total` (`invoice_total_changed`), insert
the invoice and lines and take each unique line's claim (`ClaimOwnership`). A
customer asking again for the same lines (not from a cart) gets the open invoice
back (`FindOrCreate`, `lines_digest`, `invoices_open_lines_key`); their own unpaid
one-line invoice of another price of the product is canceled for the new one. A
cart invoice empties the cart in the same transaction (`takeCart`). A zero total
completes at once. An invoice in one of the merchant's own units is paid from the customer's balance in it as it is created (`payFromBalance`): each line draws the live lots, soonest expiry first, one `credit_spend` per lot at (`invoice`, `invoice_line`, line) naming the invoice, after the customer's pending usage and running charges posted (lots are spent in time order), and a balance short of the total is `insufficient_credits` with nothing written. It has no payment (`invoices_complete_payment_check`), takes no `payment` or pay URL, and keeps the request's `Idempotency-Key`, which is required, so a retry answers it. Lifetimes: a customer's purchase expires after `invoices.CustomerTTL` (24 h, Stripe Checkout's); a merchant invoice is a bill and never expires, though its pay URL does (`paymentpage.Plan`: an hour, 30 minutes to 24 hours).

**Pay.** `payment.InvoiceAttemptService.PayInvoice` (`internal/modules/payment/invoice_pay.go`)
takes exactly one payment:

- *Card* (NMI, Stripe, custodian-held): one transaction re-locks customer and
  invoice, rechecks `invoices.Payable` and `StillBuyable`, records the `nmi_sale`
  provider intent with frozen terms, and creates and starts the invoice attempt,
  whose id is `InvoiceAttemptID(merchant, invoice, key)`: one Idempotency-Key is one
  attempt, forever. The intent runs inline (`EnqueueOwnedAndExecute`), bounded
  by its lease independently of the caller's cancellation; `NMISaleIntentHandler` executes NMI and Stripe sales alike, and charges and
  saves a new card in one request.
- *Hosted*: Stripe Checkout for one-time lines (`payInvoiceHosted`) or a CCBill
  FlexForm for one subscription (`payInvoiceCCBill`); the invoice waits with a
  `redirect_to_url` next action.
- *Solana*: a Solana Pay transaction request (`/v1/invoices/{id}/solana-pay`),
  settled from the chain ([payment-providers.md](payment-providers.md)).

**Settle.** A qualified receipt completes the intent and, in that transaction,
`invoices.Service.Paid` (`settle.go`): the payment, a number from `invoice_number_seq`,
each line fulfilled (product access, a credit lot or a subscription), claims
handed to what they produced, the receipt notice and `invoice.completed`. A definite
decline (`Declined`) reopens the invoice with `last_payment_error`; a challenge
(`Pending`) waits for `POST …/confirm`, which reads the provider (`ConfirmInvoice`).

**Books.** Storing a payment books its money in the general ledger (`cash_received` into `unapplied_cash`). When an invoice completes, by `Paid`, `RecordRemittance`, a renewal (`RenewalPaid`) or a provider's bill (`observed.Record`), `books.ApplyInvoice` applies its money to its lines (`cash_applied`, and `revenue_adjusted` for a provider's non-cash settlement) and gives each line a revenue schedule over the service it sells: a subscription's period, an access window, or delivery at once. A late or second payment stays unapplied. An invoice in a merchant's own unit has no payment: its lines' spends released each lot's money into `deferred_revenue` in the lot's paid currency, naming the schedule `books.CreditLineSchedule(line, currency)`, and applying it creates one schedule per currency, funded with exactly that (`applyUnitInvoice`); money released into it later adds to its funding, and `ledger-audit` fails a release with no schedule. See [books](books.md#cash-and-revenue-recognition).

**Close.** A customer can cancel only an invoice with origin `customer`; usage
invoices cannot be canceled by customers, and merchant-created invoices can be
canceled only through the merchant route. Subscription invoices keep their
refusal: cancel the subscription instead. `close` cancels or expires an open
invoice, releases its claims and
abandons a challenged attempt at the provider (`AbandonInvoiceAttempt`). Only a
customer's purchase has `expires_at`: one past it reads expired at once
(`Invoice.AsOf`), the `openrails.invoice_expiry` scan of the hourly billing pass
(`InvoiceExpiryWorker`) writes it, and the customer's own list leaves it out
(`ListFilter.OmitExpiredPurchases`). Money landing on
a closed invoice revives it while its claims are free; otherwise it is recorded,
its full refund is queued (`intents.RefundLateInvoicePayment`; CCBill's is left to a
person) and a critical `life.invoice.late_payment` is raised, as for a second
payment of a complete invoice (`PaidAgain`). `RecordExternalPayment`
(`RecordRemittance`) pays an open invoice with money received elsewhere: exactly the
total, no recurring line, replayed by `transaction_id`.

**Checkout facts** (`internal/modules/invoices/checkout.go`). A checkout is an invoice `Create` makes for a customer to pay in money (origin `customer` or `merchant`); renewals, usage invoices, provider bills and invoices in a merchant's unit (spent from the balance, never paid) are not, and the purchase count leaves the last out. Each records, on the invoice:

- `customer_type`: `reactivated` when the customer had MRR with the merchant (a movement) and has none now (each currency's latest movement by then), `new` on a first purchase without MRR history, `returning` otherwise; read with `purchase_number` (`GetCheckoutCustomer`). MRR adopted at import starts from the subscriptions' state then, so an imported former subscriber reads `new`.
- `purchase_number`: 1 + the customer's paid purchases (complete checkouts, refunded ones included, and observed initial enrollments) when it was created, counted under the same lock. Two checkouts open at once share the number neither has paid yet.
- `opened_at`: the customer's first read of the open invoice in person (`MarkCheckoutOpened`): billing-ui's `InvoicePayment` and the payment page both read it on load. A merchant creating a pay URL, staff reads and later reads change nothing.
- `submitted_at`: the first pay request (`MarkCheckoutSubmitted`). On `POST /v1/me/invoices/{id}/pay` a route step writes it right after the customer gate (`SubmitsCheckout`), before the pay URL limit, the attack captcha, card-attempt limits or the provider can refuse the request; a request the deployment's rate limiter refuses before authentication is not recorded. A one-call buy is opened and submitted when it is created. Both stamps write only to an open, unexpired checkout, and nothing when nothing would change.
- Countries, never the address: `billing_country` from the latest pay request's billing details or the saved card's, and `ip_country` from `CF-IPCountry` on the first request in person, read only when the request came through Cloudflare's published ranges (`iputil.CloudflareRanges`, `TrustedProxies.Country`; `XX` and `T1` ignored). `card_country`, the card's issuing country, is not recorded yet and reads null, so `country` is the billing country, else the IP's.
- `language`: the first in-person request's `Accept-Language`, as language[-region] (`request.PreferredLanguage`): its first tag by weight that names one language, skipping `*`, `mul` and entries that do not parse.
- Attribution (`billing.InvoiceAttribution`): source, medium, campaign, term, content, referrer and affiliate, set on creation (`invoices.CheckAttribution`): percent-decoded and NFKC-normalized (a value over 4,096 bytes is left out before either); the referrer keeps only its lowercase ASCII host. A field over 100 bytes, unprintable, or carrying an email address or card number is left out and counted (`checkout.attribution_dropped`); attribution never refuses a sale. Reopening the open invoice of the same lines keeps the first. Term and content often carry a recipient's id: they are kept for the merchant's own reads and are no metrics dimension. A customer's erasure (`scim.RunErasure`) clears every attribution field of its checkouts (`EraseCustomerInvoiceAttribution`).

A zero-total checkout completes when it is created, with neither `opened_at` nor `submitted_at`; conversion counts leave it out. Settling a checkout, or a provider's first bill bound to one, names it on the subscription it creates (`subscriptions.checkout_invoice_id`); a membership that replaces another (an upgrade, a Solana tier change) keeps the one it replaces. Every bill of the subscription copies it (`invoices.checkout_invoice_id`, the `assign_checkout_invoice` trigger, one row read; `Invoice.renewal.checkout_invoice_id`). Staff and application reads show the facts as `Invoice.checkout`; the customer's own reads never do. The checkout measures read them ([reporting](reporting.md)).

**Payment page** (`internal/paymentpage`). `Attach` stores the secret's sha256 on a
merchant invoice, with a creation receipt (`idempotency_keys`, `pay_url_creation`)
that lets only an authenticated retry of the same create recover the URL
(`ReplayURL`). `AttachMine` gives a customer's own open invoice an embedded pay URL,
ending the last. On the checkout origin `ResolveHTTP` resolves the bearer to one
live merchant (`ResolveInvoicePayURLMerchant`) or answers `pay_url_not_found` /
`pay_url_expired`, and `paymentpage.Authenticator` acts as that customer, in person.
`RegisterPaymentPageRoutes` mounts only routes with a `PayURL` access; `payURLScope`
pins `{id}` to the secret's invoice. Framing follows `checkout.embed_origins`.

## Interfaces

| Caller | Routes |
|---|---|
| Customer (`/v1/me`) | `GET/DELETE /v1/me/cart`, `PUT /v1/me/cart/lines/{price_id}`, `POST /v1/me/invoices/preview`, `POST/GET /v1/me/invoices`, `GET /v1/me/invoices/{id}`, `GET …/payment-attempt`, `POST …/pay`, `…/confirm`, `…/cancel`, `…/pay-url-failure` |
| Merchant backend | `POST /v1/app/invoices/preview`, `POST /v1/app/invoices`, `POST /v1/app/invoices/{id}/cancel`, `GET /v1/app/invoices/{id}/payment-attempt` |
| Staff | `GET /v1/admin/invoices`, `GET /v1/admin/invoices/{id}`, `POST /v1/admin/payments` |
| Wallet | `GET/POST /v1/invoices/{id}/solana-pay` |

- Go: `Client.PreviewInvoice`, `CreateInvoice`, `CancelInvoice`, `GetInvoicePaymentAttempt`,
  `ListInvoices`, `GetInvoice`, `RecordExternalPayment`; `Engine.CheckoutPage`.
- Configuration: `checkout_origin` (`openrails.Config.CheckoutOrigin`); merchant settings `checkout.embed_origins`.
- Events: `invoice.completed` (fulfil on it), `invoice.payment_failed`,
  `invoice.requires_action`, `invoice.canceled`, `invoice.expired`.
- Guides: [payment page](../client.md#invoices-and-checkout), [provider intents](../standalone.md#provider-intents).

## Guarantees

### 1. Exactly-once provider calls
A card sale is a `provider_intents` row (`nmi_sale`, `NMISaleIdempotencyKey`)
committed with its invoice attempt before any provider call. `fenceSale` sets the
`sale_submitted` fence (`RecordProgressIfAbsent`) so one execution submits; after
it only a read-back runs (`Verify`, or `recoverStripeSale` on Stripe), which never
resends. A replayed key must match the stored invoice, attempt, customer and
request (`owns`), else `idempotency_key_reused`. A Checkout Session moves no
money; it opens after the attempt commits, with Stripe idempotency key
`invoice:<attempt>`. CCBill and Solana get no OpenRails write: the customer pays on
CCBill's page or signs in a wallet.

Checks:
- `payInvoice` enqueues the intent and `CreateInvoiceAttempt` in one `MerchantTx`; a
  unique violation answers `invoice_payment_in_progress`.
- `Verify` returns `Retryable` for an unsubmitted sale and never calls `RunSale`.
- OR-J10-S01 (one debit per key), OR-J10-S03 (key reused for another request), OR-J10-S07 (a merchant invoice's key is unique per merchant, whichever customer), OR-J29-S02 (death after acceptance).

### 2. Agreement with providers
`ConfirmInvoice` reads the provider, never trusting the browser. A lost sale answer
is settled by reads (`collect`, `absentSale`). NMI's definitive decline under
its order ends it unpaid. An empty order search keeps it `processing` with a
high `life.sale.unresolved` finding until 24 hours after `submitted_at`, then
releases it unpaid. A retained payment receipt prevents release. An operator may
release an empty order earlier with
`intents resolve --not-executed`, an actor and a reason. Both releases reopen
the invoice with a processing failure, resolve the unresolved finding and raise
`life.sale.released_unconfirmed`: search absence is not proof that no money moved.
Failed searches and orders containing unqualified transactions stay processing.
A released open invoice expires normally. A terminal released intent stops
verification and cannot accept a receipt resolution; later money requires
operator reconciliation and refund of an unhonored charge at NMI. A partial
approval settles at NMI only in full (`nmi.SettleOnlyInFull`) and never pays the
invoice. Stripe Checkout settles from `checkout.session.*` webhooks or confirm
(`confirmInvoiceHosted`). `settle` treats a replayed payment as done and refuses a
different one on a complete invoice (`errLateApprovalDup`).

Checks:
- OR-J09-S05 (lost NMI answer) and OR-J09-S06 (partial approval).
- OR-J28-S04 and OR-J28-S08: duplicate, old and thin Stripe events settle once.
- OR-J38-S43 (a second payment of a paid invoice) and OR-J37-S70 (a transfer after
  expiry): money the invoice no longer takes is held for refund or review.

An approved new-card sale whose card cannot be read still pays the invoice and
grants access. The intent retains its receipt and retries storage without charging
again; `life.sale.card_not_saved` identifies the customer, invoice and any opened
subscription until storage recovers. The subscription waits for its method. Late
storage also records the accepted card-on-file agreement for a reusable card,
including a one-time purchase, once. Gateway refusals retain `request_refused` and
`gateway_refused` evidence and use the response code for `last_payment_error`;
they do not count as card declines.

### 3. Exact money
Totals and line amounts are `int64` micros (`invoices.total`, `CHECK (total >= 0)`);
`quote` refuses overflow and mixed currencies, and `Paid` a charge whose amount or
currency differs from the invoice. NMI sales, Stripe sales and Stripe Checkout get
`moneyutil.NativeToRailMinorExact`, which refuses a sub-minor remainder. An
off-channel payment must equal the total (`payment_exceeds_due`).

Checks:
- OR-J09-S01 pins 29,970,000 micros as 2997 `usd` cents at Stripe; OR-J09-S06
  pins NMI's decimals (a 0.70 sale approved for 0.30 leaves 0.40).
- OR-J08-S01: cart and preview return literal line totals.

### 4. Access control and tenant isolation
`/v1/me` routes act on the scope's customer (`invoiceActor`); another customer's invoice is not found (`GetForCustomer`). Paying needs the customer in person (`billingauth.Interactive`: a session, device key, access token or pay URL of the customer itself; never an API key or personal access token), else `customer_action_required`; a customer's pay URL needs the merchant's own signed-in person (`signedInPerson`, `customer_session_required`). `/v1/app/invoices` routes require `merchant:invoices:manage` or `root:invoices:manage` (`needInvoices`); staff routes take the customer-support pair by `Level`. With it an application can create an invoice in one of the merchant's own units, which the customer's balance pays as it is created, as a merchant invoice of a saved card's customer is charged without them. A pay URL secret (credential kind `pay_url`) works only on the checkout origin. Invoice queries name `merchant_id`; directory reads before a merchant is pinned (`ResolveInvoicePayURLMerchant`, `GetInvoiceMerchant`, `ListInvoiceExpiryMerchants`, `DeleteExpiredCarts`) span merchants by design, and a lookup matching two answers nothing.

Checks:
- Each `/v1/app` route in `internal/http/routes/invoices.go` has `Permission: needInvoices`.
- `payURLScope` refuses another invoice's `{id}` and saved cards the URL does not offer.
- OR-J03-S01 to S03: a secret pays its invoice only; API and checkout authority stay apart.
- OR-J01-S17: a personal access token reads the invoice but cannot pay it; the session pays.

### 5. Contracts match the code
Every route is declared once in the route catalog; admin and app routes carry the
`Name` of their `Client` method. The in-process `Client` calls the same handlers
through the in-process transport, so both clients answer alike. Invoices, lines,
attempts, carts and claims are in `api/go.txt`, `api/openapi.json` and `api/schema.txt`.

Checks:
- `go run ./scripts/contracts` passes with no removed or changed line.
- Each `Name` in `internal/http/routes/invoices.go` is a `Client` method that calls
  that route.

### 6. Data lifecycle
Retention classes (`retention.Tables`): `invoices` rows (unpaid closed invoices that
never started an attempt go 397 days after closing, `DeleteClosedUnpaidInvoices`, so a checkout's abandonment can be compared with the same month a year before),
`invoice_attempts` rows (expired never-sent attempts after 90 days), `invoice_lines`
with their invoice, `carts` 30 days after the last change, `ownership_claims` state,
`revenue_schedules` permanent.
None is partitioned. Carts stay out of the merchant archive, and a live invoice
attempt refuses an export (`preflightChecks`).

Checks:
- Each table comment carries the `Retention:` rule `retention.Tables` states.
- OR-J29-S08 (cleanup and expiry) and OR-J31-S09 (a paid hosted invoice exports).
- OR-J47-S09: an unpaid checkout keeps its facts for 397 days, then is deleted.

### 7. Work scales with activity
Payment runs in the request; settlement comes from intents and webhooks. The only
scan is invoice expiry: `ListInvoiceExpiryMerchants` and `ListExpiredInvoices` read the
partial indexes `invoices_expires_at_live_idx` and `invoice_attempts_expires_at_expirable_idx`,
200 at a time (`invoiceExpiryBatch`); an idle pass costs one indexed read, and
`DueBy` reports expiry a day overdue. Reads apply expiry themselves. Cleanup uses
`invoices_closed_at_unpaid_idx` and `carts_last_changed_at_idx`.

Checks:
- OR-J04-S37: an expiry backlog past one batch drains in one pass.
- OR-J08-S70, OR-J08-S71, OR-J09-S70: expired invoices act expired before any pass.
- OR-J08-S72, OR-J08-S73: a merchant invoice never expires; past 30 days it is paid or canceled.
- OR-J08-S74: a purchase that expired unpaid leaves the customer's own list; the merchant's keeps it.

### 8. Abuse protection
The `purchase` bucket ([limits](../standalone.md#abuse-limits)) covers `POST /v1/me/invoices`, `…/pay`, `…/confirm` and `POST /v1/invoices/{id}/solana-pay` (`ClassifyBucket`), and its captcha subjects ride into the sale (`intents.WithInvoiceCaptchaSubjects`). A pay URL allows 120 reads and 10 pays a minute per invoice (`payURLLimit`) and 10 declines (`paymentpage.MaxDeclines`, `pay_url_attempts_exhausted`). `GuardCardAttempt` checks the card-testing ledger before any provider call (`card_attempts_blocked`); an Idempotency-Key or any field of the invoice create or pay body holding a card number is refused (`cardguard.ContainsPAN`, `cardguard.PANField`). Previews, carts, cancels and reads are not limited here, only bounded by `billing.MaxInvoiceLines` and `MaxCartLines`.

Checks:
- `isPurchasePath` matches exactly the routes above.
- `PayInvoice` calls `GuardCardAttempt` before `payInvoice`; `payURLPayment` calls
  `Exhausted` before a pay on the checkout origin.

### 9. Destructive-action safeguards
Nothing here is irreversible or bulk. Canceling or expiring an invoice moves no
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
- OR-J08-S01 to S04, S70, S71: cart and preview totals, app invoices never charge,
  refused lines and stale totals, settled invoices survive cancel and expiry.
- OR-J09-S01 to S06, S70: one-off charge, challenge, decline, rollback, lost
  answer, partial approval, expiry during a challenge.
- OR-J47-S01, S02, S04 to S10: checkout steps where they happen (an attack captcha's refusal included), purchase number, customer type, country, language, attribution, renewals naming their checkout, and retention.
- OR-J48-S04: an invoice in a unit is paid from the balance and grants its keys; a short balance creates nothing.
- Also: OR-J05-S05, OR-J10-S01 to S05, OR-J21-S01 to S03, OR-J28-S04, OR-J28-S08,
  OR-J04-S04, OR-J04-S37, OR-J04-S46, OR-J29-S02, OR-J29-S08, OR-J31-S09,
  OR-J37-S70, OR-J38-S43.

Other tests: `invoices.CheckAttribution` and `request.PreferredLanguage` have unit tests; the payment and payment-page packages have none.

Gaps:
- No scenario for the customer's own embedded pay URL (`AttachMine`,
  `customer_session_required`, `embed_origins_required`), Apple Pay domain
  registration, or cart expiry.
- No scenario for `pay_url_attempts_exhausted`, the pay URL rate limits, or
  `card_attempts_blocked` on an invoice.
- No scenario covers the refund `RefundLateInvoicePayment` queues for a late NMI or
  Stripe payment; OR-J38-S43 covers only CCBill's, held for a person.
