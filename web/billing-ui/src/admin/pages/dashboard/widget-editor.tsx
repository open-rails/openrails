import { AdminMessage, useAdminUI } from "#orck/admin/ui-context"
import { useAdminServices } from "#orck/admin/context"
// Edit an existing widget's presentation. Its metrics query stays unchanged.
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
  WIDGET_VIZ,
  type Widget,
  type WidgetViz,
} from "#orck/admin/lib/api/metrics"

import { cn } from "#orck/admin/lib/utils"
import { ownRange } from "./lib"
import { WidgetVizView } from "./widget-viz"

const VIZ_LABELS: Record<WidgetViz, string> = {
  stat: "Single number",
  line: "Line chart",
  area: "Area chart",
  bar: "Bar chart",
  donut: "Donut chart",
  table: "Table",
}

export type WidgetDraft = Pick<Widget, "title" | "viz" | "fixed_range">

export function WidgetEditor({
  onOpenChange,
  initial,
  onSave,
}: {
  onOpenChange: (open: boolean) => void
  initial: Widget
  onSave: (data: WidgetDraft) => void
}) {
  const { id: adminId, text: adminText } = useAdminUI()

  const { adminQueries } = useAdminServices()

  const [title, setTitle] = React.useState(initial.title)
  const [fixedRange, setFixedRange] = React.useState(
    initial.fixed_range ?? false
  )
  const [viz, setViz] = React.useState<WidgetViz>(initial.viz)
  const { query } = initial
  const {
    data: preview,
    error: previewQueryError,
    isFetching: previewing,
    refetch: refreshPreview,
  } = useQuery(adminQueries.widgetMetrics(query))
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
            <AdminMessage id={"Edit widget"} />
          </DialogTitle>
          <DialogDescription>
            <AdminMessage
              id={
                "Change the title, chart type or date range behavior. The query stays the same."
              }
            />
          </DialogDescription>
        </DialogHeader>
        <div className="flex min-h-0 flex-1 flex-col gap-4 overflow-y-auto">
          <div className="grid gap-2 sm:grid-cols-[1fr_12rem]">
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
            <div className="flex flex-col gap-1.5">
              <Label htmlFor={adminId("widget-viz")}>
                <AdminMessage id={"Chart type"} />
              </Label>
              <Select
                items={WIDGET_VIZ.map((value) => ({
                  value,
                  label: VIZ_LABELS[value],
                }))}
                value={viz}
                onValueChange={(v) => setViz(v as WidgetViz)}
              >
                <SelectTrigger id={adminId("widget-viz")} className="w-full">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {WIDGET_VIZ.map((v) => (
                    <SelectItem key={v} value={v}>
                      {VIZ_LABELS[v]}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
          </div>
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
                <WidgetVizView viz={viz} result={preview} query={query} />
              ) : (
                <p className="flex h-full items-center justify-center text-xs text-muted-foreground">
                  {previewing ? "Running…" : "No preview yet"}
                </p>
              )}
            </div>
          </div>
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)}>
            <AdminMessage id={"Cancel"} />
          </Button>
          <Button
            onClick={() => {
              onSave({ title: title.trim(), viz, fixed_range: fixedRange })
              onOpenChange(false)
            }}
            disabled={!title.trim()}
          >
            <AdminMessage id={"Save changes"} />
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
