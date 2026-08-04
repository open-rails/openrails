// Viz renderers: stat tile (section-cards pattern w/ compare delta),
// line/area/bar (shadcn chart on recharts 3, var(--chart-N) tokens), donut,
// table. Input is the raw #733 tabular result — no client-side re-querying.
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
import type { MetricsResult, WidgetViz as VizType } from "@/lib/api/metrics"
import { cn } from "@/lib/utils"

import {
  chartColor,
  formatBucket,
  formatMeasure,
  indexColumns,
  pivotTimeSeries,
} from "./lib"

export function WidgetVizView({
  viz,
  result,
}: {
  viz: VizType
  result: MetricsResult
}) {
  switch (viz) {
    case "stat":
      return <StatViz result={result} />
    case "line":
    case "area":
    case "bar":
      return <TimeSeriesViz viz={viz} result={result} />
    case "donut":
      return <DonutViz result={result} />
    case "table":
      return <TableViz result={result} />
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

function sumMeasure(rows: MetricsResult["rows"], index: number): number {
  return rows.reduce((acc, row) => acc + Number(row[index] ?? 0), 0)
}

function StatViz({ result }: { result: MetricsResult }) {
  const idx = indexColumns(result.columns)
  const primary = idx.measures[0]
  if (!primary) return <Empty label="no measure" />
  if (result.rows.length === 0) {
    return (
      <div className="text-2xl font-semibold tabular-nums">
        {formatMeasure(0, primary.unit)}
      </div>
    )
  }
  // One rendered value per row (rows differ by dims — usually currency).
  const rows = result.rows.slice(0, 3)
  const delta = statDelta(result, primary.index)
  return (
    <div className="flex h-full flex-col justify-center gap-1">
      {rows.map((row, i) => {
        const currency =
          idx.currency >= 0 ? String(row[idx.currency] ?? "") : undefined
        const dimLabel =
          rows.length > 1
            ? idx.dims
                .map((d) => String(row[d.index] ?? ""))
                .filter(Boolean)
                .join(" · ")
            : ""
        return (
          <div key={i} className="flex items-baseline gap-2">
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
          </div>
        )
      })}
      {result.rows.length > 3 ? (
        <span className="text-xs text-muted-foreground">
          +{result.rows.length - 3} more
        </span>
      ) : null}
      {delta !== null ? (
        <span
          className={cn(
            "flex items-center gap-1 text-xs font-medium",
            delta >= 0
              ? "text-emerald-600 dark:text-emerald-400"
              : "text-red-600 dark:text-red-400"
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
}

function statDelta(result: MetricsResult, measureIndex: number): number | null {
  if (!result.compare_rows) return null
  const prev = sumMeasure(result.compare_rows, measureIndex)
  const cur = sumMeasure(result.rows, measureIndex)
  if (prev === 0) return null
  return (cur - prev) / Math.abs(prev)
}

// --- time series -----------------------------------------------------------------

function TimeSeriesViz({
  viz,
  result,
}: {
  viz: "line" | "area" | "bar"
  result: MetricsResult
}) {
  const { data, series } = pivotTimeSeries(result)
  if (data.length === 0 || series.length === 0)
    return <Empty label="no data in range" />
  const config: ChartConfig = {}
  series.forEach((s, i) => {
    config[s.key] = { label: s.key, color: chartColor(i) }
  })
  const unit = series[0]?.unit
  const tickFormatter = (v: number) =>
    formatMeasure(v, unit).replace(/\.\d+/, "")
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
        width={52}
        tickFormatter={tickFormatter}
      />
    </>
  )
  const tooltip = <ChartTooltip content={<ChartTooltipContent />} />
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
              stackId="a"
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
              stackId="a"
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

function DonutViz({ result }: { result: MetricsResult }) {
  const idx = indexColumns(result.columns)
  const primary = idx.measures[0]
  if (!primary) return <Empty label="no measure" />
  const slices = result.rows
    .map((row) => ({
      name:
        idx.dims
          .map((d) => String(row[d.index] ?? ""))
          .filter(Boolean)
          .join(" · ") || primary.name,
      value: Number(row[primary.index] ?? 0),
    }))
    .filter((s) => s.value !== 0)
  if (slices.length === 0) return <Empty label="no data in range" />
  const config: ChartConfig = {}
  slices.forEach((s, i) => {
    config[s.name] = { label: s.name, color: chartColor(i) }
  })
  return (
    <ChartContainer config={config} className="h-full min-h-24 w-full">
      <PieChart>
        <ChartTooltip content={<ChartTooltipContent nameKey="name" />} />
        <Pie
          data={slices}
          dataKey="value"
          nameKey="name"
          innerRadius="55%"
          stroke="var(--card)"
          strokeWidth={2}
        >
          {slices.map((s, i) => (
            <Cell key={s.name} fill={chartColor(i)} />
          ))}
        </Pie>
      </PieChart>
    </ChartContainer>
  )
}

// --- table --------------------------------------------------------------------------

// humanizeColumn turns raw result column names (rail_account) into labels
// (Rail account).
function humanizeColumn(name: string): string {
  const words = name.replaceAll("_", " ").trim()
  return words.charAt(0).toUpperCase() + words.slice(1)
}

function TableViz({ result }: { result: MetricsResult }) {
  const idx = indexColumns(result.columns)
  if (result.rows.length === 0) return <Empty label="no data in range" />
  return (
    <div className="h-full overflow-auto">
      <Table>
        <TableHeader>
          <TableRow>
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
                  const currency =
                    idx.currency >= 0
                      ? String(row[idx.currency] ?? "")
                      : undefined
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
