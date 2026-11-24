// Result-shaping helpers for dashboard widgets: unit-aware formatting,
// time-series pivoting, and deep links (every count tile links to its list).
import type {
  MetricsCell,
  MetricsColumn,
  QueryMetricsParams,
  MetricsRange,
  MetricsResult,
} from "#orck/admin/lib/api/metrics"
import { formatSummaryAmount, type MerchantUnits } from "#orck/admin/lib/format"

// formatMeasure renders one measure cell by its column unit.
export function formatMeasure(
  value: MetricsCell,
  unit: string | undefined,
  currency?: string,
  units?: MerchantUnits
): string {
  if (value === null || value === undefined) return "—"
  // "money" cells are exact decimal strings of native units at the
  // currency's scale: the registry's, or a merchant unit's (units); a tile
  // or chart shows a fiat amount to the cent.
  if (unit === "money") return formatSummaryAmount(value, currency ?? "", units)
  const n = typeof value === "number" ? value : Number(value)
  if (!Number.isFinite(n)) return String(value)
  switch (unit) {
    case "ratio":
      return `${(n * 100).toLocaleString(undefined, { maximumFractionDigits: 2 })}%`
    case "days":
      return `${n.toLocaleString(undefined, { maximumFractionDigits: 1 })} d`
    case "seconds": {
      // An age reads in the largest unit it fills.
      const [size, suffix] =
        Math.abs(n) >= 172_800
          ? [86_400, "d"]
          : Math.abs(n) >= 7_200
            ? [3_600, "h"]
            : Math.abs(n) >= 120
              ? [60, "min"]
              : [1, "s"]
      return `${(n / size).toLocaleString(undefined, { maximumFractionDigits: 1 })} ${suffix}`
    }
    default:
      return n.toLocaleString(undefined, { maximumFractionDigits: 2 })
  }
}

// formatBucket renders a time-bucket label for the query grain, on the
// calendar of the result's timezone.
export function formatBucket(
  iso: string,
  grain?: string,
  timeZone = "UTC"
): string {
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  switch (grain) {
    case "month":
    case "quarter":
      return d.toLocaleDateString(undefined, {
        month: "short",
        year: "numeric",
        timeZone,
      })
    case "year":
      return d.toLocaleDateString(undefined, {
        year: "numeric",
        timeZone,
      })
    default:
      return d.toLocaleDateString(undefined, {
        month: "short",
        day: "numeric",
        timeZone,
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

export function filteredCurrency(
  query?: QueryMetricsParams
): string | undefined {
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
// A ratio's change is its difference in points; anything else's is relative.
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
  if (!Number.isFinite(prev) || !Number.isFinite(cur)) return null
  if (idx.measures.find((m) => m.index === measureIndex)?.unit === "ratio")
    return cur - prev
  return prev !== 0 ? (cur - prev) / Math.abs(prev) : null
}

// changeText says a change: points for a ratio, a percentage otherwise.
export function changeText(delta: number, unit?: string): string {
  const sign = delta > 0 ? "+" : ""
  return unit === "ratio"
    ? `${sign}${(delta * 100).toFixed(1)} pp`
    : `${sign}${(delta * 100).toFixed(1)}%`
}

// comparedWith names the period a stat's delta compares with: a one-day
// range compares with yesterday, a window a year before with last year.
export function comparedWith(result: MetricsResult): string {
  if (!result.compare_range) return "previous period"
  const span =
    Date.parse(result.compare_range.to) - Date.parse(result.compare_range.from)
  if (result.compare_range.to !== result.range.from) return "last year"
  return span === 86_400_000 ? "yesterday" : "previous period"
}

// One axis (or donut total) may only combine the same measurement unit and
// currency. Counts do not become money just because a currency dimension exists.
export function groupSeries<T extends PivotSeries>(
  series: T[]
): { key: string; label: string; series: T[] }[] {
  const groups = new Map<string, { key: string; label: string; series: T[] }>()
  for (const item of series) {
    const currency = item.unit === "money" ? item.currency : undefined
    const key = JSON.stringify([item.unit ?? "number", currency ?? null])
    let group = groups.get(key)
    if (!group) {
      group = {
        key,
        label:
          item.unit === "money"
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
  data: Record<string, number | string | null>[]
  series: PivotSeries[]
}

// exactKey names the pivoted row field that keeps a series' exact wire cell
// (a money decimal string) beside the Number recharts plots, so tooltips
// format the exact value rather than a Number-coerced one.
export function exactKey(seriesKey: string): string {
  return `${seriesKey}:exact`
}

// IDENTIFIER_DIMS hold identifiers, shown as they are; any other dimension's
// values are words (needs_payment_method reads "needs payment method").
const IDENTIFIER_DIMS = new Set([
  "psp",
  "product_id",
  "price_id",
  "customer",
  "card_bin",
  "currency",
  "cohort",
  "discount_code",
])

export function dimValueLabel(dim: string, value: MetricsCell): string {
  const text = String(value ?? "")
  return IDENTIFIER_DIMS.has(dim) ? text : text.replaceAll("_", " ")
}

// pivotTimeSeries turns tabular rows into recharts rows keyed by bucket, one
// series per measure × dimension-value combination. Server-zero-filled
// buckets render as-is; combos absent for a bucket fill 0 so stacks align.
// A ratio with nothing to divide by is no data, not 0%: it stays null.
export function pivotTimeSeries(
  result: MetricsResult,
  currency?: string
): Pivoted {
  const idx = indexColumns(result.columns)
  if (idx.time < 0) return { data: [], series: [] }
  const buckets = new Map<string, Record<string, number | string | null>>()
  const series = new Map<string, PivotSeries>()
  for (const row of result.rows) {
    const t = String(row[idx.time])
    let entry = buckets.get(t)
    if (!entry) {
      entry = {
        bucket: formatBucket(t, result.grain, result.timezone),
        bucketISO: t,
      }
      buckets.set(t, entry)
    }
    const dimensions = idx.dims.map((d) => row[d.index])
    const dimLabel = idx.dims
      .map((d) => dimValueLabel(d.name, row[d.index]))
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
            m.unit === "money" ? rowCurrency(row, idx, currency) : undefined,
        }
        series.set(identity, item)
      }
      const cell = row[m.index]
      const empty = m.unit === "ratio" ? null : 0
      entry[item.key] = cell === null ? empty : Number(cell)
      entry[exactKey(item.key)] = cell ?? empty
    }
  }
  const data = [...buckets.entries()]
    .sort(([a], [b]) => (a < b ? -1 : 1))
    .map(([, entry]) => entry)
  const keys = [...series.values()]
  for (const entry of data) {
    for (const s of keys)
      if (!(s.key in entry)) {
        const empty = s.unit === "ratio" ? null : 0
        entry[s.key] = empty
        entry[exactKey(s.key)] = empty
      }
  }
  return { data, series: keys }
}

// DonutSlice is one pie slice: the plotted Number beside the exact wire cell
// (under exactKey) the tooltip formats, as the time-series rows do.
export type DonutSlice = PivotSeries & {
  value: number
  [exact: string]: unknown
}

// donutSlices turns the result's rows into slices for the primary measure,
// dropping zero slices.
export function donutSlices(
  result: MetricsResult,
  currency?: string,
  pspKey: (id: string) => string = (id) => id
): DonutSlice[] {
  const idx = indexColumns(result.columns)
  const primary = idx.measures[0]
  if (!primary) return []
  return result.rows
    .map((row, i) => {
      const key = `slice-${i}`
      const cell = row[primary.index]
      return {
        key,
        label:
          idx.dims
            .map((d) => {
              const value = String(row[d.index] ?? "")
              return d.name === "psp" && value
                ? pspKey(value)
                : dimValueLabel(d.name, value)
            })
            .filter(Boolean)
            .join(" · ") || primary.name,
        measure: primary.name,
        dimensions: idx.dims.map((d) => row[d.index]),
        unit: primary.unit,
        currency:
          primary.unit === "money"
            ? rowCurrency(row, idx, currency)
            : undefined,
        value: Number(cell ?? 0),
        [exactKey(key)]: cell ?? 0,
      }
    })
    .filter((slice) => slice.value !== 0)
}

// chartColor cycles the shadcn --chart-N tokens.
export function chartColor(i: number): string {
  return `var(--chart-${(i % 5) + 1})`
}

// Payment-health measures open Payments → Health.
const HEALTH_MEASURES = new Set([
  "attempts",
  "authorization_rate",
  "issuer_discretion_decline_rate",
  "purchases",
  "rebills",
  "rebill_rate",
  "first_try_rebill_approval_rate",
  "dunning_recovery_rate",
  "nmi_history_authorizations",
])

// deepLinkFor maps a widget to the admin page carrying the same filter: health
// measures in any viz open the health page, count stats open their list. Null
// = no sensible link.
export function deepLinkFor(
  query: QueryMetricsParams,
  viz: string = "stat"
): string | null {
  const measures = query.measures ?? []
  if (measures.some((m) => HEALTH_MEASURES.has(m))) return "/payments/health"
  if (viz !== "stat" && viz !== "kpi") return null
  const has = (...names: string[]) => names.some((n) => measures.includes(n))
  if (has("mrr_at_risk")) return "/subscriptions?view=at_risk"
  if (has("mrr", "subscribers", "arpa")) return "/subscriptions"
  if (has("cancellations", "subscriber_churn_rate"))
    return "/subscriptions?view=recently_canceled"
  if (has("cash", "payments", "refund_rate", "chargeback_rate"))
    return "/payments"
  if (has("entitled_customers")) return "/customers"
  return null
}

// ownRange says in words what a saved query's range covers.
export function ownRange(range: MetricsRange | undefined): string {
  if (!range) return "its saved range"
  if (range.from && range.to) return `${range.from} to ${range.to}`
  const ahead = !range.last && !!range.next
  const m = /^(\d+)([dwmy])$/.exec((ahead ? range.next : range.last) ?? "")
  if (!m) return "its saved range"
  if (m[1] === "1" && m[2] === "d") return "today"
  const unit = { d: "day", w: "week", m: "month", y: "year" }[m[2]]!
  return `the ${ahead ? "next" : "last"} ${m[1]} ${unit}${m[1] === "1" ? "" : "s"}`
}

export interface FunnelStep {
  key: string
  value: number
  // ofFirst is the share of the first step; fromPrevious of the step before.
  ofFirst: number | null
  fromPrevious: number | null
}

// funnelSteps reads a count by one ordered dimension (order lists its values
// from the first step to the last) as the funnel it describes: each step is
// the rows at it or further.
export function funnelSteps(
  result: MetricsResult,
  order: string[]
): FunnelStep[] {
  const idx = indexColumns(result.columns)
  const dim = idx.dims.find((d) => d.name !== "currency")
  const measure = idx.measures[0]
  if (!dim || !measure) return []
  const at = new Map<string, number>()
  for (const row of result.rows) {
    const key = String(row[dim.index] ?? "")
    at.set(key, (at.get(key) ?? 0) + Number(row[measure.index] ?? 0))
  }
  const keys = order.length > 0 ? order : [...at.keys()]
  const steps: FunnelStep[] = []
  let reached = 0
  for (let i = keys.length - 1; i >= 0; i--) {
    reached += at.get(keys[i]) ?? 0
    steps.unshift({ key: keys[i], value: reached, ofFirst: null, fromPrevious: null })
  }
  const first = steps[0]?.value ?? 0
  steps.forEach((step, i) => {
    step.ofFirst = first > 0 ? step.value / first : null
    const before = steps[i - 1]?.value ?? 0
    step.fromPrevious = i > 0 && before > 0 ? step.value / before : null
  })
  return steps
}
