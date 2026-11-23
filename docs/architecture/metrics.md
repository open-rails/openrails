# Metrics

The metrics engine answers a merchant's analytics questions (revenue, subscriptions,
churn, declines, rebills, usage, credit, webhook health, findings) from OpenRails'
own tables. An in-code registry of measures and dimensions validates a JSON query
and compiles it to parameterized SQL, so no client string ever becomes SQL. It
serves the console's [dashboard](dashboard.md), reports and findings bell, hosts through the
Go client, and any agent holding a metrics credential. Integration guide:
[metrics for LLMs](../metrics-for-llms.md).

## Concepts

- **Measure**: a named aggregate with a class (`additive`, `distinct`, `ratio`, `difference`, `snapshot`), a unit (`money`, `count`, `ratio`, `days`, `seconds`) and the dimensions it allows. Internal measures are components of others and cannot be requested.
- **Dimension**: a group-by and filter axis: a closed `Values` list, a `Parse` for typed ids (`product_id`, `price_id`, `customer`), or free text. `time` groups by the query's `grain`.
- **Family**: the source a leaf measure reads, such as `payments`, `subs_snapshot`, `attempts`, `balance` or `findings`. One statement runs per family a query touches.
- **Range**: `{from, to}` as inclusive UTC days or RFC 3339 `[from, to)`, or `{last: "7d"}` (`Nd|Nw|Nm|Ny`, ending today).
- **Plan**: a validated, compilable query (`metrics.Plan`).
- **Compare**: `previous_period` runs the same plan over the preceding window of equal length.

## How it works

1. `DecodeQuery` decodes the body strictly: an unknown key is `unknown_body_key`
   with a did-you-mean.
2. `ValidateAt(q, now)` checks measures, dimensions, grain (`Grains`), range,
   filters, order, limit and compare, and returns every error at once
   (`ValidationError` of `FieldError{code, param, message, did_you_mean, valid}`).
   The handler answers `400 metrics_query_invalid` with them in
   `metadata.errors`. A relative range ends today on the deployment's clock. A time
   series is capped at `MaxBuckets` (400), a result at `MaxLimit` (1,000) rows. A
   money measure without a single-currency filter adds `currency` as an implicit
   group-by.
3. `compile` groups the plan's leaf measures by family and builds one statement per
   family kind: `compileFlow` (rows whose time column is in the range, bucketed by
   `date_trunc`), `compileSnapshot` (state at each bucket edge, from
   `unnest($n::timestamptz[])`), `compileBalance` (cumulative ledger sums) and
   `compileDepletion` (prepaid balances against their trailing 7-day usage). Every
   SQL fragment comes from `families` and `Measures`; the merchant id, grain,
   bounds and filter values are bind arguments, and a filter is `expr = ANY($n)`.
4. `Service.run` executes the statements in one `MerchantTx`. `toLeaf` reads each
   aggregate exactly (bigint as `int64`, float8 for durations); groups are folded,
   time series zero-filled, ratios and differences computed with `big.Rat`, then
   rows ordered and limited. A money cell is a `MoneyCell`, encoded as a decimal
   string; a money ratio rounds half away from zero to whole native units
   (`moneyCellFromRat`), and a value outside `int64` is an error.
5. `Schema()` renders the registry for `GET /v1/metrics/schema`: measures with
   formulas, dimensions with values, grains, examples, `Caveats`, `Deferred` names
   and the limits.

The engine owns no table and runs no job. It reads `payments`, `order_lines`,
`prices`, `subscriptions`, `subscription_status_transitions`, `grants`,
`usage_daily`, `limit_windows`, `product_access`, `product_entitlements`,
`ledger_transfers`, `ledger_accounts`, `webhook_health`, `webhook_health_daily`,
`psps`, `custodians`, `payment_attempts`, `payment_method_versions`,
`provider_intents`, `rebill_cycles`, `nmi_history_months` and
`reconciliation_findings`. A measure sees only what those tables still retain:
payment attempts, status transitions, rebill cycles and NMI history 25 months;
daily usage 24 months after billing; webhook health days 13 months; resolved
findings 12 months.

## Interfaces

- Go: `Client.QueryMetrics(ctx, billing.MetricsQuery)` and `Client.GetMetricsSchema`;
  types `billing.MetricsQuery`, `MetricsRange`, `MetricsOrder`, `MetricsResult`,
  `MetricsColumn`, `MetricsSchema`.
- HTTP: `POST /v1/metrics/query` and `GET /v1/metrics/schema`, in the `Metrics`
  route group (`merchant:metrics:read` or `root:metrics:read`), mounted only when
  the host turns on `RouteGroups.Metrics`.
- Errors: `metrics_query_invalid`, `service_unavailable`.

## Guarantees

### 1. Exactly-once provider calls
Does not apply: the engine reads local tables and calls no provider.

### 2. Agreement with providers
Does not apply: it reports what OpenRails recorded; the systems that write those rows keep them in agreement.

### 3. Exact money
Money leaves are SQL integer sums cast to `bigint`, carried as `int64` and written
as decimal strings. Money never sums across currencies: `currency` becomes a
group-by unless one currency is filtered. Ratios divide exact rationals and round
once.

Checks:
- Every money measure that is not a ratio or difference casts its `Expr` to `::bigint`.
- `ValidateAt` sets `ImplicitCurrency` when a money measure has no single-currency filter.
- `leaf.add` and `moneyCellFromRat` refuse a value outside `int64`.
- A `currency` filter is normalized by `money.NormalizeCurrency` before binding.

### 4. Access control and tenant isolation
Over HTTP both routes check `needMetrics`; the in-process `Client` checks no
permission, as everywhere. The merchant comes from the credential or handle
(`merchant.Require`), never the query. Every statement's first predicate binds the
merchant id to the family's `merchantCols` column, its joins match `merchant_id`,
and all run in one `MerchantTx`. Results are aggregates; the `customer` dimension
groups by customer UUID and returns nothing else about the customer.

Checks:
- `merchantCols` names a column for every family but `FamDepletion`, whose SQL binds `$1` in both subqueries.
- Every `DimJoins` and `From` join in `registry.go` joins on `merchant_id`.
- OR-J01-S02 refuses both routes without a valid credential.

### 5. Stable contracts
`QueryMetrics` and `GetMetricsSchema` are in `api/go.txt` for `*Client`; both routes
are declared once in `internal/http/routes/ops.go`, and the in-process client calls
the same handlers. The vocabulary itself (measure and dimension names) is in no
generated snapshot.

Checks:
- `api/openapi.json` and `wire.ts` carry `MetricsQuery` and `MetricsResult`.
- `Schema().Limits` reports `MaxBuckets` and `MaxLimit`, the same constants validation uses.
- Renaming or removing a `Measures` entry fails no contract check; review such a change as a break.

### 6. Migrations and retention
The engine owns no table. Every flow statement bounds its time column by the range;
no family reads a partitioned table (`limit_holds`, `limit_hold_parts`).

Checks:
- `compileFlow` adds `TimeExpr >= from` and `TimeExpr < to` to every flow statement.
- No family's `From` names a table in `retention.Partitioned`.

### 7. Work scales with activity
Does not apply: the engine runs no background work; a query runs at request time, capped at 400 buckets and 1,000 rows.

### 8. Abuse protection
Does not apply: staff and application routes only, with no public or customer entry point; OpenRails rate-limits no metrics route.

### 9. Destructive-action safeguards
Does not apply: the engine is read-only.

## Tests

- OR-J14-S70 (`rebills_missed`), OR-J17-S01 (`mrr`) and OR-J23-S16
  (`limits_reached`) query through the Go client over real PostgreSQL.
- OR-J38-S41 renders the default dashboard in the console: stat tiles, a one-rail
  donut, attempt failure rate by PSP.
- OR-J38-S34 counts `POST /v1/metrics/query` as a read when proving a
  configuration reader writes nothing.
- OR-J01-S02 probes both routes without a credential.
- Gaps: no test covers validation errors and their payload, money ratios, implicit
  currency grouping, `compare`, the snapshot, balance and depletion families, or
  one merchant's query against another's rows. `internal/modules/metrics` has no
  tests of its own. No statement timeout bounds a slow query; only request
  cancellation stops it.
