# Money on the HTTP wire

Go uses signed int64 native currency units. Deposit, ledger receipt, balance,
checkout-session, capture, admission, usage-report, wasted-spend, invoice,
merchant settings/billing policy, spend delegation and self spend-window,
credit-limit, usage-rollup, resource-revenue and control-plane fleet
analytics/timeseries DTOs encode monetary values as decimal JSON strings under
`<thing>_amount` names (`settled_amount`, `monthly_amount`); the currency's
registered scale is the unit. Invoice movement maps use the same
representation. A JavaScript
consumer must use BigInt or an exact decimal library for arithmetic; converting
to Number before parsing loses values above 2^53.

Invoice responses include unit_decimals. For USD, "1234567" with scale 6 means
1.234567 USD. JPY uses its declared scale, not an assumed cents/micros scale.
Counts and timestamps are not money: counts remain numbers. Every timestamp on the
wire — response fields and request parameters alike (`expires_at`, `occurred_at`,
`at`, rollup `from`/`to`) — is an RFC3339 instant; the Go client sends
`time.RFC3339Nano` and handlers accept any fractional precision. Durations stay
integer seconds (`window_seconds`, `retry_after_seconds`) and day buckets stay
`YYYY-MM-DD`. Missing optional timestamps are omitted; explicit nullable receipt
fields use null.
Currency codes are the registry's uppercase ISO-4217 spelling on every wire
surface, whatever case a request sent; requests are read case-insensitively.
Identifiers of resource kinds `pkg/api` prefixes (`prod_`, `price_`, `sub_`,
`pay_`, `pm_`, `cs_`) travel prefixed on every DTO that names them, and every
operation accepts the prefixed form; customer, merchant and PSP ids are plain UUIDs.
Counts and timestamps are not money: counts remain numbers, and invoice/receipt
timestamps use RFC3339 with fractional seconds. Missing optional timestamps are
omitted; explicit nullable receipt fields use null.

The registry has one owner. Go consumers read it with `openrails.Currencies()`
/ `openrails.LookupCurrency(code)` (pure, no I/O); browsers fetch the same
table from the public `GET /v1/currencies` route. The admin UI's
`web/admin/src/lib/currency-units.json` is generated from it by
`go run ./scripts/currency-units` and pinned by a Go test. It formats exact decimal strings with BigInt/Intl, including
values beyond JavaScript's safe integer range; numeric money above 2^53 is shown
as out of range and rejected as input. No compatibility
parser accepts numeric money on these finalized routes during the pre-v1 cut.
Remaining unconverted endpoints are tracked in #983 and are not frozen yet.

`testdata/wire/*.json` are the canonical success, error, null, empty-list, list,
time and int64-boundary fixtures; Go (`wire_fixtures_test.go`) and the admin UI
(`web/admin/src/lib/api/wire-fixtures.test.ts`) both decode them.
`wire_money_guard_test.go` fails when a monetary int64 field in a wire package
is a JSON number unless it is listed, with its reason, as a pending #983 gap.
Map-literal responses, metrics cells and finding evidence are outside that
static guard.

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
