import { AdminMessage } from "#orck/admin/ui-context"
import { useAdminServices } from "#orck/admin/context"
// #741 configurable dashboard: a per-merchant widget grid over the metrics
// API. Tile registry + serialized Layout[] on react-grid-layout v2 — drag,
// resize, edit, remove; the layout persists via PUT (debounced). First
// load with no saved row shows the seeded default template.
import type { AdminAccess } from "#orck/client/generated/wire"
import { useAccess, useDashboardLayout } from "#orck/admin/lib/capabilities"
import * as React from "react"
import { Navigate } from "#orck/admin/router"
import { GridLayout, type Layout } from "react-grid-layout"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"

import "react-grid-layout/css/styles.css"

import { useConfirm } from "#orck/admin/lib/confirm"
import { Skeleton } from "#orck/admin/components/ui/skeleton"
import type { Widget } from "#orck/admin/lib/api/metrics"

import { RangePicker } from "./range-picker"
import { useDashboardRange } from "./use-range"
import { WidgetEditor, type WidgetDraft } from "./widget-editor"
import { WidgetTile } from "./widget-tile"

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

// A stat tile's label is uppercase 12px medium text at 0.05em tracking,
// clamped to two lines; it needs its best two-line break (the split whose
// longer line is shortest) plus the tile's 16px padding on each side. The
// saved grid is kept wherever every stat tile has that room; narrower, the
// dashboard reflows instead of cutting labels.
const STAT_PADDING = 32
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
    if (widget.viz !== "stat") continue
    const words = widget.title.toUpperCase().split(/\s+/).filter(Boolean)
    let best = line(words)
    for (let i = 1; i < words.length; i++)
      best = Math.min(
        best,
        Math.max(line(words.slice(0, i)), line(words.slice(i)))
      )
    widest = Math.max(widest, best)
  }
  return Math.ceil(widest) + STAT_PADDING
}

// narrowest is the width react-grid-layout gives the narrowest stat tile in
// the saved layout at this container width, or the narrowest chart when
// there are no stat tiles.
function narrowest(widgets: Widget[], width: number, stat: boolean): number {
  const column = (width - MARGIN * (COLS - 1) - MARGIN * 2) / COLS
  let least = Infinity
  for (const widget of widgets)
    if ((widget.viz === "stat") === stat)
      least = Math.min(
        least,
        column * widget.grid.w + MARGIN * (widget.grid.w - 1)
      )
  return least
}

// runs splits the reflowed order into consecutive stat and chart runs, so
// each run gets its own columns while the saved order is kept.
function runs(widgets: Widget[]): { stat: boolean; widgets: Widget[] }[] {
  const out: { stat: boolean; widgets: Widget[] }[] = []
  for (const widget of widgets) {
    const stat = widget.viz === "stat"
    const last = out.at(-1)
    if (last && last.stat === stat) last.widgets.push(widget)
    else out.push({ stat, widgets: [widget] })
  }
  return out
}

// Reflowed columns come from the content, not from device sizes: a stat tile
// needs 10rem (its widest label with room for a four-digit amount), a chart
// 22rem to keep its axis labels readable. Of the columns that fit, the row
// count is kept and the tiles are spread evenly over it, so twelve tiles that
// fit five across sit four, four and four rather than five, five and two.
const STAT_MIN = 160
const CHART_MIN = 352
function balancedColumns(count: number, width: number, min: number) {
  const fit = Math.max(1, Math.floor((width + MARGIN) / (min + MARGIN)))
  return Math.ceil(count / Math.ceil(count / fit))
}
// The loading skeleton's rows: auto-fit, so a few placeholders spread over
// the row rather than leaving empty tracks.
const STAT_FLOW =
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

function MetricsDashboard() {
  const { toastApiError, adminMutations, adminQueries } = useAdminServices()

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
  const [editorOpen, setEditorOpen] = React.useState(false)
  const [editing, setEditing] = React.useState<Widget | null>(null)
  const { width, containerRef, mounted, el } = useMeasuredWidth()
  const [range] = useDashboardRange()
  const confirm = useConfirm()
  const saveTimer = React.useRef<ReturnType<typeof setTimeout> | null>(null)

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

  const editWidget = (id: string, draft: WidgetDraft) => {
    if (!widgets) return
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
        <RangePicker />
      </div>
    </div>
  )

  if (loading && !widgets) {
    return (
      <div className="flex flex-col gap-4">
        {header}
        <div className={STAT_FLOW}>
          {Array.from({ length: 4 }).map((_, i) => (
            <Skeleton key={i} className="h-[124px]" />
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

  // The saved grid needs room for each stat label; the reflow also keeps
  // room for a four-digit amount, and never less than the widest label.
  const labels = el ? labelRoom(widgets, getComputedStyle(el).fontFamily) : 0
  const statMin = Math.max(STAT_MIN, labels)
  const fits =
    mounted &&
    el !== null &&
    (widgets.some((w) => w.viz === "stat")
      ? narrowest(widgets, width, true) >= labels
      : narrowest(widgets, width, false) >= CHART_MIN)

  const layout: Layout = widgets.map((w) => ({
    i: w.id,
    x: w.grid.x,
    y: w.grid.y,
    w: w.grid.w,
    h: w.grid.h,
    minW: 2,
    minH: 2,
  }))

  return (
    <div className="flex flex-col gap-4">
      {header}

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
                  gridTemplateColumns: `repeat(${balancedColumns(
                    run.widgets.length,
                    width,
                    run.stat ? statMin : CHART_MIN
                  )}, minmax(0, 1fr))`,
                }}
              >
                {run.widgets.map((w) => (
                  <div key={w.id} className={run.stat ? "h-[124px]" : "h-64"}>
                    <WidgetTile
                      widget={w}
                      range={range}
                      editable={editable}
                      onEdit={() => {
                        setEditing(w)
                        setEditorOpen(true)
                      }}
                      onDelete={() => void removeWidget(w)}
                    />
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
              <div key={w.id}>
                <WidgetTile
                  widget={w}
                  range={range}
                  editable={editable}
                  onEdit={() => {
                    setEditing(w)
                    setEditorOpen(true)
                  }}
                  onDelete={() => void removeWidget(w)}
                />
              </div>
            ))}
          </GridLayout>
        ) : null}
      </div>

      {editorOpen && editing ? (
        <WidgetEditor
          onOpenChange={setEditorOpen}
          initial={editing}
          onSave={(draft) => editWidget(editing.id, draft)}
        />
      ) : null}
    </div>
  )
}
