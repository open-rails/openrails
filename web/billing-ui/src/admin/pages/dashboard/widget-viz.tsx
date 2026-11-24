import { AdminMessage, useAdminUI } from "#orck/admin/ui-context"
// Viz renderers: kpi (value, change and sparkline), stat, line/area/bar
// (shadcn chart on recharts), donut, table. Input is the raw tabular metrics
// result; nothing re-queries client-side. Direction and thresholds come from
// the registry, so any measure draws.
import type { ComponentProps } from "react"
import { useQuery } from "@tanstack/react-query"
import { HugeiconsIcon } from "@hugeicons/react"
import { AnalyticsDownIcon, AnalyticsUpIcon } from "@hugeicons/core-free-icons"
import {
  Area,
  AreaChart,
  Bar,
  BarChart,
  CartesianGrid,
  Cell,
  Line,
  LineChart,
  Pie,
  PieChart,
  ReferenceLine,
  XAxis,
  YAxis,
} from "recharts"

import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "#orck/admin/components/ui/table"
import {
  ChartContainer,
  ChartLegend,
  ChartLegendContent,
  ChartTooltip,
  ChartTooltipContent,
  type ChartConfig,
} from "#orck/admin/components/ui/chart"
import type {
  MetricsResult,
  MetricsThreshold,
  QueryMetricsParams,
  WidgetViz as VizType,
} from "#orck/admin/lib/api/metrics"
import { cn } from "#orck/admin/lib/utils"
import { useAdminServices } from "#orck/admin/context"
import { useMerchantConfig } from "#orck/admin/lib/capabilities"

import {
  changeText,
  comparedWith,
  donutSlices,
  chartColor,
  exactKey,
  filteredCurrency,
  groupSeries,
  rowCurrency,
  statDelta,
  type PivotSeries,
  type Pivoted,
  formatBucket,
  formatMeasure,
  funnelSteps,
  indexColumns,
  pivotTimeSeries,
} from "./lib"
import { registry as emptyRegistry, type Registry } from "./registry"

export function WidgetVizView({
  viz,
  result,
  query,
  spark,
  registry = emptyRegistry(),
}: {
  viz: VizType
  result: MetricsResult
  query?: QueryMetricsParams
  // spark is a kpi's query by time.
  spark?: MetricsResult
  registry?: Registry
}) {
  const currency = filteredCurrency(query)
  switch (viz) {
    case "kpi":
      return (
        <KpiViz
          result={result}
          spark={spark}
          currency={currency}
          registry={registry}
        />
      )
    case "stat":
      return <StatViz result={result} currency={currency} registry={registry} />
    case "line":
    case "area":
    case "bar":
      return (
        <TimeSeriesViz
          viz={viz}
          result={result}
          currency={currency}
          registry={registry}
        />
      )
    case "donut":
      return <DonutViz result={result} currency={currency} />
    case "table":
      return <TableViz result={result} currency={currency} />
    case "funnel":
      return <FunnelViz result={result} query={query} registry={registry} />
    case "heading":
    case "programs":
      return null
  }
}

// usePSPKeys names a PSP by its key, for those who may read the PSPs; an
// unread PSP keeps its id.
export function usePSPKeys(enabled: boolean): (id: string) => string {
  const { adminQueries } = useAdminServices()
  const config = useMerchantConfig()
  const psps = useQuery({
    ...adminQueries.psps(),
    // The widget raises no toast.
    meta: undefined,
    enabled: config && enabled,
  }).data?.data
  return (id) => psps?.find((p) => p.id === id)?.key ?? id
}

// better says whether a change of a measure is good news: null when its
// direction is neutral.
function better(
  registry: Registry,
  measure: string,
  delta: number
): boolean | null {
  switch (registry.direction(measure)) {
    case "up_is_good":
      return delta > 0
    case "down_is_good":
      return delta < 0
    default:
      return null
  }
}

function Change({
  delta,
  unit,
  good,
  against,
}: {
  delta: number
  unit?: string
  good: boolean | null
  against: string
}) {
  // Shown to one decimal: what rounds to 0.0 is no change.
  const moved = Math.round(delta * 1000) !== 0
  const none = unit === "ratio" ? "0.0 pp" : "0.0%"
  return (
    <span
      className={cn(
        "flex items-center gap-1 text-xs font-medium",
        !moved || good === null
          ? "text-muted-foreground"
          : good
            ? "text-settled"
            : "text-failed"
      )}
    >
      {moved && (
        <HugeiconsIcon
          icon={delta > 0 ? AnalyticsUpIcon : AnalyticsDownIcon}
          className="size-3"
        />
      )}
      {`${moved ? changeText(delta, unit) : none} vs ${against}`}
    </span>
  )
}

// KpiViz is a tile's value over the range (a snapshot at its end), its
// change against the comparison coloured by the measure's direction, and a
// sparkline of the same query by time.
function KpiViz({
  result,
  spark,
  currency: fallback,
  registry,
}: {
  result: MetricsResult
  spark?: MetricsResult
  currency?: string
  registry: Registry
}) {
  const { text: adminText } = useAdminUI()
  const idx = indexColumns(result.columns)
  const primary = idx.measures[0]
  if (!primary) return <Empty label={adminText("no measure")} />
  const row = result.rows[0]
  const currency = row ? rowCurrency(row, idx, fallback) : fallback
  const value =
    !row && primary.unit === "money" && !currency
      ? "—"
      : formatMeasure(row ? row[primary.index] : 0, primary.unit, currency)
  const delta = row ? statDelta(result, row, primary.index) : null
  const line = spark ? pivotTimeSeries(spark, fallback) : undefined
  const series = line?.series.find(
    (s) =>
      s.measure === primary.name &&
      (!currency || !s.currency || s.currency === currency)
  )
  return (
    <div className="flex h-full flex-col justify-between gap-1">
      <div className="flex flex-wrap items-baseline gap-x-2 gap-y-0.5">
        <span className="text-2xl font-semibold tabular-nums @min-[12rem]/tile:text-3xl">
          {value}
        </span>
        {result.rows.length > 1 ? (
          <span className="text-xs text-muted-foreground">
            +{result.rows.length - 1}
            <AdminMessage id={" more"} />
          </span>
        ) : null}
      </div>
      {delta !== null ? (
        <Change
          delta={delta}
          unit={primary.unit}
          good={better(registry, primary.name, delta)}
          against={comparedWith(result)}
        />
      ) : null}
      {line && series && line.data.length > 1 ? (
        <Sparkline
          data={line.data}
          dataKey={series.key}
          thresholds={registry.thresholds(primary.name)}
        />
      ) : null}
    </div>
  )
}

// A sparkline draws its measure's thresholds (the networks' dispute levels),
// unlabelled.
function Sparkline({
  data,
  dataKey,
  thresholds = [],
}: {
  data: Record<string, number | string | null>[]
  dataKey: string
  thresholds?: MetricsThreshold[]
}) {
  const config: ChartConfig = { [dataKey]: { color: chartColor(0) } }
  return (
    <ChartContainer config={config} className="aspect-auto h-10 w-full">
      <LineChart data={data} margin={{ left: 0, right: 0, top: 2, bottom: 2 }}>
        <YAxis hide domain={[0, "auto"]} />
        {thresholds.map((t) => (
          <ReferenceLine
            key={t.value}
            y={t.value}
            stroke="var(--failed)"
            strokeOpacity={0.5}
            strokeDasharray="2 2"
            ifOverflow="extendDomain"
          />
        ))}
        <Line
          dataKey={dataKey}
          type="monotone"
          stroke={chartColor(0)}
          strokeWidth={1.5}
          dot={false}
          connectNulls
          isAnimationActive={animate()}
        />
      </LineChart>
    </ChartContainer>
  )
}

function Empty({ label }: { label: string }) {
  return (
    <div className="flex h-full min-h-16 items-center justify-center text-sm text-muted-foreground">
      {label}
    </div>
  )
}

function StatViz({
  result,
  currency: fallback,
  registry,
}: {
  result: MetricsResult
  currency?: string
  registry: Registry
}) {
  const { text: adminText } = useAdminUI()

  const idx = indexColumns(result.columns)
  const primary = idx.measures[0]
  if (!primary) return <Empty label={adminText("no measure")} />
  if (result.rows.length === 0) {
    // No rows and no currency filter: a money total has no currency to be
    // zero in, so it reads as no value rather than a sentence.
    return (
      <div className="text-2xl font-semibold tabular-nums @min-[12rem]/tile:text-3xl">
        {primary.unit === "money" && !fallback
          ? "—"
          : formatMeasure(0, primary.unit, fallback)}
      </div>
    )
  }
  // One rendered value per row (rows differ by dims — usually currency).
  const rows = result.rows.slice(0, 3)
  const against = comparedWith(result)
  return (
    <div className="flex h-full flex-col justify-center gap-1">
      {rows.map((row, i) => {
        const currency = rowCurrency(row, idx, fallback)
        const delta = statDelta(result, row, primary.index)
        const dimLabel =
          rows.length > 1
            ? idx.dims
                .map((d) => String(row[d.index] ?? ""))
                .filter(Boolean)
                .join(" ·")
            : ""
        return (
          <div
            key={i}
            className="flex flex-wrap items-baseline gap-x-2 gap-y-0.5"
          >
            <span
              className={cn(
                "font-semibold tabular-nums",
                // A narrow tile steps the figure down so a four-digit amount
                // still fits on one line.
                rows.length > 1
                  ? "text-lg"
                  : "text-2xl @min-[12rem]/tile:text-3xl"
              )}
            >
              {formatMeasure(row[primary.index], primary.unit, currency)}
            </span>
            {dimLabel ? (
              <span className="text-xs text-muted-foreground">{dimLabel}</span>
            ) : null}
            {delta !== null ? (
              <span className="basis-full">
                <Change
                  delta={delta}
                  unit={primary.unit}
                  good={better(registry, primary.name, delta)}
                  against={against}
                />
              </span>
            ) : null}
          </div>
        )
      })}
      {result.rows.length > 3 ? (
        <span className="text-xs text-muted-foreground">
          +{result.rows.length - 3}
          <AdminMessage id={" more"} />
        </span>
      ) : null}
    </div>
  )
}

// seriesLabel names a series for people: a bare measure key
// (new_subscriptions) reads "New subscriptions"; a dimension value (a rail, a
// currency) stays as it is.
function seriesLabel(series?: PivotSeries): string | undefined {
  if (!series) return undefined
  return series.label === series.measure
    ? humanizeColumn(series.label)
    : series.label
}

// MetricTooltip formats a hovered value from the exact wire cell the series
// row carries under exactKey (a money decimal string), falling back to the
// plotted Number only for series that never had one. Exported for tests.
export function MetricTooltip({
  series,
  ...props
}: ComponentProps<typeof ChartTooltipContent> & { series: PivotSeries[] }) {
  const measure = series[0]
  return (
    <ChartTooltipContent
      {...props}
      formatter={(value, name, item) => {
        // item.payload is the pivoted row, which carries each series' exact
        // wire cell beside the Number recharts plots.
        const row = (item as { payload?: unknown }).payload
        const exact =
          row && typeof row === "object"
            ? (row as Record<string, unknown>)[exactKey(String(name))]
            : undefined
        const cell =
          typeof exact === "string" || typeof exact === "number"
            ? exact
            : Number(value)
        return (
          <div className="flex w-full items-center justify-between gap-3">
            <span className="text-muted-foreground">
              {seriesLabel(series.find((item) => item.key === String(name))) ??
                name}
            </span>
            <span className="font-mono font-medium tabular-nums">
              {formatMeasure(cell, measure.unit, measure.currency)}
            </span>
          </div>
        )
      }}
    />
  )
}

// rateAxisTop is a rate axis's top: a quarter above the highest value, up to
// the next whole step of its leading digit (a 52% peak tops at 70%), never
// past 100%.
function rateAxisTop(max: number): number {
  const raw = max * 1.25
  const step = 10 ** Math.floor(Math.log10(raw))
  return Math.min(1, Number((Math.ceil(raw / step) * step).toPrecision(12)))
}

function TimeSeriesViz({
  viz,
  result,
  currency,
  registry,
}: {
  viz: "line" | "area" | "bar"
  result: MetricsResult
  currency?: string
  registry: Registry
}) {
  const { text: adminText } = useAdminUI()

  const { data, series } = pivotTimeSeries(result, currency)
  if (data.length === 0 || series.length === 0)
    return <Empty label={adminText("no data in range")} />
  const groups = groupSeries(series)
  if (groups.length === 1)
    return (
      <TimeSeriesChart
        viz={viz}
        data={data}
        series={series}
        registry={registry}
      />
    )
  return (
    <div className="flex h-full flex-col gap-4 overflow-auto">
      {groups.map((group) => (
        <section key={group.key} className="min-h-44 shrink-0">
          <div className="mb-1 text-xs font-medium text-muted-foreground">
            {group.label}
          </div>
          <div className="h-36">
            <TimeSeriesChart
              viz={viz}
              data={data}
              series={group.series}
              registry={registry}
            />
          </div>
        </section>
      ))}
    </div>
  )
}

// Recharts animates in script, out of reach of the reduced-motion CSS.
const animate = () =>
  !window.matchMedia?.("(prefers-reduced-motion: reduce)").matches

function TimeSeriesChart({
  viz,
  data,
  series,
  registry,
}: Pivoted & { viz: "line" | "area" | "bar"; registry: Registry }) {
  const config: ChartConfig = {}
  series.forEach((s, i) => {
    config[s.key] = { label: seriesLabel(s), color: chartColor(i) }
  })
  const unit = series[0]?.unit
  const tickFormatter = (v: number) =>
    formatMeasure(v, unit, series[0]?.currency)
  const chartProps = { data, margin: { left: 4, right: 4, top: 4, bottom: 0 } }
  const axes = (
    <>
      <CartesianGrid vertical={false} strokeOpacity={0.4} />
      <XAxis
        dataKey="bucket"
        tickLine={false}
        axisLine={false}
        tickMargin={6}
        minTickGap={24}
      />
      <YAxis
        tickLine={false}
        axisLine={false}
        width={unit === "money" ? 80 : 52}
        tickFormatter={tickFormatter}
        // Counts are whole numbers; a rate never passes 100%, and an all-zero
        // rate would otherwise get Recharts' default 0–4 (400%) scale.
        allowDecimals={unit === "money" || unit === "ratio" || unit === "days"}
        domain={
          unit === "ratio"
            ? [0, (max: number) => (max > 0 ? rateAxisTop(max) : 1)]
            : undefined
        }
      />
    </>
  )
  const tooltip = <ChartTooltip content={<MetricTooltip series={series} />} />
  // A measure's thresholds (a network's dispute ratio limit) cross its line.
  const thresholds = [...new Set(series.map((s) => s.measure))].flatMap(
    (measure) =>
      registry
        .thresholds(measure)
        .map((t) => (
          <ReferenceLine
            key={`${measure}:${t.value}`}
            y={t.value}
            stroke="var(--failed)"
            strokeDasharray="4 4"
            ifOverflow="extendDomain"
            label={{ value: t.label, position: "insideTopRight", fontSize: 10 }}
          />
        ))
  )
  const legend =
    series.length > 1 ? (
      <ChartLegend
        content={<ChartLegendContent className="flex-wrap gap-x-4 gap-y-1" />}
      />
    ) : null
  return (
    <ChartContainer config={config} className="h-full min-h-24 w-full">
      {viz === "line" ? (
        <LineChart {...chartProps}>
          {axes}
          {tooltip}
          {legend}
          {thresholds}
          {series.map((s, i) => (
            <Line
              key={s.key}
              dataKey={s.key}
              type="monotone"
              stroke={chartColor(i)}
              strokeWidth={2}
              // A rate's empty buckets are gaps; dots keep a lone value visible.
              connectNulls={s.unit === "ratio"}
              isAnimationActive={animate()}
              dot={s.unit === "ratio" ? { r: 2.5 } : false}
            />
          ))}
        </LineChart>
      ) : viz === "area" ? (
        // Signed values stack apart: refunds below the axis, sales above.
        <AreaChart {...chartProps} stackOffset="sign">
          {axes}
          {tooltip}
          {legend}
          {series.map((s, i) => (
            <Area
              key={s.key}
              dataKey={s.key}
              type="monotone"
              stackId={s.measure}
              stroke={chartColor(i)}
              fill={chartColor(i)}
              fillOpacity={0.35}
              isAnimationActive={animate()}
            />
          ))}
        </AreaChart>
      ) : (
        <BarChart {...chartProps} stackOffset="sign">
          {axes}
          {tooltip}
          {legend}
          {series.map((s, i) => (
            <Bar
              key={s.key}
              dataKey={s.key}
              stackId={s.measure}
              fill={chartColor(i)}
              radius={2}
              isAnimationActive={animate()}
            />
          ))}
        </BarChart>
      )}
    </ChartContainer>
  )
}

function DonutViz({
  result,
  currency,
}: {
  result: MetricsResult
  currency?: string
}) {
  const { text: adminText } = useAdminUI()

  const idx = indexColumns(result.columns)
  const primary = idx.measures[0]
  const pspKey = usePSPKeys(idx.dims.some((d) => d.name === "psp"))
  if (!primary) return <Empty label={adminText("no measure")} />
  const slices = donutSlices(result, currency, pspKey)
  if (slices.length === 0)
    return <Empty label={adminText("no data in range")} />
  const groups = groupSeries(slices)
  const charts = groups.map((group) => {
    const config: ChartConfig = {}
    group.series.forEach((slice, i) => {
      config[slice.key] = { label: slice.label, color: chartColor(i) }
    })
    return (
      <section
        key={group.key}
        className={groups.length > 1 ? "min-h-44 shrink-0" : "h-full"}
      >
        {groups.length > 1 && (
          <div className="mb-1 text-xs font-medium text-muted-foreground">
            {group.label}
          </div>
        )}
        <ChartContainer
          config={config}
          className={
            groups.length > 1 ? "h-36 w-full" : "h-full min-h-24 w-full"
          }
        >
          <PieChart>
            <ChartTooltip
              content={<MetricTooltip series={group.series} nameKey="key" />}
            />
            <ChartLegend
              content={
                <ChartLegendContent
                  nameKey="key"
                  className="flex-wrap gap-x-4 gap-y-1"
                />
              }
            />
            <Pie
              data={group.series}
              dataKey="value"
              nameKey="key"
              innerRadius="55%"
              isAnimationActive={animate()}
              stroke="var(--card)"
              strokeWidth={2}
            >
              {group.series.map((slice, i) => (
                <Cell key={slice.key} fill={chartColor(i)} />
              ))}
            </Pie>
          </PieChart>
        </ChartContainer>
      </section>
    )
  })
  return (
    <div className="flex h-full flex-col gap-4 overflow-auto">{charts}</div>
  )
}

// FunnelViz draws a count by ordered steps: each bar is the rows that reached
// its step or went further, with its share of the first step and of the one
// before.
function FunnelViz({
  result,
  query,
  registry,
}: {
  result: MetricsResult
  query?: QueryMetricsParams
  registry: Registry
}) {
  const { text: adminText } = useAdminUI()
  const dim = indexColumns(result.columns).dims.find(
    (d) => d.name !== "currency"
  )
  const order = dim ? (registry.values(query?.measures ?? [], dim.name) ?? []) : []
  const steps = funnelSteps(result, order)
  if (steps.length === 0 || steps[0].value === 0)
    return <Empty label={adminText("no data in range")} />
  return (
    <ol className="flex h-full flex-col justify-center gap-2 overflow-auto">
      {steps.map((step) => (
        <li
          key={step.key}
          className="grid grid-cols-[6rem_1fr_7rem] items-center gap-2 text-xs"
        >
          <span className="truncate">{humanizeColumn(step.key)}</span>
          <span className="h-5 w-full">
            <span
              className="block h-full rounded-sm"
              style={{
                width: `${Math.max(1, (step.ofFirst ?? 0) * 100)}%`,
                background: chartColor(0),
              }}
            />
          </span>
          <span className="text-right tabular-nums text-muted-foreground">
            {step.value.toLocaleString()}
            {step.fromPrevious === null
              ? ""
              : ` · ${(step.fromPrevious * 100).toFixed(1)}%`}
          </span>
        </li>
      ))}
    </ol>
  )
}

// humanizeColumn turns a result column name (first_failure_reason) into a
// label (First failure reason).
function humanizeColumn(name: string): string {
  if (name === "psp") return "PSP"
  const words = name.replaceAll("_", " ").trim()
  return words.charAt(0).toUpperCase() + words.slice(1)
}

function TableViz({
  result,
  currency: fallback,
}: {
  result: MetricsResult
  currency?: string
}) {
  const { text: adminText } = useAdminUI()
  // A PSP reads as its key, for those who may read the PSPs.
  const pspKey = usePSPKeys(result.columns.some((c) => c.name === "psp"))

  const idx = indexColumns(result.columns)
  if (result.rows.length === 0)
    return <Empty label={adminText("no data in range")} />
  return (
    <div className="h-full overflow-auto">
      <Table>
        <TableHeader>
          <TableRow className="hover:bg-transparent">
            {result.columns.map((c) => (
              <TableHead
                key={c.name}
                className={cn(
                  "text-xs whitespace-nowrap",
                  c.kind === "measure" && "text-right"
                )}
              >
                {humanizeColumn(c.name)}
              </TableHead>
            ))}
          </TableRow>
        </TableHeader>
        <TableBody>
          {result.rows.map((row, ri) => (
            <TableRow key={ri}>
              {result.columns.map((c, ci) => {
                const cell = row[ci]
                let rendered: string
                if (c.kind === "time")
                  rendered = formatBucket(
                    String(cell),
                    result.grain,
                    result.timezone
                  )
                else if (c.kind === "measure") {
                  const currency = rowCurrency(row, idx, fallback)
                  rendered = formatMeasure(cell, c.unit, currency)
                } else if (c.name === "psp" && cell)
                  rendered = pspKey(String(cell))
                else
                  rendered = cell === null || cell === "" ? "—" : String(cell)
                return (
                  <TableCell
                    key={c.name}
                    className={cn(
                      "text-xs",
                      c.kind === "measure" && "text-right tabular-nums"
                    )}
                  >
                    {rendered}
                  </TableCell>
                )
              })}
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </div>
  )
}
