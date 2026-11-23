# Dashboard

Each merchant has one saved dashboard: a grid of widgets, each a
[metrics](metrics.md) query with a visualization and a position. The console's
home page draws it; hosts read and replace it through the Go client. A merchant
that saved none gets a default template built in code. Every widget's query passes
the metrics validator before every save, so a saved dashboard holds only queries
the engine accepts at that time.

## Concepts

- **Widget** (`billing.DashboardWidget`): `id`, `title`, `viz`, `query` (a `billing.MetricsQuery`), `grid` and `fixed_range`.
- **Viz**: one of `dashboard.VizTypes`: `stat`, `line`, `area`, `bar`, `donut`, `table`.
- **Grid** (`billing.DashboardGrid`): `x`, `y`, `w`, `h` on the console's react-grid-layout grid.
- **Fixed range**: the widget keeps its query's own range (the default's first row: today against yesterday); other widgets follow the console's range picker, their saved range the default.
- **Layout**: the merchant's ordered widget list, stored whole.
- **Default template**: `DefaultWidgets(hasUsage)`, answered with `is_default: true` while nothing is saved.

## How it works

**Read.** `dashboard.Service.Get` reads `GetDashboardConfig` for the context's
merchant. With no row it runs `HasUsageActivity` (any `usage_daily` row, or a
purchased credit grant) and answers `DefaultWidgets`: usage widgets only for a
merchant with usage. Nothing is written until the merchant saves. A stored layout
is decoded as saved; a row that does not decode is an error. Stored widgets are
not validated again on read.

**Save.** `PUT /v1/merchant/dashboard` replaces the whole layout:

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
`layout jsonb`, `updated_at`, `updated_by`. The module runs no job.

## Interfaces

- Go: `Client.GetDashboard(ctx)` and `Client.SetDashboard(ctx, billing.SetDashboardParams)`;
  types `billing.Dashboard`, `DashboardWidget`, `DashboardGrid`, `SetDashboardParams`.
- HTTP: `GET /v1/metrics/dashboard` (`Metrics` group, `merchant:metrics:read`) and
  `PUT /v1/merchant/dashboard` (`MerchantConfig` group, update level,
  `merchant:config:manage`).
- Errors: `dashboard_invalid`, `service_unavailable`, `authentication_required`.
- Guide: [billing administration](../../sdk/billing-ui/README.md#staff-administration).

## Guarantees

### 1. Exactly-once provider calls
Does not apply: the dashboard calls no provider.

### 2. Agreement with providers
Does not apply: it stores presentation, not provider records.

### 3. Exact money
Does not apply: a layout holds queries, not amounts; the [metrics](metrics.md) engine keeps money exact when they run.

### 4. Access control and tenant isolation
Reading needs the metrics permission, saving the merchant-config manage
permission, both at the credential's merchant. Every read and write names the
context's merchant, and the table's key is that merchant.

Checks:
- `GET /v1/metrics/dashboard` is in the `Metrics` group and `PUT /v1/merchant/dashboard` in `MerchantConfig` at `LevelUpdate` (`internal/http/routes/ops.go`).
- `Service.Get` and `Service.Put` take the merchant from `merchant.Require`, never the body.
- `GetDashboardConfig`, `UpsertDashboardConfig` and `HasUsageActivity` all bind `merchant_id`.

### 5. Stable contracts
`GetDashboard` and `SetDashboard` are in `api/go.txt` for `*Client`, and both
routes are declared once in the catalog. A widget is a metrics query, so the
metrics vocabulary is part of this contract: a measure removed from the registry
leaves saved widgets whose queries answer `metrics_query_invalid`.

Checks:
- `wire.ts` carries `Dashboard`, `DashboardWidget` and `SetDashboardParams`.
- `DecodePut` decodes into `billing.DashboardWidget` with unknown fields refused, like the metrics API.
- Compare the console's hand-written `Dashboard` and `Widget` (`sdk/billing-ui/src/admin/lib/api/metrics.ts`) with `wire.ts`: `updated_at` and `updated_by` are optional there but `string | null` on the wire.

### 6. Migrations and retention
`dashboard_configs` is in the baseline with retention class state; a row lives as
long as its merchant. Merchant archives exclude it as presentation, so a restored
merchant starts from the default template.

Checks:
- `internal/retention/tables.go` classes `dashboard_configs` as `State`.
- `merchantarchive`'s `excludedTables` lists `dashboard_configs`.

### 7. Work scales with activity
Does not apply: the dashboard runs no background work.

### 8. Abuse protection
Does not apply: staff routes only, with no public or customer entry point; a layout is capped at 60 widgets.

### 9. Destructive-action safeguards
Does not apply: the only write replaces presentation; it is stamped with `updated_by` and undone by another save.

## Tests

- No critical scenario saves a dashboard or exercises its validation;
  `SetDashboard`, `dashboard_invalid` and widget errors are untested.
- OR-J38-S41 draws the default template in the console (stat labels, the one-rail
  donut, attempt failure rate by PSP), which reads `GET /v1/metrics/dashboard`.
- OR-J01-S02 probes both routes without a credential.
- `internal/modules/dashboard` has no tests of its own.
