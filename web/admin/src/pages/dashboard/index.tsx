// #741 configurable dashboard: a per-merchant widget grid over the metrics
// API. Tile registry + serialized Layout[] on react-grid-layout v2 — drag,
// resize, edit, remove; the layout persists via PUT (debounced). First
// load with no saved row shows the seeded default template.
import type { AdminAccess } from "@/lib/api/generated/wire"
import { useAccess, useDashboardLayout } from "@/lib/capabilities"
import * as React from "react"
import { Navigate } from "react-router-dom"
import { GridLayout, type Layout } from "react-grid-layout"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"

import "react-grid-layout/css/styles.css"

import { useConfirm } from "@/lib/confirm"
import { Skeleton } from "@/components/ui/skeleton"
import type { Widget } from "@/lib/api/metrics"
import { adminMutations } from "@/lib/mutations"
import { toastApiError } from "@/lib/toast"
import { adminQueries } from "@/lib/queries"

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
  return { containerRef: setEl, width, mounted: width > 0 }
}

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
  if (access.admin !== "none") return "/customers"
  if (access.catalog) return "/catalog"
  return "/settings"
}

function MetricsDashboard() {
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
  const { width, containerRef, mounted } = useMeasuredWidth()
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
    [saveDashboard]
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
      <h1 className="text-2xl font-semibold tracking-tight">Dashboard</h1>
      <div className="flex flex-wrap items-center gap-3">
        <RangePicker />
      </div>
    </div>
  )

  if (loading && !widgets) {
    return (
      <div className="flex flex-col gap-4">
        {header}
        <div className="grid grid-cols-1 gap-3 md:grid-cols-4">
          {Array.from({ length: 4 }).map((_, i) => (
            <Skeleton key={i} className="h-[124px]" />
          ))}
          <Skeleton className="h-64 md:col-span-2" />
          <Skeleton className="h-64 md:col-span-2" />
        </div>
      </div>
    )
  }
  if (!widgets) return null

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
            <p className="text-sm text-muted-foreground">No widgets yet</p>
          </div>
        ) : (
          <>
            <div className="grid gap-3 md:hidden">
              {stackedWidgets(widgets, layout).map((w) => (
                <div
                  key={w.id}
                  className={w.viz === "stat" ? "h-[124px]" : "h-64"}
                >
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
            <div className="hidden md:block">
              {mounted ? (
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
          </>
        )}
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
