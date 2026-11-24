// The metrics registry as the console reads it (GET /v1/metrics/schema):
// each measure's group, direction, thresholds and dimensions. Widget kinds
// read it, so a new measure draws without console changes.
import * as React from "react"
import { useQuery } from "@tanstack/react-query"

import { useAdminServices } from "#orck/admin/context"
import { useMetrics } from "#orck/admin/lib/capabilities"
import type {
  MetricsMeasure,
  MetricsSchema,
  MetricsThreshold,
  QueryMetricsParams,
} from "#orck/admin/lib/api/metrics"

export type Direction = "up_is_good" | "down_is_good" | "neutral"

export interface Registry {
  measures: MetricsMeasure[]
  measure: (name: string) => MetricsMeasure | undefined
  // direction is which way a change of the measure is good news.
  direction: (name: string) => Direction
  thresholds: (name: string) => MetricsThreshold[]
  // dims are the dimensions every named measure allows.
  dims: (measures: string[]) => string[]
  // values is a dimension's closed vocabulary on every named measure; null
  // when it is open.
  values: (measures: string[], dim: string) => string[] | null
  // takesCurrency: every measure of the query may be filtered by currency.
  takesCurrency: (query: QueryMetricsParams) => boolean
}

export function registry(schema?: MetricsSchema): Registry {
  const byName = new Map((schema?.measures ?? []).map((m) => [m.name, m]))
  const dimValues = new Map(
    (schema?.dimensions ?? []).map((d) => [d.name, d.values ?? null])
  )
  const dims = (measures: string[]) => {
    const [first, ...rest] = measures.map((m) => byName.get(m)?.dims ?? [])
    return (first ?? []).filter((d) => rest.every((r) => r.includes(d)))
  }
  return {
    measures: schema?.measures ?? [],
    measure: (name) => byName.get(name),
    direction: (name) =>
      (byName.get(name)?.direction as Direction | undefined) ?? "neutral",
    thresholds: (name) => byName.get(name)?.thresholds ?? [],
    dims,
    values: (measures, dim) => {
      let out: string[] | null = null
      for (const name of measures) {
        const own = byName.get(name)?.dim_values?.[dim] ?? dimValues.get(dim)
        if (!own) continue
        out = out ? out.filter((v) => own.includes(v)) : own
      }
      return out
    },
    takesCurrency: (query) =>
      query.measures.length > 0 && dims(query.measures).includes("currency"),
  }
}

export function useRegistry(): Registry {
  const { adminQueries } = useAdminServices()
  const { data } = useQuery(adminQueries.metricsSchema(useMetrics()))
  return React.useMemo(() => registry(data), [data])
}

// ACRONYMS are the words a label spells in capitals.
const ACRONYMS: Record<string, string> = {
  psp: "PSP",
  mrr: "MRR",
  nrr: "NRR",
  grr: "GRR",
  arpa: "ARPA",
  ltv: "LTV",
  vamp: "VAMP",
  ecm: "ECM",
  bin: "BIN",
}

// measureLabel names a measure or column for people: mrr_movements reads
// "MRR movements".
export function measureLabel(name: string): string {
  const words = name
    .split("_")
    .filter(Boolean)
    .map((w) => ACRONYMS[w] ?? w)
    .join(" ")
  return words.charAt(0).toUpperCase() + words.slice(1)
}
