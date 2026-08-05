// #741 configurable dashboard: a per-merchant widget grid over the metrics
// API. Tile registry + serialized Layout[] on react-grid-layout v2 — drag,
// resize, add, edit, remove; the layout persists via PUT (debounced). First
// load with no saved row shows the seeded default template.
import { HugeiconsIcon } from "@hugeicons/react"
import { Add01Icon } from "@hugeicons/core-free-icons"
import * as React from "react"
import { GridLayout, type Layout } from "react-grid-layout"

import "react-grid-layout/css/styles.css"

import { Button } from "@/components/ui/button"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { Skeleton } from "@/components/ui/skeleton"
import { useApiData } from "@/hooks/use-api-data"
import { getBootstrap } from "@/lib/api/client"
import {
  getDashboard,
  putDashboard,
  type MetricsQuery,
  type MetricsRange,
  type Widget,
  type WidgetViz,
} from "@/lib/api/metrics"
import { toastApiError } from "@/lib/toast"

import { AskPanel } from "./ask-panel"
import { newWidgetId } from "./lib"
import { WidgetEditor } from "./widget-editor"
import { WidgetTile } from "./widget-tile"

const COLS = 12
const ROW_HEIGHT = 56

const RANGES = [
  { value: "7d", label: "Last 7 days", days: 7 },
  { value: "30d", label: "Last 30 days", days: 30 },
  { value: "90d", label: "Last 3 months", days: 90 },
  { value: "6m", label: "Last 6 months", days: 182 },
  { value: "1y", label: "Last year", days: 365 },
]

// rangeSpan resolves a trailing window to concrete dates ("May 6 – Aug 4").
function rangeSpan(last: string): string {
  const days = RANGES.find((r) => r.value === last)?.days ?? 30
  const to = new Date()
  const from = new Date(to)
  from.setDate(from.getDate() - days)
  const fmt = (d: Date) =>
    d.toLocaleDateString(undefined, { month: "short", day: "numeric" })
  return `${fmt(from)} – ${fmt(to)}`
}

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

function nlWidgetsEnabled(): boolean {
  try {
    return getBootstrap().nl_widgets_enabled
  } catch {
    return false
  }
}

function askEnabled(): boolean {
  try {
    return getBootstrap().ask_enabled
  } catch {
    return false
  }
}

// Default tile size per viz for newly added widgets.
function defaultSize(viz: WidgetViz): { w: number; h: number } {
  return viz === "stat" ? { w: 3, h: 2 } : { w: 6, h: 4 }
}

export function DashboardPage() {
  const { data, loading, error } = useApiData(getDashboard, [])
  const [widgets, setWidgets] = React.useState<Widget[] | null>(null)
  const [editorOpen, setEditorOpen] = React.useState(false)
  const [editing, setEditing] = React.useState<Widget | null>(null)
  // seed pre-fills the editor for a NEW widget (ask evidence → add-as-widget).
  const [seed, setSeed] = React.useState<{
    title: string
    viz: WidgetViz
    query: MetricsQuery
  } | null>(null)
  const { width, containerRef, mounted } = useMeasuredWidth()
  const [rangeLast, setRangeLast] = React.useState("30d")
  const saveTimer = React.useRef<ReturnType<typeof setTimeout> | null>(null)
  const range: MetricsRange = { last: rangeLast }

  React.useEffect(() => {
    if (error) toastApiError(error, "Load dashboard")
  }, [error])
  React.useEffect(() => {
    // Adopt the server copy (saved layout or seeded default) once per load.
    // eslint-disable-next-line react-hooks/set-state-in-effect -- deliberate: sync server state into the editable draft
    if (data) setWidgets(data.widgets)
  }, [data])
  React.useEffect(
    () => () => {
      if (saveTimer.current) clearTimeout(saveTimer.current)
    },
    []
  )

  // Debounced persistence: drags/resizes coalesce into one PUT.
  const persist = React.useCallback((next: Widget[]) => {
    if (saveTimer.current) clearTimeout(saveTimer.current)
    saveTimer.current = setTimeout(() => {
      putDashboard(next).catch((err) => toastApiError(err, "Save dashboard"))
    }, 800)
  }, [])

  const update = React.useCallback(
    (next: Widget[]) => {
      setWidgets(next)
      persist(next)
    },
    [persist]
  )

  const onLayoutChange = (layout: Layout) => {
    if (!widgets) return
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

  const addWidget = (draft: {
    title: string
    viz: WidgetViz
    query: MetricsQuery
  }) => {
    if (!widgets) return
    const size = defaultSize(draft.viz)
    const y = widgets.reduce((max, w) => Math.max(max, w.grid.y + w.grid.h), 0)
    update([
      ...widgets,
      {
        id: newWidgetId(),
        title: draft.title,
        viz: draft.viz,
        query: draft.query,
        grid: { x: 0, y, ...size },
      },
    ])
  }

  const editWidget = (
    id: string,
    draft: { title: string; viz: WidgetViz; query: MetricsQuery }
  ) => {
    if (!widgets) return
    update(
      widgets.map((w) =>
        w.id === id
          ? { ...w, title: draft.title, viz: draft.viz, query: draft.query }
          : w
      )
    )
  }

  const removeWidget = (id: string) => {
    if (!widgets) return
    update(widgets.filter((w) => w.id !== id))
  }

  const openAdd = () => {
    setEditing(null)
    setSeed(null)
    setEditorOpen(true)
  }

  const header = (
    <div className="flex flex-wrap items-center justify-between gap-3">
      <h1 className="text-2xl font-semibold tracking-tight">Dashboard</h1>
      <div className="flex items-center gap-3">
        <span className="hidden text-sm text-muted-foreground tabular-nums sm:inline">
          {rangeSpan(rangeLast)}
        </span>
        <Select
          items={RANGES.map((r) => ({ value: r.value, label: r.label }))}
          value={rangeLast}
          onValueChange={(next) => {
            if (next) setRangeLast(next)
          }}
        >
          <SelectTrigger size="sm" className="w-[150px]">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {RANGES.map((r) => (
              <SelectItem key={r.value} value={r.value}>
                {r.label}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        <Button size="sm" onClick={openAdd}>
          <HugeiconsIcon icon={Add01Icon} className="size-4" /> Add widget
        </Button>
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
      <AskPanel
        enabled={askEnabled()}
        onAddWidget={(draft) => {
          setEditing(null)
          setSeed(draft)
          setEditorOpen(true)
        }}
      />

      <div ref={containerRef} className="min-h-0 w-full">
        {widgets.length === 0 ? (
          <div className="flex h-48 flex-col items-center justify-center gap-3 rounded-xl border border-dashed">
            <p className="text-sm text-muted-foreground">No widgets yet</p>
            <Button size="sm" variant="outline" onClick={openAdd}>
              <HugeiconsIcon icon={Add01Icon} className="size-4" /> Add widget
            </Button>
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
                    onEdit={() => {
                      setEditing(w)
                      setSeed(null)
                      setEditorOpen(true)
                    }}
                    onDelete={() => removeWidget(w.id)}
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
                        onEdit={() => {
                          setEditing(w)
                          setSeed(null)
                          setEditorOpen(true)
                        }}
                        onDelete={() => removeWidget(w.id)}
                      />
                    </div>
                  ))}
                </GridLayout>
              ) : null}
            </div>
          </>
        )}
      </div>

      <WidgetEditor
        open={editorOpen}
        onOpenChange={setEditorOpen}
        initial={editing}
        seed={seed}
        nlEnabled={nlWidgetsEnabled()}
        onSave={(draft) =>
          editing ? editWidget(editing.id, draft) : addWidget(draft)
        }
      />
    </div>
  )
}
