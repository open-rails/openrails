import { AdminMessage } from "#orck/admin/ui-context"
// Ops › Signals: the ops measures, operator signals kept off the business
// overview. Each has a finding at its threshold; this page shows how they
// move.
import { Navigate } from "#orck/admin/router"
import { useAccess } from "#orck/admin/lib/capabilities"
import { STAT_TILES_4 } from "#orck/admin/lib/tile-grids"
import type { Widget } from "#orck/admin/lib/api/metrics"

import { RangePicker } from "#orck/admin/pages/dashboard/range-picker"
import { useRegistry } from "#orck/admin/pages/dashboard/registry"
import { useDashboardView } from "#orck/admin/pages/dashboard/use-range"
import { WidgetTile } from "#orck/admin/pages/dashboard/widget-tile"

const range = { last: "30d" }
const grid = { x: 0, y: 0, w: 1, h: 1 }

const tile = (id: string, title: string, measure: string): Widget => ({
  id,
  title,
  viz: "kpi",
  query: { measures: [measure], range, compare: "previous_period" },
  grid,
  fixed_range: false,
})

const chart = (
  id: string,
  title: string,
  viz: Widget["viz"],
  query: Omit<Widget["query"], "range">
): Widget => ({
  id,
  title,
  viz,
  query: { ...query, range },
  grid,
  fixed_range: false,
})

export const SIGNAL_TILES: Widget[] = [
  tile("open-findings", "Open findings", "open_findings"),
  tile(
    "verification-pressure",
    "Unverified past paid-through",
    "verification_pressure"
  ),
  tile(
    "verification-age",
    "Oldest unverified",
    "verification_pressure_age_seconds"
  ),
  tile("entitled-customers", "Entitled customers", "entitled_customers"),
]

export const SIGNAL_CHARTS: Widget[] = [
  chart("findings-by-group", "Open findings by group", "bar", {
    measures: ["open_findings"],
    by: ["time", "finding_group"],
    grain: "auto",
  }),
  chart("missed-rebills", "Missed rebills by reason", "table", {
    measures: ["rebills"],
    by: ["miss_reason", "psp"],
    filters: { first_outcome: ["missed"] },
  }),
  chart("webhook-silence", "Webhook silence by PSP", "table", {
    measures: ["webhook_silence_age_seconds"],
    by: ["psp"],
  }),
  chart("webhook-faults", "Webhook rejects and drift", "bar", {
    measures: ["webhook_rejects", "webhook_drift_events"],
    by: ["time"],
    grain: "auto",
  }),
  chart("verification-trend", "Verification pressure", "line", {
    measures: ["verification_pressure"],
    by: ["time"],
    grain: "auto",
  }),
]

export function SignalsPage() {
  const access = useAccess()
  const [view] = useDashboardView()
  const registry = useRegistry()
  if (!access) return null
  if (!access.metrics) return <Navigate to="/ops" replace />
  const draw = (w: Widget) => (
    <WidgetTile
      key={w.id}
      widget={w}
      view={view}
      registry={registry}
      editable={false}
      onEdit={() => {}}
      onDelete={() => {}}
    />
  )
  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <h1 className="text-2xl font-semibold tracking-tight">
          <AdminMessage id={"Signals"} />
        </h1>
        <RangePicker />
      </div>
      <div className={STAT_TILES_4}>
        {SIGNAL_TILES.map((w) => (
          <div key={w.id} className="h-[164px]">
            {draw(w)}
          </div>
        ))}
      </div>
      <div className="grid grid-cols-[repeat(auto-fit,minmax(min(100%,22rem),1fr))] gap-3">
        {SIGNAL_CHARTS.map((w) => (
          <div key={w.id} className="h-64">
            {draw(w)}
          </div>
        ))}
      </div>
    </div>
  )
}
