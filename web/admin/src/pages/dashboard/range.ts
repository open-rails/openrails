// The dashboard's range, kept in the URL: a preset (?range=7d) or a custom
// span of UTC dates (?from=2026-09-01&to=2026-09-30, both inclusive). Every
// widget runs over it except one that keeps its own (fixed_range).
import type { MetricsQuery, MetricsRange, Widget } from "@/lib/api/metrics"

export const RANGE_PRESETS = [
  { value: "7d", label: "Last 7 days" },
  { value: "30d", label: "Last 30 days" },
  { value: "90d", label: "Last 3 months" },
  { value: "6m", label: "Last 6 months" },
  { value: "1y", label: "Last year" },
] as const

export const DEFAULT_PRESET = "30d"

export type DashboardRange = { preset: string } | { from: string; to: string }

const DATE = /^\d{4}-\d{2}-\d{2}$/

const validDate = (s: string | null): s is string =>
  !!s && DATE.test(s) && !Number.isNaN(Date.parse(`${s}T00:00:00Z`))

export function rangeFromParams(params: URLSearchParams): DashboardRange {
  const from = params.get("from")
  const to = params.get("to")
  if (validDate(from) && validDate(to)) {
    return from <= to ? { from, to } : { from: to, to: from }
  }
  const preset = params.get("range")
  return {
    preset: RANGE_PRESETS.some((p) => p.value === preset)
      ? preset!
      : DEFAULT_PRESET,
  }
}

// withRange is params showing range; the default preset leaves no trace.
export function withRange(
  params: URLSearchParams,
  range: DashboardRange
): URLSearchParams {
  const p = new URLSearchParams(params)
  p.delete("range")
  p.delete("from")
  p.delete("to")
  if ("from" in range) {
    p.set("from", range.from)
    p.set("to", range.to)
  } else if (range.preset !== DEFAULT_PRESET) {
    p.set("range", range.preset)
  }
  return p
}

export function metricsRange(range: DashboardRange): MetricsRange {
  return "from" in range
    ? { from: range.from, to: range.to }
    : { last: range.preset }
}

const dayMs = 86_400_000

const utcDate = (d: Date) => d.toISOString().slice(0, 10)

// rangeDates is the range as inclusive UTC dates. A preset ends today and
// counts back as the API resolves "last": "6m" is six calendar months.
export function rangeDates(
  range: DashboardRange,
  now = new Date()
): { from: string; to: string } {
  if ("from" in range) return range
  const [, n, unit] = /^(\d+)([dwmy])$/.exec(range.preset) ?? ["", "30", "d"]
  const count = Number(n)
  const start = new Date(
    Date.UTC(now.getUTCFullYear(), now.getUTCMonth(), now.getUTCDate() + 1)
  )
  if (unit === "d") start.setUTCDate(start.getUTCDate() - count)
  if (unit === "w") start.setUTCDate(start.getUTCDate() - 7 * count)
  if (unit === "m") start.setUTCMonth(start.getUTCMonth() - count)
  if (unit === "y") start.setUTCFullYear(start.getUTCFullYear() - count)
  return { from: utcDate(start), to: utcDate(now) }
}

export function rangeLabel(range: DashboardRange, now = new Date()): string {
  const { from, to } = rangeDates(range, now)
  const fmt = (s: string) =>
    new Date(`${s}T00:00:00Z`).toLocaleDateString(undefined, {
      month: "short",
      day: "numeric",
      year: s.slice(0, 4) === utcDate(now).slice(0, 4) ? undefined : "numeric",
      timeZone: "UTC",
    })
  return from === to ? fmt(from) : `${fmt(from)} – ${fmt(to)}`
}

// MAX_BUCKETS is the metrics API's limit on a time series.
const MAX_BUCKETS = 400
const GRAINS = ["day", "week", "month", "quarter", "year"]
const GRAIN_DAYS: Record<string, number> = {
  day: 1,
  week: 7,
  month: 28,
  quarter: 89,
  year: 365,
}

// fitGrain coarsens grain until the range fits the bucket limit, so a long
// custom range never turns a daily chart into an error.
export function fitGrain(
  grain: string,
  range: DashboardRange,
  now = new Date()
): string {
  const { from, to } = rangeDates(range, now)
  const days = (Date.parse(to) - Date.parse(from)) / dayMs + 1
  let i = Math.max(GRAINS.indexOf(grain), 0)
  while (
    i < GRAINS.length - 1 &&
    days / GRAIN_DAYS[GRAINS[i]] > MAX_BUCKETS - 2
  )
    i++
  return GRAINS[i]
}

// widgetQuery is the query a widget runs on a dashboard showing range.
export function widgetQuery(
  widget: Widget,
  range: DashboardRange
): MetricsQuery {
  if (widget.fixed_range) return widget.query
  const query: MetricsQuery = { ...widget.query, range: metricsRange(range) }
  if (query.grain) query.grain = fitGrain(query.grain, range)
  return query
}
