// The Reports page's tables, built from metrics queries over the dashboard's
// range: subscriptions by UTC day, renewals and churn by UTC month.
import type {
  MetricsCell,
  MetricsQuery,
  MetricsResult,
} from "@/lib/api/metrics"

import { metricsRange, rangeDates, type DashboardRange } from "./range"

// MAX_DAYS is the most days the daily report covers (the API's bucket limit).
export const MAX_DAYS = 400

export function rangeDays(range: DashboardRange, now = new Date()): number {
  const { from, to } = rangeDates(range, now)
  return (Date.parse(to) - Date.parse(from)) / 86_400_000 + 1
}

export function dailyQuery(range: DashboardRange): MetricsQuery {
  return {
    measures: ["new_subscriptions", "cancellations", "net_new_subscriptions"],
    by: ["time"],
    grain: "day",
    range: metricsRange(range),
  }
}

export function renewalsQuery(range: DashboardRange): MetricsQuery {
  return {
    measures: ["payment_count"],
    by: ["time"],
    grain: "month",
    range: metricsRange(range),
    filters: { attempt_kind: ["renewal"] },
  }
}

export function churnQuery(range: DashboardRange): MetricsQuery {
  return {
    measures: ["cancellations", "churn_rate"],
    by: ["time"],
    grain: "month",
    range: metricsRange(range),
  }
}

export interface DailyRow {
  // date is the UTC day, YYYY-MM-DD.
  date: string
  started: number
  canceled: number
  net: number
}

export interface MonthlyRow {
  // month is the UTC month, YYYY-MM.
  month: string
  renewals: number
  canceled: number
  // churn is null for a month that began with no subscriptions.
  churn: number | null
}

const count = (cell: MetricsCell | undefined) => Number(cell ?? 0)

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

// dailyRows is the daily report, oldest day first.
export function dailyRows(result: MetricsResult): DailyRow[] {
  return [...cellsByBucket(result)]
    .sort(([a], [b]) => (a < b ? -1 : 1))
    .map(([bucket, cells]) => ({
      date: bucket.slice(0, 10),
      started: count(cells.new_subscriptions),
      canceled: count(cells.cancellations),
      net: count(cells.net_new_subscriptions),
    }))
}

// monthlyRows joins renewals with cancellations and churn, oldest month first.
export function monthlyRows(
  renewals: MetricsResult,
  churn: MetricsResult
): MonthlyRow[] {
  const paid = cellsByBucket(renewals)
  const lost = cellsByBucket(churn)
  const buckets = [...new Set([...paid.keys(), ...lost.keys()])].sort()
  return buckets.map((bucket) => {
    const rate = lost.get(bucket)?.churn_rate
    return {
      month: bucket.slice(0, 7),
      renewals: count(paid.get(bucket)?.payment_count),
      canceled: count(lost.get(bucket)?.cancellations),
      churn: rate === null || rate === undefined ? null : Number(rate),
    }
  })
}

export const dailyCsv = (rows: DailyRow[]) => ({
  header: ["date", "new_subscriptions", "cancellations", "net_change"],
  rows: rows.map((r) => [r.date, r.started, r.canceled, r.net]),
})

export const monthlyCsv = (rows: MonthlyRow[]) => ({
  header: ["month", "renewals", "cancellations", "churn_rate"],
  rows: rows.map((r) => [r.month, r.renewals, r.canceled, r.churn ?? ""]),
})
