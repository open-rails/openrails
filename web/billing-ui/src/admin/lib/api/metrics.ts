// Metrics and dashboard API; types mirror the Go handlers.
import type {
  DashboardProfile,
  DisputeStanding,
  ListPage,
  MetricsSchema,
} from "#orck/client/generated/wire"
import type { AdminClient } from "./client"

export type {
  DashboardProfile,
  DisputeStanding,
  MetricsMeasure,
  MetricsSchema,
  MetricsThreshold,
} from "#orck/client/generated/wire"

export interface MetricsRange {
  from?: string
  to?: string
  // Relative trailing window ending today ("7d", "12w", "6m", "1y") —
  // mutually exclusive with from/to; saved widgets prefer it.
  last?: string
}

export interface OrderTerm {
  measure?: string
  dimension?: string
  dir?: "asc" | "desc"
}

export interface QueryMetricsParams {
  measures: string[]
  by?: string[]
  grain?: string
  range: MetricsRange
  filters?: Record<string, string[]>
  order?: OrderTerm[]
  limit?: number
  compare?: Compare
}

// Compare is the window a change is measured against.
export type Compare = "previous_period" | "previous_year"

export interface MetricsColumn {
  name: string
  kind: "time" | "dimension" | "measure"
  unit?: string
}

export type MetricsCell = string | number | null

export interface MetricsResult {
  grain?: string
  range: { from: string; to: string }
  columns: MetricsColumn[]
  rows: MetricsCell[][]
  compare_range?: { from: string; to: string }
  compare_rows?: MetricsCell[][]
}

// A kpi is a tile's value, its change and a sparkline; a heading is a
// section title and programs the dispute program standings, both with no
// query.
export type WidgetViz =
  | "kpi"
  | "stat"
  | "line"
  | "area"
  | "bar"
  | "donut"
  | "table"
  | "heading"
  | "programs"

// queryless: a viz that draws no metrics query.
export const queryless = (viz: WidgetViz) =>
  viz === "heading" || viz === "programs"

// CHART_VIZ are the vizzes a widget with a query may take.
export const CHART_VIZ: WidgetViz[] = [
  "kpi",
  "stat",
  "line",
  "area",
  "bar",
  "donut",
  "table",
]

export interface WidgetGrid {
  x: number
  y: number
  w: number
  h: number
}

export interface Widget {
  id: string
  title: string
  viz: WidgetViz
  query: QueryMetricsParams
  grid: WidgetGrid
  // fixed_range keeps the query's own range; other widgets follow the
  // dashboard's range picker.
  fixed_range: boolean
}

export interface Dashboard {
  widgets: Widget[]
  is_default: boolean
  // profile is what the default layout was built for; null on a saved one.
  profile?: DashboardProfile | null
  updated_at?: string
  updated_by?: string
}

export function createMetrics(client: AdminClient) {
  const { api } = client
  const metricsQuery = (query: QueryMetricsParams, signal?: AbortSignal) =>
    api<MetricsResult>("/metrics/query", {
      method: "POST",
      body: query,
      signal,
    })

  const getDashboard = (signal?: AbortSignal) =>
    api<Dashboard>("/metrics/dashboard", { signal })

  const getMetricsSchema = (signal?: AbortSignal) =>
    api<MetricsSchema>("/metrics/schema", { signal })

  const putDashboard = (widgets: Widget[]) =>
    api<Dashboard>("/merchant/dashboard", { method: "PUT", body: { widgets } })

  // Each card PSP's standing in the dispute programs for the month holding
  // at (now when absent).
  const listDisputePrograms = (at?: string, signal?: AbortSignal) =>
    api<ListPage<DisputeStanding>>(
      `/metrics/dispute-programs${at ? `?at=${encodeURIComponent(at)}` : ""}`,
      { signal }
    )

  return {
    metricsQuery,
    getDashboard,
    getMetricsSchema,
    putDashboard,
    listDisputePrograms,
  }
}
