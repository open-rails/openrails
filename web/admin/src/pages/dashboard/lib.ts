// Result-shaping helpers for dashboard widgets: unit-aware formatting,
// time-series pivoting, and count-widget deep links (#733 contract: every
// count tile links to the matching admin list).
import type {
  MetricsCell,
  MetricsColumn,
  MetricsQuery,
  MetricsResult,
} from "@/lib/api/metrics"
import { formatMicros } from "@/lib/format"

export function newWidgetId(): string {
  return crypto.randomUUID()
}

// formatMeasure renders one measure cell by its column unit.
export function formatMeasure(
  value: MetricsCell,
  unit: string | undefined,
  currency?: string
): string {
  if (value === null || value === undefined) return "—"
  const n = typeof value === "number" ? value : Number(value)
  if (!Number.isFinite(n)) return String(value)
  switch (unit) {
    case "micros":
      return formatMicros(n, currency ?? "").trim()
    case "ratio":
      return `${(n * 100).toLocaleString(undefined, { maximumFractionDigits: 2 })}%`
    case "days":
      return `${n.toLocaleString(undefined, { maximumFractionDigits: 1 })} d`
    default:
      return n.toLocaleString(undefined, { maximumFractionDigits: 2 })
  }
}

// formatBucket renders a time-bucket label for the query grain.
export function formatBucket(iso: string, grain?: string): string {
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  switch (grain) {
    case "month":
    case "quarter":
      return d.toLocaleDateString(undefined, {
        month: "short",
        year: "numeric",
        timeZone: "UTC",
      })
    case "year":
      return d.toLocaleDateString(undefined, {
        year: "numeric",
        timeZone: "UTC",
      })
    default:
      return d.toLocaleDateString(undefined, {
        month: "short",
        day: "numeric",
        timeZone: "UTC",
      })
  }
}

export interface ColumnIndex {
  time: number
  dims: { name: string; index: number }[]
  measures: { name: string; unit?: string; index: number }[]
  currency: number
}

export function indexColumns(columns: MetricsColumn[]): ColumnIndex {
  const idx: ColumnIndex = { time: -1, dims: [], measures: [], currency: -1 }
  columns.forEach((c, i) => {
    if (c.kind === "time") idx.time = i
    else if (c.kind === "dimension") {
      idx.dims.push({ name: c.name, index: i })
      if (c.name === "currency") idx.currency = i
    } else idx.measures.push({ name: c.name, unit: c.unit, index: i })
  })
  return idx
}

export interface PivotSeries {
  key: string
  label: string
  measure: string
  dimensions: MetricsCell[]
  unit?: string
  currency?: string
}

export function filteredCurrency(query?: MetricsQuery): string | undefined {
  const currencies = query?.filters?.currency
  return currencies?.length === 1 ? currencies[0].trim() : undefined
}

export function rowCurrency(
  row: MetricsCell[],
  idx: ColumnIndex,
  fallback?: string
): string | undefined {
  return idx.currency >= 0
    ? String(row[idx.currency] ?? "") || undefined
    : fallback
}

// Absolute bucket dates differ between periods; stat deltas are defined only
// for a single value per dimension tuple. Never sum ratios or unlike monies.
export function statDelta(
  result: MetricsResult,
  row: MetricsCell[],
  measureIndex: number
): number | null {
  if (!result.compare_rows || row[measureIndex] === null) return null
  const idx = indexColumns(result.columns)
  const key = (r: MetricsCell[]) =>
    JSON.stringify(idx.dims.map((d) => r[d.index]))
  const currentKey = key(row)
  if (result.rows.filter((r) => key(r) === currentKey).length !== 1) return null
  const previous = result.compare_rows.filter((r) => key(r) === currentKey)
  if (previous.length !== 1 || previous[0][measureIndex] === null) return null
  const prev = Number(previous[0][measureIndex])
  const cur = Number(row[measureIndex])
  return Number.isFinite(prev) && Number.isFinite(cur) && prev !== 0
    ? (cur - prev) / Math.abs(prev)
    : null
}

// One axis (or donut total) may only combine the same measurement unit and
// currency. Counts do not become money just because a currency dimension exists.
export function groupSeries<T extends PivotSeries>(
  series: T[]
): { key: string; label: string; series: T[] }[] {
  const groups = new Map<string, { key: string; label: string; series: T[] }>()
  for (const item of series) {
    const currency = item.unit === "micros" ? item.currency : undefined
    const key = JSON.stringify([item.unit ?? "number", currency ?? null])
    let group = groups.get(key)
    if (!group) {
      group = {
        key,
        label:
          item.unit === "micros"
            ? currency || "Amount (currency not supplied)"
            : item.unit || "Values",
        series: [],
      }
      groups.set(key, group)
    }
    group.series.push(item)
  }
  return [...groups.values()]
}

export interface Pivoted {
  data: Record<string, number | string>[]
  series: PivotSeries[]
}

// pivotTimeSeries turns tabular rows into recharts rows keyed by bucket, one
// series per measure × dimension-value combination. Server-zero-filled
// buckets render as-is; combos absent for a bucket fill 0 so stacks align.
export function pivotTimeSeries(
  result: MetricsResult,
  currency?: string
): Pivoted {
  const idx = indexColumns(result.columns)
  if (idx.time < 0) return { data: [], series: [] }
  const buckets = new Map<string, Record<string, number | string>>()
  const series = new Map<string, PivotSeries>()
  for (const row of result.rows) {
    const t = String(row[idx.time])
    let entry = buckets.get(t)
    if (!entry) {
      entry = { bucket: formatBucket(t, result.grain), bucketISO: t }
      buckets.set(t, entry)
    }
    const dimensions = idx.dims.map((d) => row[d.index])
    const dimLabel = dimensions
      .map((value) => String(value ?? ""))
      .filter(Boolean)
      .join(" · ")
    for (const m of idx.measures) {
      const identity = JSON.stringify([m.name, ...dimensions])
      let item = series.get(identity)
      if (!item) {
        item = {
          key: `series-${series.size}`,
          label:
            dimLabel && idx.measures.length > 1
              ? `${m.name} · ${dimLabel}`
              : dimLabel || m.name,
          measure: m.name,
          dimensions,
          unit: m.unit,
          currency:
            m.unit === "micros" ? rowCurrency(row, idx, currency) : undefined,
        }
        series.set(identity, item)
      }
      entry[item.key] = Number(row[m.index] ?? 0)
    }
  }
  const data = [...buckets.entries()]
    .sort(([a], [b]) => (a < b ? -1 : 1))
    .map(([, entry]) => entry)
  const keys = [...series.values()]
  for (const entry of data) {
    for (const s of keys) if (!(s.key in entry)) entry[s.key] = 0
  }
  return { data, series: keys }
}

// chartColor cycles the shadcn --chart-N tokens.
export function chartColor(i: number): string {
  return `var(--chart-${(i % 5) + 1})`
}

// deepLinkFor maps count widgets to the admin list page carrying the same
// filter (#733 count→list contract). Null = no sensible link.
export function deepLinkFor(query: MetricsQuery): string | null {
  const measures = query.measures ?? []
  const status = query.filters?.status?.[0]
  const has = (...names: string[]) => names.some((n) => measures.includes(n))
  if (has("subscriptions", "billable_subscriptions")) {
    return status
      ? `/subscriptions?status=${encodeURIComponent(status)}`
      : "/subscriptions"
  }
  if (has("new_subscriptions")) return "/subscriptions?status=active"
  if (has("cancellations")) return "/subscriptions?status=cancelled"
  if (has("payment_failures", "unique_failed_customers")) return "/payments"
  if (
    has(
      "payment_count",
      "refund_count",
      "chargeback_count",
      "unique_rebilled_customers"
    )
  )
    return "/payments"
  if (has("entitled_customers")) return "/customers"
  return null
}
