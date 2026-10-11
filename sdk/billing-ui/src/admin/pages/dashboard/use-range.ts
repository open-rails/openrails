import { useSearchParams } from "#orck/admin/router"

import { rangeFromParams, withRange, type DashboardRange } from "./range"

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
