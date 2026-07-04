// #741 configurable dashboard: a per-merchant widget grid over the metrics
// API. Tile registry + serialized Layout[] on react-grid-layout v2 — drag,
// resize, add, edit, remove; the layout persists via PUT (debounced). First
// load with no saved row shows the seeded default template.
import * as React from "react"
import { PlusIcon } from "lucide-react"
import { GridLayout, useContainerWidth, type Layout } from "react-grid-layout"
import "react-grid-layout/css/styles.css"

import { Button } from "@/components/ui/button"
import { Skeleton } from "@/components/ui/skeleton"
import { useApiData } from "@/hooks/use-api-data"
import { getBootstrap } from "@/lib/api/client"
import {
  getDashboard,
  putDashboard,
  type MetricsQuery,
  type Widget,
  type WidgetViz,
} from "@/lib/api/metrics"
import { toastApiError } from "@/lib/toast"

import { newWidgetId } from "./lib"
import { WidgetEditor } from "./widget-editor"
import { WidgetTile } from "./widget-tile"

const COLS = 12
const ROW_HEIGHT = 72

function nlWidgetsEnabled(): boolean {
  try {
    return getBootstrap().nl_widgets_enabled
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
  const { width, containerRef, mounted } = useContainerWidth()
  const saveTimer = React.useRef<ReturnType<typeof setTimeout> | null>(null)

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
    [],
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
    [persist],
  )

  const onLayoutChange = (layout: Layout) => {
    if (!widgets) return
    const byId = new Map(layout.map((l) => [l.i, l]))
    let changed = false
    const next = widgets.map((w) => {
      const l = byId.get(w.id)
      if (!l) return w
      if (l.x !== w.grid.x || l.y !== w.grid.y || l.w !== w.grid.w || l.h !== w.grid.h) {
        changed = true
        return { ...w, grid: { x: l.x, y: l.y, w: l.w, h: l.h } }
      }
      return w
    })
    if (changed) update(next)
  }

  const addWidget = (draft: { title: string; viz: WidgetViz; query: MetricsQuery }) => {
    if (!widgets) return
    const size = defaultSize(draft.viz)
    const y = widgets.reduce((max, w) => Math.max(max, w.grid.y + w.grid.h), 0)
    update([
      ...widgets,
      { id: newWidgetId(), title: draft.title, viz: draft.viz, query: draft.query, grid: { x: 0, y, ...size } },
    ])
  }

  const editWidget = (id: string, draft: { title: string; viz: WidgetViz; query: MetricsQuery }) => {
    if (!widgets) return
    update(widgets.map((w) => (w.id === id ? { ...w, title: draft.title, viz: draft.viz, query: draft.query } : w)))
  }

  const removeWidget = (id: string) => {
    if (!widgets) return
    update(widgets.filter((w) => w.id !== id))
  }

  if (loading && !widgets) {
    return (
      <div className="grid grid-cols-1 gap-4 md:grid-cols-4">
        {Array.from({ length: 4 }).map((_, i) => (
          <Skeleton key={i} className="h-36" />
        ))}
        <Skeleton className="h-72 md:col-span-2" />
        <Skeleton className="h-72 md:col-span-2" />
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
    <div className="flex flex-col gap-3">
      <div className="flex items-center gap-3">
        {data?.is_default ? (
          <p className="text-muted-foreground text-sm">
            Default template — rearrange, edit or add widgets and it saves as your own layout.
          </p>
        ) : (
          <p className="text-muted-foreground text-sm">Changes save automatically.</p>
        )}
        <Button
          size="sm"
          className="ml-auto"
          onClick={() => {
            setEditing(null)
            setEditorOpen(true)
          }}
        >
          <PlusIcon className="size-4" /> Add widget
        </Button>
      </div>

      <div ref={containerRef} className="min-h-0 w-full">
        {mounted && widgets.length > 0 ? (
          <GridLayout
            layout={layout}
            width={width}
            gridConfig={{ cols: COLS, rowHeight: ROW_HEIGHT, margin: [12, 12] }}
            dragConfig={{ handle: ".widget-drag-handle" }}
            onLayoutChange={onLayoutChange}
            className="-mx-3"
          >
            {widgets.map((w) => (
              <div key={w.id}>
                <WidgetTile
                  widget={w}
                  onEdit={() => {
                    setEditing(w)
                    setEditorOpen(true)
                  }}
                  onDelete={() => removeWidget(w.id)}
                />
              </div>
            ))}
          </GridLayout>
        ) : widgets.length === 0 ? (
          <div className="text-muted-foreground flex h-48 items-center justify-center rounded-xl border border-dashed text-sm">
            No widgets — add one to get started.
          </div>
        ) : null}
      </div>

      <WidgetEditor
        open={editorOpen}
        onOpenChange={setEditorOpen}
        initial={editing}
        nlEnabled={nlWidgetsEnabled()}
        onSave={(draft) => (editing ? editWidget(editing.id, draft) : addWidget(draft))}
      />
    </div>
  )
}
