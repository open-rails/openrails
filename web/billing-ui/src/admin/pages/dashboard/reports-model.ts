// The Reports page's tables, built from metrics queries over the dashboard's
// range: subscribers gained and lost by day, renewals and churn by month, in
// the merchant's accounting timezone, and the explorer's table of any query.
import type {
  MetricsCell,
  QueryMetricsParams,
  MetricsResult,
} from "#orck/admin/lib/api/metrics"

import { metricsRange, rangeDates, type DashboardRange } from "./range"

// MAX_DAYS is the most days the daily report covers (the API's bucket limit).
export const MAX_DAYS = 400

export function rangeDays(range: DashboardRange, now = new Date()): number {
  const { from, to } = rangeDates(range, now)
  return (Date.parse(to) - Date.parse(from)) / 86_400_000 + 1
}

export function dailyQuery(range: DashboardRange): QueryMetricsParams {
  return {
    measures: ["subscriber_movements"],
    by: ["time", "movement"],
    grain: "day",
    range: metricsRange(range),
  }
}

export function renewalsQuery(range: DashboardRange): QueryMetricsParams {
  return {
    measures: ["payments"],
    by: ["time"],
    grain: "month",
    range: metricsRange(range),
    filters: { payment_kind: ["sale"], kind: ["renewal"] },
  }
}

export function churnQuery(range: DashboardRange): QueryMetricsParams {
  return {
    measures: ["cancellations", "subscriber_churn_rate"],
    by: ["time"],
    grain: "month",
    range: metricsRange(range),
  }
}

export interface DailyRow {
  // date is the day in the result's timezone, YYYY-MM-DD.
  date: string
  // started: customers who gained MRR from none (new or back); canceled:
  // those who lost all of it; net: every change, imports and corrections too.
  started: number
  canceled: number
  net: number
}

export interface MonthlyRow {
  // month is the month in the result's timezone, YYYY-MM.
  month: string
  renewals: number
  canceled: number
  // churn is null for a month that began with no subscriptions.
  churn: number | null
}

const count = (cell: MetricsCell | undefined) => Number(cell ?? 0)

// calendarDate is the YYYY-MM-DD date a bucket's instant falls on in
// timeZone.
function calendarDate(iso: string, timeZone = "UTC"): string {
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso.slice(0, 10)
  return new Intl.DateTimeFormat("en-CA", {
    timeZone,
    year: "numeric",
    month: "2-digit",
    day: "2-digit",
  }).format(d)
}

// cellsByBucket keys each row's measures by its time bucket.
function cellsByBucket(
  result: MetricsResult
): Map<string, Record<string, MetricsCell>> {
  const time = result.columns.findIndex((c) => c.kind === "time")
  const out = new Map<string, Record<string, MetricsCell>>()
  for (const row of result.rows) {
    const cells: Record<string, MetricsCell> = {}
    result.columns.forEach((c, i) => {
      cells[c.name] = row[i]
    })
    out.set(String(row[time]), cells)
  }
  return out
}

// dailyRows is the daily report, oldest day first: each day's subscriber
// movements folded.
export function dailyRows(result: MetricsResult): DailyRow[] {
  const time = result.columns.findIndex((c) => c.kind === "time")
  const movement = result.columns.findIndex((c) => c.name === "movement")
  const value = result.columns.findIndex(
    (c) => c.name === "subscriber_movements"
  )
  const days = new Map<string, DailyRow>()
  for (const row of result.rows) {
    const bucket = String(row[time])
    const day = days.get(bucket) ?? {
      date: calendarDate(bucket, result.timezone),
      started: 0,
      canceled: 0,
      net: 0,
    }
    const n = count(row[value])
    const kind = String(row[movement])
    if (kind === "new" || kind === "reactivation") day.started += n
    if (kind === "churn") day.canceled -= n
    day.net += n
    days.set(bucket, day)
  }
  return [...days].sort(([a], [b]) => (a < b ? -1 : 1)).map(([, day]) => day)
}

// monthlyRows joins renewals with cancellations and churn, oldest month first.
export function monthlyRows(
  renewals: MetricsResult,
  churn: MetricsResult
): MonthlyRow[] {
  const paid = cellsByBucket(renewals)
  const lost = cellsByBucket(churn)
  const buckets = [...new Set([...paid.keys(), ...lost.keys()])].sort()
  const zone = renewals.timezone ?? churn.timezone
  return buckets.map((bucket) => {
    const rate = lost.get(bucket)?.subscriber_churn_rate
    return {
      month: calendarDate(bucket, zone).slice(0, 7),
      renewals: count(paid.get(bucket)?.payments),
      canceled: count(lost.get(bucket)?.cancellations),
      churn: rate === null || rate === undefined ? null : Number(rate),
    }
  })
}

export const dailyCsv = (rows: DailyRow[]) => ({
  header: ["date", "new_subscribers", "churned_subscribers", "net_change"],
  rows: rows.map((r) => [r.date, r.started, r.canceled, r.net]),
})

// resultCsv is any result as a table: its columns, then its rows.
export const resultCsv = (result: MetricsResult) => ({
  header: result.columns.map((c) => c.name),
  rows: result.rows,
})

export const monthlyCsv = (rows: MonthlyRow[]) => ({
  header: ["month", "renewals", "cancellations", "subscriber_churn_rate"],
  rows: rows.map((r) => [r.month, r.renewals, r.canceled, r.churn ?? ""]),
})
