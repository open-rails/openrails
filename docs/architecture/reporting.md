# Reporting

Reporting is the metrics engine and the merchant dashboard. The engine answers a
merchant's analytics questions (revenue, subscriptions, churn, declines, rebills,
usage, credit, webhook health, findings) from OpenRails' own tables. An in-code
registry of measures and dimensions validates a JSON query and compiles it to
parameterized SQL, so no client string ever becomes SQL. It serves the console's
dashboard, reports and findings bell, hosts through the Go client, and any agent
holding a metrics credential. Each merchant has one saved dashboard: a grid of
widgets, each a metrics query with a visualization and a position. The console's
home page draws it; hosts read and replace it through the Go client. A merchant
that saved none gets a default template built in code. Every widget's query
passes the metrics validator before every save, so a saved dashboard holds only
queries the engine accepts at that time.

## Concepts

- **Measure**: a named aggregate with a class (`additive`, `distinct`, `ratio`, `difference`, `snapshot`), a unit (`money`, `count`, `ratio`, `days`, `seconds`) and the dimensions it allows. Internal measures are components of others and cannot be requested.
- **Dimension**: a group-by and filter axis: a closed `Values` list, a `Parse` for typed ids (`product_id`, `price_id`, `customer`), or free text. `time` groups by the query's `grain`.
- **Family**: the source a leaf measure reads, such as `payments`, `subs_snapshot`, `attempts`, `balance` or `findings`. One statement runs per family a query touches.
- **Range**: `{from, to}` as inclusive UTC days or RFC 3339 `[from, to)`, or `{last: "7d"}` (`Nd|Nw|Nm|Ny`, ending today).
- **Plan**: a validated, compilable query (`metrics.Plan`).
- **Compare**: `previous_period` runs the same plan over the preceding window of equal length.
- **Widget** (`billing.DashboardWidget`): `id`, `title`, `viz`, `query` (a `billing.MetricsQuery`), `grid` and `fixed_range`.
- **Viz**: one of `dashboard.VizTypes`: `stat`, `line`, `area`, `bar`, `donut`, `table`.
- **Grid** (`billing.DashboardGrid`): `x`, `y`, `w`, `h` on the console's react-grid-layout grid.
- **Fixed range**: the widget keeps its query's own range (the default's first row: today against yesterday); other widgets follow the console's range picker, their saved range the default.
- **Layout**: the merchant's ordered widget list, stored whole.
- **Default template**: `DefaultWidgets(hasUsage)`, answered with `is_default: true` while nothing is saved.

## How it works

**Query.**

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

**Dashboard read.** `dashboard.Service.Get` reads `GetDashboardConfig` for the
context's merchant. With no row it runs `HasUsageActivity` (any `usage_daily` row,
or a purchased credit grant) and answers `DefaultWidgets`: usage widgets only for
a merchant with usage. Nothing is written until the merchant saves. A stored
layout is decoded as saved; a row that does not decode is an error. Stored
widgets are not validated again on read.

**Dashboard save.** `PUT /v1/merchant/dashboard` replaces the whole layout:

1. `DecodePut` decodes `{"widgets": [...]}` strictly: an unknown key anywhere,
   widget queries included, is `unknown_body_key`.
2. `ValidateWidgets` returns every error at once, each named by its widget
   (`widgets[i].<field>`): at most `MaxWidgets` (60); an `id` present and unique;
   a `title`; a `viz` from `VizTypes`; `x, y >= 0` and `w, h >= 1`; and each
   query through `metrics.ValidateAt`, its errors re-parameterized
   `widgets[i].query.<param>`. A failure is `400 dashboard_invalid` with the
   errors in `metadata.errors`.
3. The handler requires a bound staff identity (`authentication_required`
   otherwise); `UpsertDashboardConfig` stores the layout with `updated_at` and
   `updated_by` (the staff subject; `host` for an in-process call).

There is no revision check: the last save wins. Validation uses the wall clock
(`time.Now`); only relative-range bucket counts depend on it.

**Draw.** The console's dashboard page (`sdk/billing-ui/src/admin/pages/dashboard`)
runs each widget's query through `POST /v1/metrics/query`, applies the range
picker to widgets without `fixed_range`, and edits widgets
(`widget-editor.tsx`) before a save.

**Storage.** One table, `dashboard_configs`: `merchant_id` (primary key),
`layout jsonb`, `updated_at`, `updated_by`.

## Interfaces

- Go: `Client.QueryMetrics(ctx, billing.MetricsQuery)`, `Client.GetMetricsSchema`,
  `Client.GetDashboard(ctx)` and `Client.SetDashboard(ctx, billing.SetDashboardParams)`;
  types `billing.MetricsQuery`, `MetricsRange`, `MetricsOrder`, `MetricsResult`,
  `MetricsColumn`, `MetricsSchema`, `Dashboard`, `DashboardWidget`, `DashboardGrid`,
  `SetDashboardParams`.
- HTTP: `POST /v1/metrics/query`, `GET /v1/metrics/schema` and
  `GET /v1/metrics/dashboard`, in the `Metrics` route group (`merchant:metrics:read`
  or `root:metrics:read`), mounted only when the host turns on
  `RouteGroups.Metrics`; `PUT /v1/merchant/dashboard` (`MerchantConfig` group,
  update level, `merchant:config:manage`).
- Errors: `metrics_query_invalid`, `dashboard_invalid`, `service_unavailable`,
  `authentication_required`.
- Guides: [metrics for LLMs](../metrics-for-llms.md),
  [billing administration](../billing-admin.md).

## Guarantees

### 1. Exactly-once provider calls
Does not apply: reporting reads local tables and calls no provider.

### 2. Agreement with providers
Does not apply: it reports what OpenRails recorded and stores presentation; the systems that write those rows keep them in agreement.

### 3. Exact money
Money leaves are SQL integer sums cast to `bigint`, carried as `int64` and written
as decimal strings. Money never sums across currencies: `currency` becomes a
group-by unless one currency is filtered. Ratios divide exact rationals and round
once. A dashboard layout holds queries, not amounts.

Checks:
- Every money measure that is not a ratio or difference casts its `Expr` to `::bigint`.
- `ValidateAt` sets `ImplicitCurrency` when a money measure has no single-currency filter.
- `leaf.add` and `moneyCellFromRat` refuse a value outside `int64`.
- A `currency` filter is normalized by `money.NormalizeCurrency` before binding.

### 4. Access control and tenant isolation
Over HTTP the metrics routes check `needMetrics`; saving a dashboard needs the
merchant-config manage permission. The in-process `Client` checks no permission,
as everywhere. The merchant comes from the credential or handle
(`merchant.Require`), never the query or body. Every metrics statement's first
predicate binds the merchant id to the family's `merchantCols` column, its joins
match `merchant_id`, and all run in one `MerchantTx`; every dashboard read and
write names the context's merchant, the table's key. Results are aggregates; the
`customer` dimension groups by customer UUID and returns nothing else about the
customer.

Checks:
- `merchantCols` names a column for every family but `FamDepletion`, whose SQL binds `$1` in both subqueries.
- Every `DimJoins` and `From` join in `registry.go` joins on `merchant_id`.
- `GET /v1/metrics/dashboard` is in the `Metrics` group and `PUT /v1/merchant/dashboard` in `MerchantConfig` at `LevelUpdate` (`internal/http/routes/ops.go`).
- `Service.Get` and `Service.Put` take the merchant from `merchant.Require`, never the body.
- `GetDashboardConfig`, `UpsertDashboardConfig` and `HasUsageActivity` all bind `merchant_id`.
- OR-J01-S02 refuses the metrics and dashboard routes without a valid credential.

### 5. Contracts match the code
`QueryMetrics`, `GetMetricsSchema`, `GetDashboard` and `SetDashboard` are in
`api/go.txt` for `*Client`; their routes are declared once in
`internal/http/routes/ops.go`, and the in-process client calls the same handlers.
The vocabulary itself (measure and dimension names) is in no generated snapshot,
yet a widget is a metrics query, so it is part of the dashboard's contract: a
measure removed from the registry leaves saved widgets whose queries answer
`metrics_query_invalid`.

Checks:
- `api/openapi.json` and `wire.ts` carry `MetricsQuery` and `MetricsResult`; `wire.ts` carries `Dashboard`, `DashboardWidget` and `SetDashboardParams`.
- `Schema().Limits` reports `MaxBuckets` and `MaxLimit`, the same constants validation uses.
- `DecodePut` decodes into `billing.DashboardWidget` with unknown fields refused, like the metrics API.
- Renaming or removing a `Measures` entry fails no contract check; review such a change as a break.
- Compare the console's hand-written `Dashboard` and `Widget` (`sdk/billing-ui/src/admin/lib/api/metrics.ts`) with `wire.ts`: `updated_at` and `updated_by` are optional there but `string | null` on the wire.

### 6. Data lifecycle
The engine owns no table. Every flow statement bounds its time column by the range;
no family reads a partitioned table (`limit_holds`, `limit_hold_parts`).
`dashboard_configs` has retention class state; a row lives as long as its
merchant. Merchant archives exclude it as presentation, so a restored merchant
starts from the default template.

Checks:
- `compileFlow` adds `TimeExpr >= from` and `TimeExpr < to` to every flow statement.
- No family's `From` names a table in `retention.Partitioned`.
- `internal/retention/tables.go` classes `dashboard_configs` as `State`.
- `merchantarchive`'s `excludedTables` lists `dashboard_configs`.

### 7. Work scales with activity
Does not apply: reporting runs no background work; a query runs at request time, capped at 400 buckets and 1,000 rows.

### 8. Abuse protection
Does not apply: staff and application routes only, with no public or customer entry point; OpenRails rate-limits no metrics route, and a layout is capped at 60 widgets.

### 9. Destructive-action safeguards
Does not apply: the engine is read-only, and the dashboard's only write replaces presentation; it is stamped with `updated_by` and undone by another save.

## Tests

- OR-J14-S70 (`rebills_missed`), OR-J17-S01 (`mrr`) and OR-J23-S16
  (`limits_reached`) query through the Go client over real PostgreSQL.
- OR-J38-S41 draws the default dashboard template in the console (stat tiles, a
  one-rail donut, attempt failure rate by PSP), which reads
  `GET /v1/metrics/dashboard`.
- OR-J38-S34 counts `POST /v1/metrics/query` as a read when proving a
  configuration reader writes nothing.
- OR-J01-S02 probes the metrics and dashboard routes without a credential.
- Gaps: no test covers validation errors and their payload, money ratios, implicit
  currency grouping, `compare`, the snapshot, balance and depletion families, or
  one merchant's query against another's rows. No critical scenario saves a
  dashboard or exercises its validation; `SetDashboard`, `dashboard_invalid` and
  widget errors are untested. `internal/modules/metrics` and
  `internal/modules/dashboard` have no tests of their own. No statement timeout
  bounds a slow query; only request cancellation stops it.
