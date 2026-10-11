// Metrics and dashboard API; types mirror the Go handlers.
import { api } from "./client"

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

export type WidgetViz = "stat" | "line" | "area" | "bar" | "donut" | "table"

export const WIDGET_VIZ: WidgetViz[] = [
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
  query: MetricsQuery
  grid: WidgetGrid
  // fixed_range keeps the query's own range; other widgets follow the
  // dashboard's range picker.
  fixed_range: boolean
}

export interface Dashboard {
  widgets: Widget[]
  is_default: boolean
  updated_at?: string
  updated_by?: string
}

export const metricsQuery = (query: MetricsQuery, signal?: AbortSignal) =>
  api<MetricsResult>("/metrics/query", {
    method: "POST",
    body: query,
    signal,
  })

export const getDashboard = (signal?: AbortSignal) =>
  api<Dashboard>("/metrics/dashboard", { signal })

export const putDashboard = (widgets: Widget[]) =>
  api<Dashboard>("/merchant/dashboard", { method: "PUT", body: { widgets } })

// AskEvidence is one executed tool query: the query plus its VERBATIM result
// (a MetricsResult) — on-screen numbers come from here, never from prose.
export interface AskEvidence extends MetricsResult {
  query: MetricsQuery
}

export interface AskResponse {
  answer: string
  evidence: AskEvidence[]
}

// askMetrics: free-form question → LLM-run metrics queries + answer. Not
// mounted when llm.ask_enabled / the LLM key are not configured.
export const askMetrics = (question: string) =>
  api<AskResponse>("/metrics/ask", {
    method: "POST",
    body: { question },
  })
