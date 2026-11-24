# Reporting

Reporting is the metrics engine and the merchant dashboard. The engine answers a merchant's analytics questions (cash, payments, subscriptions, churn, declines, renewals, usage, credit, webhook health, findings) from OpenRails' own tables. An in-code registry of measures and dimensions validates a JSON query and compiles it to parameterized SQL, so no client string ever becomes SQL. It serves the console's dashboard, reports and ops signals, hosts through the Go client, and any agent holding a metrics credential. Each merchant has one saved dashboard: a grid of widgets, each a metrics query with a visualization and a position. The console's home page draws it; hosts read and replace it through the Go client. A merchant that saved none gets the health overview, built in code from what its data shows it sells. Every widget's query passes the metrics validator before every save, so a saved dashboard holds only queries the engine accepts at that time.

## Concepts

- **Measure**: a named aggregate with a class (`additive`, `distinct`, `ratio`, `difference`, `snapshot`), a unit (`money`, `count`, `ratio`, `days`, `seconds`), a group (`business` or `ops`), a direction (`up_is_good`, `down_is_good`, `neutral`), thresholds (a value and label a chart draws across it) and the dimensions it allows. Internal measures are components of others and cannot be requested.
- **Funnel**: one measure per fact, split by an outcome or kind dimension, never a measure per outcome. `attempts` (`outcome`: approved, declined), `purchases` (approved, recovered, failed, open), `rebills` (approved, recovered, lost, open), `checkouts` (`outcome`: paid, abandoned, open; `step`: created, opened, submitted, paid), `payments` (`payment_kind`) and `cash` (`cash_kind`) are funnels.
- **Slice**: a funnel measure restricted to dimension values (`Slice{"outcome": {"approved", "recovered"}}`). A rate is a ratio measure whose `NumSlice` and `DenSlice` slice its funnel; the registry builds each slice as an internal measure from the funnel's `Agg` and the dimension's own SQL expression, so a rate and the dimension it slices cannot disagree. A sliced rate allows its funnel's dimensions less the sliced ones unless it declares its own.
- **Dimension**: a group-by and filter axis: a closed `Values` list, a `Parse` for typed ids (`product_id`, `price_id`, `customer`), or free text. A family may give a dimension its own values (`DimValues`): `outcome` and `kind` differ by measure, and `/schema` lists them per measure as `dim_values`. `time` groups by the query's `grain`.
- **Family**: the source a leaf measure reads, such as `payments`, `subs_snapshot`, `attempts`, `rebill_cycles`, `usage_charged` or `findings`. One statement runs per family a query touches.
- **Range**: `{from, to}` as inclusive UTC days or RFC 3339 `[from, to)`, or `{last: "7d"}` (`Nd|Nw|Nm|Ny`, ending today).
- **Grain**: `day`, `week`, `month`, `quarter`, `year`, or `auto`: day up to 45 days, week up to 26 weeks, then month. The result names the grain used.
- **Plan**: a validated, compilable query (`metrics.Plan`).
- **Compare**: `previous_period` runs the same plan over the preceding window of equal length, `previous_year` over the same window a year before.
- **Removed**: `metrics.Removed` names each retired measure's replacement and `RemovedDimensions` each renamed dimension; a query naming one is refused with it. There are no aliases. `Deferred` lists the measures later work builds; naming one is refused as planned.
- **Widget** (`billing.DashboardWidget`): `id`, `title`, `viz`, `query` (a `billing.MetricsQuery`), `grid` and `fixed_range`.
- **Viz**: one of `dashboard.VizTypes`: `kpi`, `stat`, `line`, `area`, `bar`, `donut`, `table`, `funnel`, `heading`, `programs`.
- **Grid** (`billing.DashboardGrid`): `x`, `y`, `w`, `h` on the console's react-grid-layout grid.
- **Fixed range**: the widget keeps its query's own range; other widgets follow the console's range picker, their saved range the default.
- **Layout**: the merchant's ordered widget list, stored whole.
- **Profile** (`billing.DashboardProfile`): whether the merchant has subscriptions, card payments, metered usage, purchased credits and checkouts, read from data presence by `GetDashboardProfile`.
- **Health overview**: `DefaultWidgets(profile)`, answered with `is_default: true` and the profile while nothing is saved.

## Measures

Business measures: `cash`, `payments`, `refund_rate`, `chargeback_rate`, `disputes`, `dispute_win_rate`, `fraud_reports`, `new_subscriptions`, `cancellations`, `net_new_subscriptions`, `churn_rate`, `subscriptions`, `mrr`, `attempts`, `attempt_failure_rate`, `purchases`, `purchase_failure_rate`, `rebills`, `rebill_rate`, `dunning_recovery_rate`, `rebill_first_failure_rate`, `credits_sold`, `repeat_topup_rate`, `usage_charged`, `usage_units`, `active_customers`, `limits_reached`, `outstanding_credit_liability`, `outstanding_owed`, `customers_at_depletion_risk`, `checkouts`, `checkout_value`, `checkout_conversion_rate`, `checkout_submission_rate`, `checkout_customers`, `customer_conversion_rate`, `sales`, `sales_customers` and `repeat_purchase_rate`. `GET /v1/metrics/schema` gives each one's formula and dimensions.

- `cash` is signed money by `cash_kind`: a payment and a won dispute add, a refund and a chargeback subtract; its total is net cash, gross of processor fees.
- `payments` counts settled sales and reversals by `payment_kind`, a refund also by `refund_reason`; `refund_rate` and `chargeback_rate` are refunds and chargebacks over sales in the same bucket, by `network` (`billing.card_network` of the card brand) among others.
- `vamp_ratio` and `ecm_ratio` are the monitor's own ratios over `network_activity_daily`: Visa chargebacks and fraud reports over Visa sales in the bucket, and Mastercard chargebacks over Mastercard sales a month earlier (`network_prior`); use grain month. Their thresholds are the programs' levels (`disputerules.ProgramLines`).
- `disputes` counts dispute cases by when they became chargebacks, else opened, by `dispute_kind` (inquiry, chargeback, prevented, unknown), `dispute_reason`, `outcome` (open, won, lost, closed, unreported), `network` and `kind` (the disputed charge: initial or renewal); `dispute_win_rate` is won over won and lost. `fraud_reports` counts issuer fraud reports by `outcome`: what OpenRails did. A program's standing, which divides by the month's sales or the month before's, is the `programs` widget's (`GET /v1/metrics/dispute-programs`), not a measure.
- `purchases` and `rebills` take their outcome as of the range end: a purchase quiet for an hour without an approval has failed, a renewal past its 15-day dunning window uncollected is lost. `rebill_rate` is collected over closed renewals; `dunning_recovery_rate` is recovered over closed renewals whose first try failed or was missed.
- `usage_charged` is priced usage by `charge`: charged, or forgiven by the customer's grace.
- `checkouts` counts checkouts (orders OpenRails started for a customer to pay, with a total) by creation, with the `step` they reached and their `outcome` as of the range end ([orders](orders.md)); `checkout_value` sums their totals. `checkout_conversion_rate` is paid over settled checkouts whose payment form loaded (Stripe Checkout's completed over loaded); `checkout_submission_rate` the share of those whose customer asked to pay, so one less it is opened and left; `customer_conversion_rate` counts customers instead, whatever they retried. `sales` and `sales_customers` count paid checkouts by payment, `repeat_purchase_rate` the customers whose sale was not their first (`purchase_number`). Their dimensions are the checkout's facts: `country` and `country_source`, `language`, `attribution_source`, `attribution_medium`, `attribution_campaign`, `attribution_term`, `attribution_content`, `attribution_referrer`, `attribution_affiliate`, `purchase_number`, `origin` and `surface`; `cash` and `payments` take the facts of the checkout behind each payment, a renewal's being the checkout that bought its subscription.

## Ops view

The `ops` group is operator signals, queryable but off the health overview, each with a finding at its threshold: `open_findings` (by `finding_group`: orphaned, freeloader, duplicate_coverage, other), `verification_pressure`, `verification_pressure_age_seconds`, `webhook_silence_age_seconds`, `webhook_rejects`, `webhook_drift_events`, `nmi_history_authorizations` (by `outcome`) and `entitled_customers`. The console's Ops › Signals page lays them out with missed renewals by `miss_reason`, and Ops › Findings counts open findings by group.

## Health overview

`DefaultWidgets(profile)` is a row of `kpi` tiles, then a `heading` and its charts for each section the profile has data for. The tiles are Cash collected for everyone; Subscriptions, New subscriptions, Cancellations, Renewals in dunning (`rebills` open) and Rebill rate for subscriptions; Chargeback rate and Refund rate for card payments; Usage charged and Active customers for usage; Credits sold for credits; Checkout conversion for checkouts. The sections are Revenue and cash; Recurring and Retention for subscriptions; Payment health for subscriptions or card payments; Credits and usage for usage or credits; Checkout and customers for checkouts: the checkout funnel, abandoned checkouts by step, conversion by surface, customers by purchase number, the repeat purchase rate, and sales and cash by country, language and source. An empty merchant gets the tiles of a subscription business and no sections; the console reads them as "—" and points to catalog and PSP setup.

The widget kinds are general, so adding a tile or chart is adding a widget to `DefaultWidgets`:

- `kpi`: the value over the range (a snapshot at its end), its change against the comparison, coloured by the measure's `direction`, a ratio's in points, and a sparkline: the console runs the same query by `time` at grain `auto`.
- `heading`: a section title; its query is empty, which validation requires (`widget_has_query`).
- `programs`: each card PSP's standing in each dispute monitoring program for the month the page's range ends in: the ratio so far, projected, and its level; its query is empty too. Card payments' Payment health section carries it beside the chargeback rate against the networks' levels, the refund rate, disputes by reason and by the disputed charge's kind, the dispute win rate and fraud reports by outcome.
- `funnel`: one count measure by one dimension whose values are ordered steps, with no time (`funnel_shape` otherwise); each bar is the rows that reached its step or went further, with its share of the step before.
- `bar` and `area` stack signed values apart (`stackOffset` sign), so refunds stack below the axis.
- `line` draws each of its measures' `thresholds` as a reference line.
- The page's currency switch lists the currencies with cash, usage or credits in the range, defaults to the one with the most cash in major units, hides with one, and filters every widget all of whose measures allow `currency`. Money never sums across currencies.
- The page's comparison (`previous_period` or `previous_year`) applies to every widget that compares.

## How it works

**Query.**

1. `DecodeQuery` decodes the body strictly: an unknown key is `unknown_body_key` with a did-you-mean.
2. `ValidateAt(q, now)` checks measures, dimensions, grain (`Grains`), range, filters, order, limit and compare, and returns every error at once (`ValidationError` of `FieldError{code, param, message, did_you_mean, valid}`). A removed measure is `unknown_measure` whose message and `did_you_mean` name its replacement. A filter value is checked against each requested measure's own values. The handler answers `400 metrics_query_invalid` with them in `metadata.errors`. A relative range ends today on the deployment's clock; `auto` resolves to a grain from the range. A time series is capped at `MaxBuckets` (400), a result at `MaxLimit` (1,000) rows. A money measure without a single-currency filter adds `currency` as an implicit group-by.
3. `compile` groups the plan's leaf measures by family and builds one statement per family kind: `compileFlow` (rows whose time column is in the range, bucketed by `date_trunc`), `compileSnapshot` (state at each bucket edge, from `unnest($n::timestamptz[])`), `compileBalance` (cumulative ledger sums) and `compileDepletion` (prepaid balances against their trailing 7-day usage). Every SQL fragment comes from `families` and `Measures`; the merchant id, grain, bounds and filter values are bind arguments, and a filter is `expr = ANY($n)`. A slice's values are registry constants, inlined as checked literals.
4. `Service.run` executes the statements in one `MerchantTx`. `toLeaf` reads each aggregate exactly (bigint as `int64`, float8 for durations); groups are folded, time series zero-filled, ratios and differences computed with `big.Rat`, then rows ordered and limited. A money cell is a `MoneyCell`, encoded as a decimal string; a money ratio rounds half away from zero to whole native units (`moneyCellFromRat`), and a value outside `int64` is an error.
5. `Schema()` renders the registry for `GET /v1/metrics/schema`: measures with formulas, groups, directions, thresholds, dimensions and their values, grains, examples, `Caveats`, `Deferred` names and the limits.

The engine owns no table and runs no job. It reads `payments`, `order_lines`,
`prices`, `subscriptions`, `subscription_status_transitions`, `credit_grants`,
`usage_daily`, `limit_windows`, `product_access`, `product_entitlements`,
`ledger_transfers`, `ledger_accounts`, `webhook_health`, `webhook_health_daily`,
`psps`, `custodians`, `payment_attempts`, `payment_method_versions`,
`provider_intents`, `rebill_cycles`, `nmi_history_months`,
`reconciliation_findings`, `disputes`, `fraud_reports` and `network_activity_daily`. A measure sees only what those tables still retain:
payment attempts, status transitions, rebill cycles and NMI history 25 months;
daily usage 24 months after billing; webhook health days 13 months; resolved
findings 12 months.

**Adding a measure.** Declare it in `declared` (`registry.go`) with its family, `Expr` or `Agg`, dimensions, group and direction. A new outcome of an existing fact is a dimension value with its expression in the family's `DimExprs` and values in `DimValues`, and its rate a ratio over slices, not a new count. Removing or renaming a measure adds it to `Removed` and ships a migration that drops saved widgets naming it.

**Dashboard read.** `dashboard.Service.Get` reads `GetDashboardConfig` for the context's merchant. With no row it runs `GetDashboardProfile` and answers `DefaultWidgets(profile)` with the profile. Nothing is written until the merchant saves. A stored layout is decoded as saved; a row that does not decode is an error. Stored widgets are not validated again on read: the migration that retires a measure (`dashboard_retired_measures`) drops the widgets naming it, deleting a layout it leaves empty.

**Dashboard save.** `PUT /v1/merchant/dashboard` replaces the whole layout:

1. `DecodePut` decodes `{"widgets": [...]}` strictly: an unknown key anywhere,
   widget queries included, is `unknown_body_key`.
2. `ValidateWidgets` returns every error at once, each named by its widget (`widgets[i].<field>`): at most `MaxWidgets` (60); an `id` present and unique; a `title`; a `viz` from `VizTypes`; `x, y >= 0` and `w, h >= 1`; a heading's or programs widget's query empty (`widget_has_query`); and each other query through `metrics.ValidateAt`, its errors re-parameterized `widgets[i].query.<param>`. A failure is `400 dashboard_invalid` with the errors in `metadata.errors`.
3. The handler requires a bound staff identity (`authentication_required`
   otherwise); `UpsertDashboardConfig` stores the layout with `updated_at` and
   `updated_by` (the staff subject; `host` for an in-process call).

There is no revision check: the last save wins. Validation uses the wall clock
(`time.Now`); only relative-range bucket counts depend on it.

**Draw.** The console's dashboard page (`web/billing-ui/src/admin/pages/dashboard`) reads `GET /v1/metrics/schema` once (`registry.ts`) for directions, thresholds and dimensions, runs each widget's query through `POST /v1/metrics/query` under the page's range, currency and comparison (`widgetQuery`), and adds and edits widgets (`widget-editor.tsx`) before a save. The editor and the Reports page's explorer build queries from the schema (`query-builder.tsx`), so every measure can be charted by its dimensions without console changes.

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
- Guides: [metrics for LLMs](../client.md#metrics),
  [billing administration](../../web/billing-ui/README.md#staff-administration).

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
- `GetDashboardConfig`, `UpsertDashboardConfig` and `GetDashboardProfile` all bind `merchant_id`.
- OR-J01-S02 refuses the metrics and dashboard routes without a valid credential.

### 5. Contracts match the code
`QueryMetrics`, `GetMetricsSchema`, `GetDashboard` and `SetDashboard` are in
`api/go.txt` for `*Client`; their routes are declared once in
`internal/http/routes/ops.go`, and the in-process client calls the same handlers.
The vocabulary itself (measure and dimension names) is in no generated snapshot, yet a widget is a metrics query, so it is part of the dashboard's contract: a removed measure is refused naming its replacement, and its removal ships a migration that drops saved widgets naming it.

Checks:
- `api/openapi.json` and `wire.ts` carry `MetricsQuery`, `MetricsResult` and `MetricsSchema`; `wire.ts` carries `Dashboard`, `DashboardProfile`, `DashboardWidget` and `SetDashboardParams`.
- `Schema().Limits` reports `MaxBuckets` and `MaxLimit`, the same constants validation uses.
- `DecodePut` decodes into `billing.DashboardWidget` with unknown fields refused, like the metrics API.
- `registry` panics at start on a measure without a direction, one both live and removed, a slice of a measure without `Agg`, or a slice value outside its dimension's values.
- `TestRetiredMeasuresLeaveSavedLayouts` fails when the dashboard migration misses a name in `Removed`.
- Compare the console's hand-written `Dashboard` and `Widget` (`web/billing-ui/src/admin/lib/api/metrics.ts`) with `wire.ts`: `updated_at` and `updated_by` are optional there but `string | null` on the wire.

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

- OR-J53-S01 buys a membership and four orders on one merchant (a refund, a chargeback and its won dispute, a decline recovered by another card, a decline left) and checks `cash`, `payments`, `attempts`, `purchases` and `rebills` by kind and outcome, and their rates, against the measures they replace; a removed measure and `attempt_kind` are refused naming their replacements.
- OR-J53-S02 checks the overview a subscription and card merchant gets, that every widget answers under each currency of the switch with only that currency's cash, that an empty merchant's tiles answer, that every profile's overview saves, that a removed measure cannot be saved, and that the dashboard migration drops saved widgets naming one.
- OR-J53-S03 checks that every money measure splits by currency unless one is named, by PSP and over time too, and that money beside a measure without currency is refused.
- OR-J46-S12 checks the dispute measures against their records (disputes by kind, reason, outcome and charge kind, the win rate, the chargeback rate by network and its lines, fraud reports, refunds by reason) and that a card merchant's overview carries the `programs` widget.
- OR-J14-S70 (`rebills` missed), OR-J17-S01 (`mrr`) and OR-J23-S16 (`limits_reached`) query through the Go client over real PostgreSQL.
- OR-J38-S41 draws the health overview in the console (its sections, the cash donut and the attempts table naming the PSP by key, tile labels that fit), which reads `GET /v1/metrics/dashboard`.
- OR-J38-S34 counts `POST /v1/metrics/query` as a read when proving a configuration reader writes nothing.
- OR-J01-S02 probes the metrics and dashboard routes without a credential.
- `internal/modules/metrics` and `internal/modules/dashboard` check the registry, removed names, `auto` grain and every profile's layout without a database.
- Gaps: no test covers money ratios, the snapshot, balance and depletion families beyond currency splits, or one merchant's query against another's rows. No statement timeout bounds a slow query; only request cancellation stops it.
