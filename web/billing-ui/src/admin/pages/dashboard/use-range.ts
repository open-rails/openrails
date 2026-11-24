import { useSearchParams } from "#orck/admin/router"
import type { Compare } from "#orck/admin/lib/api/metrics"

import {
  rangeFromParams,
  withRange,
  type DashboardRange,
  type DashboardView,
} from "./range"

// useDashboardRange reads the range from the URL and replaces it there.
export function useDashboardRange(): [
  DashboardRange,
  (range: DashboardRange) => void,
] {
  const [params, setParams] = useSearchParams()
  return [
    rangeFromParams(params),
    (range) => setParams(withRange(params, range), { replace: true }),
  ]
}

// useDashboardView is the range, currency (?currency=EUR) and comparison
// (?compare=previous_year) the page shows, kept in the URL.
export function useDashboardView(): [
  DashboardView,
  (change: Partial<Omit<DashboardView, "range">>) => void,
] {
  const [params, setParams] = useSearchParams()
  const compare = params.get("compare")
  return [
    {
      range: rangeFromParams(params),
      currency: params.get("currency") ?? undefined,
      compare: compare === "previous_year" ? compare : undefined,
    },
    (change) => {
      const next = new URLSearchParams(params)
      for (const [key, value] of Object.entries(change) as [
        string,
        string | Compare | undefined,
      ][])
        if (value) next.set(key, value)
        else next.delete(key)
      setParams(next, { replace: true })
    },
  ]
}
