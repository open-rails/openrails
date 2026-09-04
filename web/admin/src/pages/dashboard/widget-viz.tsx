// Viz renderers: stat tile (section-cards pattern w/ compare delta),
// line/area/bar (shadcn chart on recharts 3, var(--chart-N) tokens), donut,
// table. Input is the raw #733 tabular result — no client-side re-querying.
import type { ComponentProps } from "react"
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
} from "@/components/ui/table"
import {
  ChartContainer,
  ChartTooltip,
  ChartTooltipContent,
  type ChartConfig,
} from "@/components/ui/chart"
import type {
  MetricsResult,
  MetricsQuery,
  WidgetViz as VizType,
} from "@/lib/api/metrics"
import { cn } from "@/lib/utils"

import {
  chartColor,
  filteredCurrency,
  groupSeries,
  rowCurrency,
  statDelta,
  type PivotSeries,
  type Pivoted,
  formatBucket,
  formatMeasure,
  indexColumns,
  pivotTimeSeries,
} from "./lib"

export function WidgetVizView({
  viz,
  result,
  query,
}: {
  viz: VizType
  result: MetricsResult
  query?: MetricsQuery
}) {
  const currency = filteredCurrency(query)
  switch (viz) {
    case "stat":
      return <StatViz result={result} currency={currency} />
    case "line":
    case "area":
    case "bar":
      return <TimeSeriesViz viz={viz} result={result} currency={currency} />
    case "donut":
      return <DonutViz result={result} currency={currency} />
    case "table":
      return <TableViz result={result} currency={currency} />
  }
}

function Empty({ label }: { label: string }) {
  return (
    <div className="flex h-full min-h-16 items-center justify-center text-sm text-muted-foreground">
      {label}
    </div>
  )
}

// --- stat ----------------------------------------------------------------------

function StatViz({
  result,
  currency: fallback,
}: {
  result: MetricsResult
  currency?: string
}) {
  const idx = indexColumns(result.columns)
  const primary = idx.measures[0]
  if (!primary) return <Empty label="no measure" />
  if (result.rows.length === 0) {
    return (
      <div className="text-2xl font-semibold tabular-nums">
        {formatMeasure(0, primary.unit, fallback)}
      </div>
    )
  }
  // One rendered value per row (rows differ by dims — usually currency).
  const rows = result.rows.slice(0, 3)
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
                rows.length > 1 ? "text-lg" : "text-3xl"
              )}
            >
              {formatMeasure(row[primary.index], primary.unit, currency)}
            </span>
            {dimLabel ? (
              <span className="text-xs text-muted-foreground">{dimLabel}</span>
            ) : null}
            {delta !== null ? (
              <span
                className={cn(
                  "flex basis-full items-center gap-1 text-xs font-medium",
                  delta >= 0 ? "text-settled" : "text-failed"
                )}
              >
                {delta >= 0 ? (
                  <HugeiconsIcon icon={AnalyticsUpIcon} className="size-3" />
                ) : (
                  <HugeiconsIcon icon={AnalyticsDownIcon} className="size-3" />
                )}
                {`${delta >= 0 ? "+" : ""}${(delta * 100).toFixed(1)}% vs previous period`}
              </span>
            ) : null}
          </div>
        )
      })}
      {result.rows.length > 3 ? (
        <span className="text-xs text-muted-foreground">
          +{result.rows.length - 3} more
        </span>
      ) : null}
    </div>
  )
}

function MetricTooltip({
  series,
  ...props
}: ComponentProps<typeof ChartTooltipContent> & { series: PivotSeries[] }) {
  const measure = series[0]
  return (
    <ChartTooltipContent
      {...props}
      formatter={(value, name) => (
        <div className="flex w-full items-center justify-between gap-3">
          <span className="text-muted-foreground">
            {series.find((item) => item.key === String(name))?.label ?? name}
          </span>
          <span className="font-mono font-medium tabular-nums">
            {formatMeasure(Number(value), measure.unit, measure.currency)}
          </span>
        </div>
      )}
    />
  )
}

// --- time series -----------------------------------------------------------------

function TimeSeriesViz({
  viz,
  result,
  currency,
}: {
  viz: "line" | "area" | "bar"
  result: MetricsResult
  currency?: string
}) {
  const { data, series } = pivotTimeSeries(result, currency)
  if (data.length === 0 || series.length === 0)
    return <Empty label="no data in range" />
  const groups = groupSeries(series)
  if (groups.length === 1)
    return <TimeSeriesChart viz={viz} data={data} series={series} />
  return (
    <div className="flex h-full flex-col gap-4 overflow-auto">
      {groups.map((group) => (
        <section key={group.key} className="min-h-44 shrink-0">
          <div className="mb-1 text-xs font-medium text-muted-foreground">
            {group.label}
          </div>
          <div className="h-36">
            <TimeSeriesChart viz={viz} data={data} series={group.series} />
          </div>
        </section>
      ))}
    </div>
  )
}

function TimeSeriesChart({
  viz,
  data,
  series,
}: Pivoted & { viz: "line" | "area" | "bar" }) {
  const config: ChartConfig = {}
  series.forEach((s, i) => {
    config[s.key] = { label: s.label, color: chartColor(i) }
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
        width={unit === "micros" ? 80 : 52}
        tickFormatter={tickFormatter}
      />
    </>
  )
  const tooltip = <ChartTooltip content={<MetricTooltip series={series} />} />
  return (
    <ChartContainer config={config} className="h-full min-h-24 w-full">
      {viz === "line" ? (
        <LineChart {...chartProps}>
          {axes}
          {tooltip}
          {series.map((s, i) => (
            <Line
              key={s.key}
              dataKey={s.key}
              type="monotone"
              stroke={chartColor(i)}
              strokeWidth={2}
              dot={false}
            />
          ))}
        </LineChart>
      ) : viz === "area" ? (
        <AreaChart {...chartProps}>
          {axes}
          {tooltip}
          {series.map((s, i) => (
            <Area
              key={s.key}
              dataKey={s.key}
              type="monotone"
              stackId={s.measure}
              stroke={chartColor(i)}
              fill={chartColor(i)}
              fillOpacity={0.35}
            />
          ))}
        </AreaChart>
      ) : (
        <BarChart {...chartProps}>
          {axes}
          {tooltip}
          {series.map((s, i) => (
            <Bar
              key={s.key}
              dataKey={s.key}
              stackId={s.measure}
              fill={chartColor(i)}
              radius={2}
            />
          ))}
        </BarChart>
      )}
    </ChartContainer>
  )
}

// --- donut -------------------------------------------------------------------------

function DonutViz({
  result,
  currency,
}: {
  result: MetricsResult
  currency?: string
}) {
  const idx = indexColumns(result.columns)
  const primary = idx.measures[0]
  if (!primary) return <Empty label="no measure" />
  const slices = result.rows
    .map((row, i) => ({
      key: `slice-${i}`,
      label:
        idx.dims
          .map((d) => String(row[d.index] ?? ""))
          .filter(Boolean)
          .join(" · ") || primary.name,
      measure: primary.name,
      dimensions: idx.dims.map((d) => row[d.index]),
      unit: primary.unit,
      currency:
        primary.unit === "micros" ? rowCurrency(row, idx, currency) : undefined,
      value: Number(row[primary.index] ?? 0),
    }))
    .filter((slice) => slice.value !== 0)
  if (slices.length === 0) return <Empty label="no data in range" />
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
            <Pie
              data={group.series}
              dataKey="value"
              nameKey="key"
              innerRadius="55%"
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

// --- table --------------------------------------------------------------------------

// humanizeColumn turns raw result column names (rail_account) into labels
// (Rail account).
function humanizeColumn(name: string): string {
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
  const idx = indexColumns(result.columns)
  if (result.rows.length === 0) return <Empty label="no data in range" />
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
                  rendered = formatBucket(String(cell), result.grain)
                else if (c.kind === "measure") {
                  const currency = rowCurrency(row, idx, fallback)
                  rendered = formatMeasure(cell, c.unit, currency)
                } else
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
