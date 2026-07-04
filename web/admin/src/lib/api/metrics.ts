// #733 metrics API + #741 dashboard API — types mirror the Go handlers.
import { api } from "./client"

// --- metrics query -----------------------------------------------------------

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

export interface MetricsQuery {
  measures: string[]
  by?: string[]
  grain?: string
  range: MetricsRange
  filters?: Record<string, string[]>
  order?: OrderTerm[]
  limit?: number
  compare?: "previous_period"
}

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

// --- dashboard ---------------------------------------------------------------

export type WidgetViz = "stat" | "line" | "area" | "bar" | "donut" | "table"

export const WIDGET_VIZ: WidgetViz[] = ["stat", "line", "area", "bar", "donut", "table"]

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
  query: MetricsQuery
  grid: WidgetGrid
}

export interface Dashboard {
  widgets: Widget[]
  is_default: boolean
  updated_at?: string
  updated_by?: string
}

export interface GeneratedWidget {
  query: MetricsQuery
  title: string
  viz: WidgetViz
}

// --- endpoints -----------------------------------------------------------------

export const metricsQuery = (query: MetricsQuery) =>
  api<MetricsResult>("/merchant/metrics/query", { method: "POST", body: query })

export const getDashboard = () => api<Dashboard>("/merchant/dashboard")

export const putDashboard = (widgets: Widget[]) =>
  api<Dashboard>("/merchant/dashboard", { method: "PUT", body: { widgets } })

// generateWidget: NL prompt → validated {query,title,viz}. baseQuery (an
// existing widget's query) makes the prompt a refinement of it (#755).
export const generateWidget = (prompt: string, baseQuery?: MetricsQuery) =>
  api<GeneratedWidget>("/merchant/dashboard/widgets/generate", {
    method: "POST",
    body: baseQuery ? { prompt, base_query: baseQuery } : { prompt },
  })
