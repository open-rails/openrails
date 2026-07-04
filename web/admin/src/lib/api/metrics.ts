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

// --- metrics schema (drives the manual builder) --------------------------------

export interface SchemaMeasure {
  name: string
  class: string
  unit: string
  description: string
  formula: string
  dims: string[]
  money?: boolean
}

export interface SchemaDimension {
  name: string
  description: string
  values?: string[]
}

export interface SchemaExample {
  intent: string
  query: MetricsQuery
}

export interface MetricsSchema {
  measures: SchemaMeasure[]
  dimensions: SchemaDimension[]
  grains: string[]
  examples: SchemaExample[]
  limits: { max_buckets: number; max_limit: number }
  query_shape: string
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

export const metricsSchema = () => api<MetricsSchema>("/merchant/metrics/schema")

export const getDashboard = () => api<Dashboard>("/merchant/dashboard")

export const putDashboard = (widgets: Widget[]) =>
  api<Dashboard>("/merchant/dashboard", { method: "PUT", body: { widgets } })

export const generateWidget = (prompt: string) =>
  api<GeneratedWidget>("/merchant/dashboard/widgets/generate", {
    method: "POST",
    body: { prompt },
  })
