# Money, ids and times on the wire

## Money

In Go an amount is a signed `int64` in the currency's native units; on the wire
it is a decimal string of the same integer, named `amount` (or `<thing>_amount`)
beside a `currency`. No field name carries a currency.

```json
{"amount": "9990000", "currency": "USD"}
```

- **Native units** are the currency's registered scale: micros for USD and EUR
  (`"1234567"` is 1.234567 USD), 10^4 per yen for JPY. `GET /v1/config`'s
  `currencies` list each currency's `decimals` (the native scale) and `minor_decimals`
  (what providers settle in); `billing.Currencies()` is the same table in Go.
- **Every monetary value is a string**: prices, payments and refunds, balances,
  holds and captures, credit grants and transactions, usage, invoices, limits
  and spend windows, rate cards, tier-change amounts, and the checkout session
  document. A JavaScript consumer uses `BigInt` or an exact decimal library;
  converting to `Number` loses values above 2^53. A numeric amount in a request
  is refused.
- **Currency codes** are upper-case ISO 4217 spelling on every response,
  whatever case a request sent.
- **Rates are not money.** Provider rates (`/v1/solana/tokens` `price`,
  `token_price_usd`, `fx_rate`) are decimal strings of the quoted float.
- Counts are numbers. Durations are integer seconds (`window_seconds`,
  `retry_after_seconds`); day buckets are `YYYY-MM-DD`.

The checkout session document also carries `plan.unit_decimals`, the scale of
its currency, so a payment page formats amounts without a second request.

## Ids

A resource's id is typed, prefixed text on every body, path and query
parameter that names it, and only that spelling is accepted: a bare UUID or
another kind's prefix is `400 invalid_param`.

| Prefix | Resource | Prefix | Resource |
|---|---|---|---|
| `prod_` | product | `price_` | price |
| `sub_` | subscription | `pay_` | payment |
| `pm_` | payment method | `inv_` | invoice |
| `psp_` | PSP | `chk_` | checkout attempt |
| `ocs_` | checkout session | `att_` | payment attempt |
| `cyc_` | rebill cycle | | |
| `cgr_` | credit grant | `txn_` | balance transaction |
| `ent_` | entitlement | `pa_` | product access |
| `pmig_` | price migration | | |
| `uev_` | usage event | `hev_` | host event |
| `ntf_` | notification | `fnd_` | finding |
| `awh_` | alert webhook | `pop_` | payment operation |
| `par_` | product archive | | |

Customer and merchant ids are plain UUIDs. A price or product *key* is a
separate, opaque handle (`price_key`, `product_key`, the `keys` and `key` list filters); a
field that takes an id never accepts a key. In Go the ids are types
(`billing.PriceID`, `billing.CustomerID`, …): the zero id marshals as `""`, and
a Client refuses one before any request.

An entitlement's or grant's `source_id` is the source resource's own id beside
`source_type`: `sub_` for a subscription or grace source, `pay_` for a
purchase; an `admin` source is the grant itself.

## Times

Every timestamp, in responses and requests alike, is an RFC 3339 instant in
UTC (`created_at`, `expires_at`, `occurred_at`, `at`, `from`, `to`). Handlers
accept any fractional precision. A timestamp with no value is `null`.

## One registry

The registry has one owner. Go consumers read it with `billing.Currencies()`
/ `billing.LookupCurrency(code)` (pure, no I/O); browsers fetch the same
table from the `currencies` of the public `GET /v1/config`. The admin UI's
`web/admin/src/lib/currency-units.json` is generated from it by
`go run ./scripts/currency-units` and pinned by a Go test. It formats exact decimal strings with BigInt/Intl, including
values beyond JavaScript's safe integer range; numeric money above 2^53 is shown
as out of range and rejected as input.
Catalog application documents (`POST /v1/admin/catalog/applications` JSON; YAML files keep exact
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
`wire_money_guard_test.go` walks the root package, `billing`, `catalog`
and `internal` (all but SQLC output) and fails when a
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
