import { AdminMessage, useAdminUI } from "#orck/admin/ui-context"
import { useAdminServices } from "#orck/admin/context"
// Add or edit a widget: its title, chart type, range behaviour and query,
// built from the metrics registry so any measure can be charted. A heading
// and the dispute programs have no query.
import { HugeiconsIcon } from "@hugeicons/react"
import { Loading02Icon } from "@hugeicons/core-free-icons"
import * as React from "react"
import { useQuery } from "@tanstack/react-query"
import { Button } from "#orck/admin/components/ui/button"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "#orck/admin/components/ui/dialog"
import { Input } from "#orck/admin/components/ui/input"
import { Label } from "#orck/admin/components/ui/label"
import { Switch } from "#orck/admin/components/ui/switch"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "#orck/admin/components/ui/select"
import { ApiError } from "#orck/admin/lib/api/client"
import { DIALOG_WIDE } from "#orck/admin/lib/dialog-width"
import {
  CHART_VIZ,
  queryless,
  type Widget,
  type WidgetViz,
} from "#orck/admin/lib/api/metrics"

import { cn } from "#orck/admin/lib/utils"
import { ownRange } from "./lib"
import { draftOf, QueryBuilder, queryOf, type Draft } from "./query-builder"
import { sparkQuery, widgetQuery, type DashboardView } from "./range"
import type { Registry } from "./registry"
import { WidgetVizView } from "./widget-viz"

const VIZ_LABELS: Record<WidgetViz, string> = {
  kpi: "KPI tile (value, change, sparkline)",
  stat: "Single number",
  line: "Line chart",
  area: "Area chart",
  bar: "Bar chart",
  donut: "Donut chart",
  table: "Table",
  cohort: "Cohort heatmap",
  heading: "Section heading",
  programs: "Dispute programs",
}

// A tile is one figure: it takes no time grouping (a kpi draws its own
// sparkline) and compares with the period before.
const scalar = (viz: WidgetViz) => viz === "kpi" || viz === "stat"
const series = (viz: WidgetViz) =>
  viz === "line" || viz === "area" || viz === "bar"

export type WidgetDraft = Pick<
  Widget,
  "title" | "viz" | "fixed_range" | "query"
>

export function WidgetEditor({
  onOpenChange,
  initial,
  added = false,
  view,
  registry,
  onSave,
}: {
  onOpenChange: (open: boolean) => void
  initial: Widget
  // added: a new widget, not yet on the dashboard.
  added?: boolean
  view: DashboardView
  registry: Registry
  onSave: (data: WidgetDraft) => void
}) {
  const { id: adminId, text: adminText } = useAdminUI()

  const { adminQueries } = useAdminServices()

  // A queryless widget (a heading, the dispute programs) has a title only.
  const heading = queryless(initial.viz)
  const [title, setTitle] = React.useState(initial.title)
  const [fixedRange, setFixedRange] = React.useState(
    initial.fixed_range ?? false
  )
  const [viz, setViz] = React.useState<WidgetViz>(initial.viz)
  const [draft, setDraft] = React.useState<Draft>(() => draftOf(initial.query))
  const changeViz = (next: WidgetViz) => {
    setViz(next)
    // Tiles are one figure; charts of a series run over time.
    if (scalar(next)) setDraft((d) => ({ ...d, time: false }))
    if (series(next)) setDraft((d) => ({ ...d, time: true }))
  }
  const base = {
    ...initial.query,
    compare: scalar(viz)
      ? (initial.query.compare ?? "previous_period")
      : undefined,
  }
  const query = heading ? initial.query : queryOf(draft, base)
  const shown = widgetQuery(
    { ...initial, viz, query, fixed_range: fixedRange },
    view,
    registry.takesCurrency
  )
  const {
    data: preview,
    error: previewQueryError,
    isFetching: previewing,
    refetch: refreshPreview,
  } = useQuery({ ...adminQueries.widgetMetrics(shown), enabled: !heading })
  const spark = useQuery({
    ...adminQueries.widgetMetrics(sparkQuery(shown)),
    enabled: !heading && viz === "kpi",
  })
  const previewError = previewQueryError
    ? previewQueryError instanceof ApiError
      ? previewQueryError.message
      : "query failed"
    : null

  return (
    <Dialog open onOpenChange={onOpenChange}>
      <DialogContent
        className={cn(
          "flex max-h-[90vh] flex-col overflow-hidden",
          DIALOG_WIDE
        )}
      >
        <DialogHeader>
          <DialogTitle>
            {added ? (
              <AdminMessage id={"Add widget"} />
            ) : (
              <AdminMessage id={"Edit widget"} />
            )}
          </DialogTitle>
          <DialogDescription>
            <AdminMessage
              id={"Pick what it measures, how it is split and how it is drawn."}
            />
          </DialogDescription>
        </DialogHeader>
        <div className="flex min-h-0 flex-1 flex-col gap-4 overflow-y-auto">
          <div className="grid gap-2 sm:grid-cols-[1fr_16rem]">
            <div className="flex flex-col gap-1.5">
              <Label htmlFor={adminId("widget-title")}>
                <AdminMessage id={"Title"} />
              </Label>
              <Input
                id={adminId("widget-title")}
                value={title}
                onChange={(e) => setTitle(e.target.value)}
                placeholder={adminText("Widget title")}
              />
            </div>
            {heading ? null : (
              <div className="flex flex-col gap-1.5">
                <Label htmlFor={adminId("widget-viz")}>
                  <AdminMessage id={"Chart type"} />
                </Label>
                <Select
                  items={CHART_VIZ.map((value) => ({
                    value,
                    label: VIZ_LABELS[value],
                  }))}
                  value={viz}
                  onValueChange={(v) => v && changeViz(v as WidgetViz)}
                >
                  <SelectTrigger id={adminId("widget-viz")} className="w-full">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    {CHART_VIZ.map((v) => (
                      <SelectItem key={v} value={v}>
                        {VIZ_LABELS[v]}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </div>
            )}
          </div>
          {heading ? null : (
            <>
              <QueryBuilder
                draft={draft}
                onChange={setDraft}
                registry={registry}
              />
              <div className="flex items-start gap-3">
                <Switch
                  id={adminId("widget-fixed-range")}
                  checked={fixedRange}
                  onCheckedChange={setFixedRange}
                />
                <div className="grid gap-0.5">
                  <Label htmlFor={adminId("widget-fixed-range")}>
                    <AdminMessage id={"Keep its own range"} />
                  </Label>
                  <p className="text-xs text-muted-foreground">
                    {fixedRange
                      ? `Always shows ${ownRange(query.range)}, whatever range the dashboard shows.`
                      : "Follows the range picked on the dashboard."}
                  </p>
                </div>
              </div>
              <div className="flex min-h-56 flex-1 flex-col rounded-lg border bg-muted/30 p-3">
                <div className="mb-2 flex items-center justify-between">
                  <span className="text-xs font-medium text-muted-foreground uppercase">
                    <AdminMessage id={"Preview"} />
                  </span>
                  <Button
                    size="sm"
                    variant="outline"
                    onClick={() => void refreshPreview()}
                    disabled={previewing}
                  >
                    {previewing ? (
                      <HugeiconsIcon
                        icon={Loading02Icon}
                        className="size-3.5 animate-spin"
                      />
                    ) : null}{" "}
                    <AdminMessage id={"Refresh"} />
                  </Button>
                </div>
                <div className="min-h-0 flex-1">
                  {previewError ? (
                    <p
                      className="text-xs whitespace-pre-wrap text-destructive"
                      role="alert"
                    >
                      {previewError}
                    </p>
                  ) : preview ? (
                    <WidgetVizView
                      viz={viz}
                      result={preview}
                      query={shown}
                      spark={spark.data}
                      registry={registry}
                    />
                  ) : (
                    <p className="flex h-full items-center justify-center text-xs text-muted-foreground">
                      {previewing ? "Running…" : "No preview yet"}
                    </p>
                  )}
                </div>
              </div>
            </>
          )}
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)}>
            <AdminMessage id={"Cancel"} />
          </Button>
          <Button
            onClick={() => {
              onSave({
                title: title.trim(),
                viz,
                fixed_range: fixedRange,
                query,
              })
              onOpenChange(false)
            }}
            disabled={!title.trim() || (!heading && previewError !== null)}
          >
            {added ? (
              <AdminMessage id={"Add widget"} />
            ) : (
              <AdminMessage id={"Save changes"} />
            )}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
