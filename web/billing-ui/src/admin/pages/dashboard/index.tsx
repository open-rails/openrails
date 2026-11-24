import { AdminMessage, useAdminUI } from "#orck/admin/ui-context"
import { useAdminServices } from "#orck/admin/context"
// The merchant's dashboard: a per-merchant widget grid over the metrics API
// on react-grid-layout v2 — drag, resize, add, edit, remove; the layout
// persists via PUT (debounced). With no saved row it is the health overview
// built for the merchant's profile. The page's range, currency and
// comparison apply to every widget.
import type { AdminAccess } from "#orck/client/generated/wire"
import { useAccess, useDashboardLayout } from "#orck/admin/lib/capabilities"
import * as React from "react"
import { Link, Navigate } from "#orck/admin/router"
import { GridLayout, type Layout } from "react-grid-layout"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"

import "react-grid-layout/css/styles.css"

import { useConfirm } from "#orck/admin/lib/confirm"
import { Button } from "#orck/admin/components/ui/button"
import { Skeleton } from "#orck/admin/components/ui/skeleton"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "#orck/admin/components/ui/select"
import type { Compare, Widget } from "#orck/admin/lib/api/metrics"
import { currencyScale } from "#orck/admin/lib/format"

import { metricsRange, type DashboardRange } from "./range"
import { RangePicker } from "./range-picker"
import { useRegistry } from "./registry"
import { useDashboardView } from "./use-range"
import { WidgetEditor, type WidgetDraft } from "./widget-editor"
import { isTile, WidgetTile } from "./widget-tile"

const COLS = 12
const ROW_HEIGHT = 56

// useMeasuredWidth tracks the live content width of the grid container; the
// ResizeObserver fires on observe and on every resize, so the grid rescales
// with the container instead of trusting a one-shot measurement.
function useMeasuredWidth() {
  // Callback ref: the grid container mounts after loading returns early, so
  // a one-shot ref effect would observe null and never re-attach.
  const [el, setEl] = React.useState<HTMLDivElement | null>(null)
  const [width, setWidth] = React.useState(0)
  React.useEffect(() => {
    if (!el) return
    const observer = new ResizeObserver((entries) => {
      setWidth(Math.round(entries[0].contentRect.width))
    })
    observer.observe(el)
    return () => observer.disconnect()
  }, [el])
  return { containerRef: setEl, width, mounted: width > 0, el }
}

// A tile's label is uppercase 12px medium text at 0.05em tracking, clamped
// to two lines; it needs its best two-line break (the split whose longer
// line is shortest) plus the tile's 16px padding on each side. The saved
// grid is kept wherever every tile has that room; narrower, the dashboard
// reflows instead of cutting labels.
const TILE_PADDING = 32
const MARGIN = 12
function labelRoom(widgets: Widget[], font: string): number {
  const context = document.createElement("canvas").getContext("2d")
  if (!context) return 0
  context.font = `500 12px ${font}`
  const line = (words: string[]) => {
    const text = words.join(" ")
    return context.measureText(text).width + text.length * 0.6
  }
  let widest = 0
  for (const widget of widgets) {
    if (!isTile(widget)) continue
    const words = widget.title.toUpperCase().split(/\s+/).filter(Boolean)
    let best = line(words)
    for (let i = 1; i < words.length; i++)
      best = Math.min(
        best,
        Math.max(line(words.slice(0, i)), line(words.slice(i)))
      )
    widest = Math.max(widest, best)
  }
  return Math.ceil(widest) + TILE_PADDING
}

type Kind = "tile" | "chart" | "heading"
const kindOf = (widget: Widget): Kind =>
  widget.viz === "heading" ? "heading" : isTile(widget) ? "tile" : "chart"

// narrowest is the width react-grid-layout gives the narrowest widget of a
// kind in the saved layout at this container width.
function narrowest(widgets: Widget[], width: number, kind: Kind): number {
  const column = (width - MARGIN * (COLS - 1) - MARGIN * 2) / COLS
  let least = Infinity
  for (const widget of widgets)
    if (kindOf(widget) === kind)
      least = Math.min(
        least,
        column * widget.grid.w + MARGIN * (widget.grid.w - 1)
      )
  return least
}

// runs splits the reflowed order into consecutive runs of one kind, so each
// run gets its own columns while the saved order is kept.
function runs(widgets: Widget[]): { kind: Kind; widgets: Widget[] }[] {
  const out: { kind: Kind; widgets: Widget[] }[] = []
  for (const widget of widgets) {
    const kind = kindOf(widget)
    const last = out.at(-1)
    if (last && last.kind === kind && kind !== "heading")
      last.widgets.push(widget)
    else out.push({ kind, widgets: [widget] })
  }
  return out
}

// Reflowed columns come from the content, not from device sizes: a tile
// needs 10rem (its widest label with room for a four-digit amount), a chart
// 22rem to keep its axis labels readable. Of the columns that fit, the row
// count is kept and the widgets are spread evenly over it, so twelve tiles
// that fit five across sit four, four and four rather than five, five and
// two.
const TILE_MIN = 160
const CHART_MIN = 352
function balancedColumns(count: number, width: number, min: number) {
  const fit = Math.max(1, Math.floor((width + MARGIN) / (min + MARGIN)))
  return Math.ceil(count / Math.ceil(count / fit))
}
// A reflowed widget's height: a kpi has room for its sparkline.
function reflowHeight(widget: Widget): string {
  if (widget.viz === "heading") return "h-9"
  if (widget.viz === "kpi") return "h-[164px]"
  return widget.viz === "stat" ? "h-[124px]" : "h-64"
}
// The loading skeleton's rows: auto-fit, so a few placeholders spread over
// the row rather than leaving empty tracks.
const TILE_FLOW =
  "grid grid-cols-[repeat(auto-fit,minmax(min(100%,10rem),1fr))] gap-3"
const CHART_FLOW =
  "grid grid-cols-[repeat(auto-fit,minmax(min(100%,22rem),1fr))] gap-3"

// stackedWidgets orders widgets by their grid position (top-left first) for
// the single-column mobile flow.
function stackedWidgets<T extends { id: string }>(
  widgets: T[],
  layout: Layout
): T[] {
  const pos = new Map(layout.map((l) => [l.i, l.y * 1000 + l.x]))
  return [...widgets].sort(
    (a, b) => (pos.get(a.id) ?? 0) - (pos.get(b.id) ?? 0)
  )
}

// DashboardPage is the console's home: the metrics dashboard for a caller
// holding Metrics, else their first area.
export function DashboardPage() {
  const access = useAccess()
  if (!access) return null
  if (!access.metrics) return <Navigate to={firstArea(access)} replace />
  return <MetricsDashboard />
}

// firstArea is where a caller without metrics starts.
function firstArea(access: AdminAccess): string {
  if (access.admin === "read" || access.admin === "update") return "/customers"
  if (access.catalog === "read" || access.catalog === "update")
    return "/catalog"
  return "/settings"
}

// useCurrencies lists the currencies with activity in the range, the one
// with the most cash (in major units) first.
function useCurrencies(range: DashboardRange, enabled: boolean): string[] {
  const { adminQueries } = useAdminServices()
  const { data } = useQuery({
    ...adminQueries.widgetMetrics({
      measures: ["cash", "usage_charged", "credits_sold"],
      by: ["currency"],
      range: metricsRange(range),
    }),
    enabled,
  })
  return React.useMemo(() => {
    if (!data) return []
    const cash = data.columns.findIndex((c) => c.name === "cash")
    const major = (row: (typeof data.rows)[number]) =>
      Number(row[cash] ?? 0) / 10 ** (currencyScale(String(row[0])) ?? 0)
    return [...data.rows]
      .sort((a, b) => major(b) - major(a))
      .map((row) => String(row[0]))
  }, [data])
}

const COMPARES: { value: Compare; label: string }[] = [
  { value: "previous_period", label: "vs previous period" },
  { value: "previous_year", label: "vs last year" },
]

// newWidget is where an added widget starts: cash over the range by day.
function newWidget(widgets: Widget[]): Widget {
  const bottom = widgets.reduce((y, w) => Math.max(y, w.grid.y + w.grid.h), 0)
  return {
    id: `widget-${Date.now().toString(36)}`,
    title: "Cash",
    viz: "line",
    query: {
      measures: ["cash"],
      by: ["time"],
      grain: "auto",
      range: { last: "30d" },
    },
    grid: { x: 0, y: bottom, w: 6, h: 4 },
    fixed_range: false,
  }
}

function MetricsDashboard() {
  const { toastApiError, adminMutations, adminQueries } = useAdminServices()
  const { text: adminText } = useAdminUI()

  const queryClient = useQueryClient()
  // The layout is the merchant's configuration, over its metrics: editable
  // only to a caller holding both.
  const editable = useDashboardLayout()
  const { mutate: saveDashboard } = useMutation(
    adminMutations.saveDashboard(queryClient)
  )
  const dashboardOptions = adminQueries.dashboard()
  const { data, isPending: loading } = useQuery(dashboardOptions)
  const [draftWidgets, setDraftWidgets] = React.useState<Widget[] | null>(null)
  const widgets = draftWidgets ?? data?.widgets ?? null
  const [editing, setEditing] = React.useState<{
    widget: Widget
    added: boolean
  } | null>(null)
  const { width, containerRef, mounted, el } = useMeasuredWidth()
  const [view, setView] = useDashboardView()
  const registry = useRegistry()
  const confirm = useConfirm()
  const saveTimer = React.useRef<ReturnType<typeof setTimeout> | null>(null)
  const profile = data?.is_default ? data.profile : null
  const empty =
    !!profile &&
    !profile.subscriptions &&
    !profile.card_payments &&
    !profile.usage &&
    !profile.credits
  const currencies = useCurrencies(view.range, !!data && !empty)
  // The page's currency: the one picked if it has activity, else the one
  // with the most cash.
  const currency =
    view.currency && currencies.includes(view.currency)
      ? view.currency
      : currencies[0]
  const shown = { ...view, currency }

  React.useEffect(
    () => () => {
      if (saveTimer.current) clearTimeout(saveTimer.current)
    },
    []
  )

  // Debounced persistence: drags/resizes coalesce into one PUT.
  const persist = React.useCallback(
    (next: Widget[]) => {
      if (saveTimer.current) clearTimeout(saveTimer.current)
      saveTimer.current = setTimeout(() => {
        saveDashboard(next, {
          onError: (err) => toastApiError(err, "Save dashboard"),
        })
      }, 800)
    },
    [saveDashboard, toastApiError]
  )

  const update = React.useCallback(
    (next: Widget[]) => {
      setDraftWidgets(next)
      persist(next)
    },
    [persist]
  )

  const onLayoutChange = (layout: Layout) => {
    if (!widgets || !editable) return
    const byId = new Map(layout.map((l) => [l.i, l]))
    let changed = false
    const next = widgets.map((w) => {
      const l = byId.get(w.id)
      if (!l) return w
      if (
        l.x !== w.grid.x ||
        l.y !== w.grid.y ||
        l.w !== w.grid.w ||
        l.h !== w.grid.h
      ) {
        changed = true
        return { ...w, grid: { x: l.x, y: l.y, w: l.w, h: l.h } }
      }
      return w
    })
    if (changed) update(next)
  }

  const saveWidget = (id: string, draft: WidgetDraft, added: boolean) => {
    if (!widgets) return
    if (added) {
      update([...widgets, { ...newWidget(widgets), ...draft, id }])
      return
    }
    update(widgets.map((w) => (w.id === id ? { ...w, ...draft } : w)))
  }

  const removeWidget = async (widget: Widget) => {
    if (!widgets) return
    const removing = await confirm({
      title: `Remove "${widget.title}"?`,
      description: "This removes it from the merchant dashboard for everyone.",
      confirmLabel: "Remove widget",
      tone: "destructive",
    })
    if (removing) update(widgets.filter((w) => w.id !== widget.id))
  }

  const header = (
    <div className="flex flex-wrap items-center justify-between gap-3">
      <h1 className="text-2xl font-semibold tracking-tight">
        <AdminMessage id={"Dashboard"} />
      </h1>
      <div className="flex flex-wrap items-center gap-3">
        {currencies.length > 1 ? (
          <Select
            items={currencies.map((c) => ({ value: c, label: c }))}
            value={currency}
            onValueChange={(next) => setView({ currency: next ?? undefined })}
          >
            <SelectTrigger
              size="sm"
              className="w-[96px]"
              aria-label={adminText("Currency")}
            >
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              {currencies.map((c) => (
                <SelectItem key={c} value={c}>
                  {c}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        ) : null}
        <Select
          items={COMPARES}
          value={view.compare ?? "previous_period"}
          onValueChange={(next) =>
            setView({
              compare: next === "previous_year" ? "previous_year" : undefined,
            })
          }
        >
          <SelectTrigger
            size="sm"
            className="w-[170px]"
            aria-label={adminText("Compare with")}
          >
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {COMPARES.map((c) => (
              <SelectItem key={c.value} value={c.value}>
                {c.label}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        <RangePicker />
        {editable && widgets ? (
          <Button
            size="sm"
            variant="outline"
            onClick={() =>
              setEditing({ widget: newWidget(widgets), added: true })
            }
          >
            <AdminMessage id={"Add widget"} />
          </Button>
        ) : null}
      </div>
    </div>
  )

  if (loading && !widgets) {
    return (
      <div className="flex flex-col gap-4">
        {header}
        <div className={TILE_FLOW}>
          {Array.from({ length: 4 }).map((_, i) => (
            <Skeleton key={i} className="h-[164px]" />
          ))}
        </div>
        <div className={CHART_FLOW}>
          <Skeleton className="h-64" />
          <Skeleton className="h-64" />
        </div>
      </div>
    )
  }
  if (!widgets) return null

  // The saved grid needs room for each tile label; the reflow also keeps
  // room for a four-digit amount, and never less than the widest label.
  const labels = el ? labelRoom(widgets, getComputedStyle(el).fontFamily) : 0
  const tileMin = Math.max(TILE_MIN, labels)
  const fits =
    mounted &&
    el !== null &&
    (widgets.some(isTile)
      ? narrowest(widgets, width, "tile") >= labels
      : narrowest(widgets, width, "chart") >= CHART_MIN)

  const layout: Layout = widgets.map((w) => ({
    i: w.id,
    x: w.grid.x,
    y: w.grid.y,
    w: w.grid.w,
    h: w.grid.h,
    minW: w.viz === "heading" ? COLS : 2,
    minH: w.viz === "heading" ? 1 : 2,
  }))
  const tile = (w: Widget) => (
    <WidgetTile
      widget={w}
      view={shown}
      registry={registry}
      empty={empty}
      editable={editable}
      onEdit={() => setEditing({ widget: w, added: false })}
      onDelete={() => void removeWidget(w)}
    />
  )

  return (
    <div className="flex flex-col gap-4">
      {header}
      {empty ? (
        <p className="text-sm text-muted-foreground">
          <AdminMessage id={"Nothing to show yet. Add products in "} />
          <Link to="/catalog" className="underline underline-offset-2">
            <AdminMessage id={"Catalog"} />
          </Link>
          <AdminMessage id={" and connect a PSP in "} />
          <Link
            to="/settings?tab=psps"
            className="underline underline-offset-2"
          >
            <AdminMessage id={"Settings › PSPs"} />
          </Link>
          <AdminMessage id={" to start taking payments."} />
        </p>
      ) : null}

      <div ref={containerRef} className="min-h-0 w-full">
        {widgets.length === 0 ? (
          <div className="flex h-48 flex-col items-center justify-center gap-3 rounded-xl border border-dashed">
            <p className="text-sm text-muted-foreground">
              <AdminMessage id={"No widgets yet"} />
            </p>
          </div>
        ) : mounted && !fits ? (
          <div className="flex flex-col gap-3">
            {runs(stackedWidgets(widgets, layout)).map((run) => (
              <div
                key={run.widgets[0].id}
                className="grid gap-3"
                style={{
                  gridTemplateColumns: `repeat(${
                    run.kind === "heading"
                      ? 1
                      : balancedColumns(
                          run.widgets.length,
                          width,
                          run.kind === "tile" ? tileMin : CHART_MIN
                        )
                  }, minmax(0, 1fr))`,
                }}
              >
                {run.widgets.map((w) => (
                  <div key={w.id} className={reflowHeight(w)}>
                    {tile(w)}
                  </div>
                ))}
              </div>
            ))}
          </div>
        ) : mounted ? (
          <GridLayout
            layout={layout}
            width={width}
            gridConfig={{
              cols: COLS,
              rowHeight: ROW_HEIGHT,
              margin: [12, 12],
            }}
            dragConfig={{ handle: ".widget-drag-handle" }}
            onLayoutChange={onLayoutChange}
          >
            {widgets.map((w) => (
              <div key={w.id}>{tile(w)}</div>
            ))}
          </GridLayout>
        ) : null}
      </div>

      {editing ? (
        <WidgetEditor
          onOpenChange={(open) => {
            if (!open) setEditing(null)
          }}
          initial={editing.widget}
          added={editing.added}
          view={shown}
          registry={registry}
          onSave={(draft) =>
            saveWidget(editing.widget.id, draft, editing.added)
          }
        />
      ) : null}
    </div>
  )
}
