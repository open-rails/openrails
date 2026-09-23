# Money on the HTTP wire

Go uses signed int64 native currency units. Deposit, ledger receipt, balance,
checkout-session, capture, admission, usage-report, wasted-spend, invoice,
merchant settings/billing policy, spend delegation and self spend-window,
credit-limit, usage-rollup and resource-revenue, credit grant and credit
transaction, self balance and usage, invoker credit balance, delinquency,
Solana token base-unit, control-plane fleet analytics/timeseries, catalog
price and copilot price draft, public price, payment and refund,
subscription price/payment, tier-change and rate-card DTOs, and the hosted
checkout document (`HostedCheckoutSession`, served by hosts to the
`openrails-checkout` browser package with `plan.unit_decimals` stamped from
the registry), encode monetary values as decimal JSON strings. Fleet values
use `<thing>_amount` names (`settled_amount`, `monthly_amount`); the currency's
registered scale is the unit. Rate cards keep the same representation in
`catalog_rate_cards.price`. Invoice movement maps use the same representation.
A JavaScript
consumer must use BigInt or an exact decimal library for arithmetic; converting
to Number before parsing loses values above 2^53.

Invoice responses include unit_decimals. For USD, "1234567" with scale 6 means
1.234567 USD. JPY uses its declared scale, not an assumed cents/micros scale.
Currency codes are the registry's uppercase ISO-4217 spelling on every wire
surface, whatever case a request sent; requests are read case-insensitively.
Identifiers are typed (`ids.go`): products, prices, subscriptions, payments,
payment methods and checkout sessions travel as `prod_`, `price_`, `sub_`,
`pay_`, `pm_` and `cs_` text on every DTO, path and query parameter that names
them, and only that spelling is accepted — a bare UUID or another kind's prefix
is `invalid_param`. Customer, merchant and PSP ids are plain UUIDs. Go callers
hold `openrails.PriceID` etc.; the zero id marshals as `""` and `IsZero` tells.
The catalog `by-key` routes, checkout `price_id` on the public (browser)
checkout route and `plan-migrations` price references still accept a price
key; the shared Client's typed fields do not. The same spelling holds
wherever a typed kind appears inside another document: the customer's own
`/v1/me/subscriptions`, `/v1/me/status` and `/v1/me/notifications` (the
shared `Subscription`, `BillingStatus` and `Notification` shapes, so the ids
they list are the ids their action routes take), metrics `product_id` /
`price_id` dimensions and filters, findings evidence and recommendation
params, the hosted checkout document's `payment_id` / `subscription_id` /
`saved_methods[].id` / `payment_method_id`, and entitlement or grant
`source_id` (the source resource's own id beside `source_type`: `sub_` for
subscription and grace sources, `pay_` for one-off and purchase sources; an
admin source is the host's declared id verbatim). Customer filters on the
merchant list routes are `customer_id`.
Counts and timestamps are not money: counts remain numbers. Every timestamp on the
wire — response fields and request parameters alike (`created_at`, `expires_at`,
`occurred_at`, `at`, rollup `from`/`to`) — is an RFC3339 instant, including the
public product/price/payment objects and the admin credit grant's `expires_at`
(no Stripe-style epoch seconds remain); the Go client sends
`time.RFC3339Nano` and handlers accept any fractional precision. Durations stay
integer seconds (`window_seconds`, `retry_after_seconds`) and day buckets stay
`YYYY-MM-DD`. Missing optional timestamps are omitted; explicit nullable receipt
fields use null. Handlers encode instants as `time.Time` (RFC3339 at full
nanosecond precision), never through a hand-written second-precision format.
A subscription's `payments[]` history is the same `Payment` shape
`GET /v1/merchant/payments` serves (`created_at`, `status` `succeeded`,
`amount`), not a second summary shape.


The registry has one owner. Go consumers read it with `openrails.Currencies()`
/ `openrails.LookupCurrency(code)` (pure, no I/O); browsers fetch the same
table from the public `GET /v1/currencies` route. The admin UI's
`web/admin/src/lib/currency-units.json` is generated from it by
`go run ./scripts/currency-units` and pinned by a Go test. It formats exact decimal strings with BigInt/Intl, including
values beyond JavaScript's safe integer range; numeric money above 2^53 is shown
as out of range and rejected as input. No compatibility
parser accepts numeric money on these finalized routes during the pre-v1 cut.
Catalog application documents (`POST /merchant/catalog/applications` JSON; YAML files keep exact
integers), the admin customer billing profile balances and metrics money cells
(unit `money`, exact int64 sums; a money-unit ratio such as
`realized_revenue_per_customer` is the exact rational quotient rounded half
away from zero, never a float) use the same decimal strings. Finding evidence,
recommendation params and notification data are wire too and spell money the
same way; stored event payloads, row metadata and River job arguments are
internal records and keep integers. Provider rates (`/v1/solana/tokens`
`price`, `token_price_usd`, `fx_rate`) are decimal strings of the float the
feed quoted (`strconv.FormatFloat(rate, 'f', -1, 64)`): a rate is not money,
but the browser never parses a JSON float either.

`testdata/wire/*.json` are the canonical success, error, null, empty-list, list,
time and int64-boundary fixtures; Go (`wire_fixtures_test.go`) and the admin UI
(`web/admin/src/lib/api/wire-fixtures.test.ts`) both decode them.
`wire_money_guard_test.go` walks the root package, `pkg`, `internal`,
`embed`, `config` and `permissions` (all but SQLC output) and fails when a
monetary field (any integer, float or untyped `any`/`map[string]any`/
`json.RawMessage` whose JSON name names money) is not an int64/uint64 with
`,string`, when a `map[string]any` literal or `m["amount"] = v` assignment
carries a monetary key whose value is not spelled as a string, and when a
custom `MarshalJSON` is not pinned to the test that proves its encoding —
unless the site is listed with its reason (storage row, provider wire, job
args, stored metadata, log context, page size, count). Listed entries can only
shrink; `TestWireMoneyGuardDetects` proves each rule fires.

## Admin console browser support

The exact display path (`web/admin/src/lib/format.ts`) depends on:

| Feature | Used for | Minimum (MDN compat data) |
|---|---|---|
| `Intl.NumberFormat.prototype.format` with a decimal **string** argument (ECMA-402 2023) | rendering the exact major-unit decimal without a `Number` | Chrome/Edge 106, Firefox 116, Safari/iOS 15.4, Node 19 |
| `BigInt` (literals, `**`, `%`, `/`, comparisons) | scaling int64 units, input parsing, invoice arithmetic | Chrome/Edge 67, Firefox 68, Safari/iOS 14, Node 10.4 |
| `Object.hasOwn` | registry lookup | Chrome/Edge 93, Firefox 92, Safari/iOS 15.4 |
| `Intl.NumberFormat` options `style: "currency"`, `currency`, `maximumFractionDigits`, `useGrouping` (boolean) | currency and plain-number rendering | baseline (Chrome 24, Firefox 29, Safari 10) |

`format(bigint)` and `formatToParts` are not used. The minimum for exact
display of every int64 amount is therefore **Chrome/Edge 106, Firefox 116,
Safari/iOS 15.4**. The Vite build target (`baseline-widely-available`:
Chrome/Edge 111, Firefox 114, Safari 16.4) is the syntax floor; browsers
below it may fail to parse the bundle and show nothing.

On a browser inside the syntax floor but without exact decimal-string
formatting (Firefox 114–115), `format` coerces the string through `Number`.
`format.ts` probes this once at load (`intlFormatsDecimalStringsExactly`,
formatting `2^53 + 1`). On such an engine amounts below 10^15 native units are
still rendered, because a `Number` rounds back to those digits at every registry
scale, and larger amounts render as `<currency> amount exceeds this browser's
exact display range` instead of a silently rounded figure.
`src/lib/format-legacy-intl.test.ts` proves both halves against a coercing
`Intl.NumberFormat`. Without `BigInt` the bundle does not parse; without
`Object.hasOwn` the registry lookup throws — neither shows a wrong amount.
