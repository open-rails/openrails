// Add/edit widget dialog (#755, NL-first): a natural-language prompt is THE
// way to create and refine widget queries — the LLM writes the query, the
// dialog previews it live through the real /metrics/query endpoint, and the
// only human knobs are the viz-type toggle and the title. Editing an existing
// widget seeds the current query and sends it as base_query so instructions
// like "make it weekly" refine instead of starting over; direct title/viz
// edits never touch the LLM. With no LLM configured, creation shows a pointed
// empty-state (existing widgets stay directly editable).
import { HugeiconsIcon } from "@hugeicons/react"
import { Loading02Icon, SparklesIcon } from "@hugeicons/core-free-icons"
import * as React from "react"
import { useMutation, useQuery } from "@tanstack/react-query"
import { Button } from "@/components/ui/button"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { Textarea } from "@/components/ui/textarea"
import { ApiError } from "@/lib/api/client"
import { DIALOG_WIDE } from "@/lib/dialog-width"
import {
  WIDGET_VIZ,
  type MetricsQuery,
  type Widget,
  type WidgetViz,
} from "@/lib/api/metrics"
import { adminMutations } from "@/lib/mutations"
import { adminQueries } from "@/lib/queries"

import { WidgetVizView } from "./widget-viz"
import { cn } from "@/lib/utils"

// The reader here runs a merchant, not the deployment: config keys and doc paths
// are not theirs to act on. Say what is unavailable, who can change it, and what
// still works — the last part matters because editing existing widgets never
// touches the LLM.
// The wire values are chart-library names. These are what the shapes are called
// when you are choosing one.
const VIZ_LABELS: Record<WidgetViz, string> = {
  stat: "Single number",
  line: "Line chart",
  area: "Area chart",
  bar: "Bar chart",
  donut: "Donut chart",
  table: "Table",
}

function vizLabel(viz: WidgetViz): string {
  return VIZ_LABELS[viz] ?? viz
}

const KEYLESS_MESSAGE =
  "Ask whoever runs this deployment to turn it on. You can still rename existing widgets and change how they are shown."

export function WidgetEditor({
  onOpenChange,
  initial,
  seed,
  nlEnabled,
  onSave,
}: {
  onOpenChange: (open: boolean) => void
  initial: Widget | null
  // seed pre-fills a NEW widget draft (#756 ask evidence → add-as-widget):
  // the query previews immediately and the prompt refines it, but saving
  // still ADDS a widget (initial stays null).
  seed?: { title: string; viz: WidgetViz; query: MetricsQuery } | null
  nlEnabled: boolean
  onSave: (data: { title: string; viz: WidgetViz; query: MetricsQuery }) => void
}) {
  const source = initial ?? seed
  const [title, setTitle] = React.useState(source?.title ?? "")
  const [viz, setViz] = React.useState<WidgetViz>(source?.viz ?? "line")
  const [query, setQuery] = React.useState<MetricsQuery | null>(
    source?.query ?? null
  )
  const [prompt, setPrompt] = React.useState("")
  const generateDashboardWidget = useMutation(
    adminMutations.generateDashboardWidget()
  )
  const generating = generateDashboardWidget.isPending
  const genError = generateDashboardWidget.error
    ? generateDashboardWidget.error instanceof ApiError
      ? generateDashboardWidget.error.message
      : "generation failed"
    : null
  const {
    data: preview,
    error: previewQueryError,
    isFetching: previewing,
    refetch: refreshPreview,
  } = useQuery(adminQueries.widgetMetrics(query ?? undefined))
  const previewError = previewQueryError
    ? previewQueryError instanceof ApiError
      ? previewQueryError.message
      : "query failed"
    : null

  const generate = () => {
    if (!prompt.trim()) return
    // The current query (saved or freshly generated) rides as base_query so
    // the prompt refines it; absent, the LLM starts fresh.
    generateDashboardWidget.mutate(
      { prompt: prompt.trim(), baseQuery: query ?? undefined },
      {
        onSuccess: (res) => {
          setTitle(res.title)
          setViz(res.viz)
          setQuery(res.query)
          setPrompt("")
        },
      }
    )
  }

  const canSave = query !== null && title.trim().length > 0
  const keylessCreate = !nlEnabled && !initial

  return (
    <Dialog open onOpenChange={onOpenChange}>
      <DialogContent
        className={cn(
          "flex max-h-[90vh] flex-col overflow-hidden",
          DIALOG_WIDE
        )}
      >
        <DialogHeader>
          <DialogTitle>{initial ? "Edit widget" : "Add widget"}</DialogTitle>
          <DialogDescription>
            {keylessCreate
              ? "Adding widgets is not switched on here."
              : !initial
                ? "Describe what the widget should show. It is previewed before you save it."
                : nlEnabled
                  ? "Refine the widget with an instruction, or change the title and chart type yourself. You can preview before saving."
                  : "Change the title or the chart type. You can preview before saving."}
          </DialogDescription>
        </DialogHeader>

        {keylessCreate ? (
          <div className="flex min-h-32 items-center justify-center rounded-lg border border-dashed p-6 text-center text-sm text-muted-foreground">
            {KEYLESS_MESSAGE}
          </div>
        ) : (
          <div className="flex min-h-0 flex-1 flex-col gap-4 overflow-y-auto">
            {nlEnabled ? (
              <div className="flex flex-col gap-2">
                <Textarea
                  placeholder={
                    query
                      ? 'Refine it. For example "make it weekly" or "split by rail"'
                      : 'e.g. "count of users who cancelled per day, for the past 7 days"'
                  }
                  value={prompt}
                  onChange={(e) => setPrompt(e.target.value)}
                  rows={3}
                />
                <div className="flex items-center gap-2">
                  <Button
                    size="sm"
                    onClick={generate}
                    disabled={generating || !prompt.trim()}
                  >
                    {generating ? (
                      <HugeiconsIcon
                        icon={Loading02Icon}
                        className="size-3.5 animate-spin"
                      />
                    ) : (
                      <HugeiconsIcon icon={SparklesIcon} className="size-3.5" />
                    )}
                    {query ? "Refine" : "Generate"}
                  </Button>
                </div>
                {genError ? (
                  <p
                    className="text-xs whitespace-pre-wrap text-destructive"
                    role="alert"
                  >
                    {genError}
                  </p>
                ) : null}
              </div>
            ) : null}

            {query ? (
              <>
                <div className="grid gap-2 sm:grid-cols-[1fr_12rem]">
                  <div className="flex flex-col gap-1.5">
                    <Label htmlFor="widget-title">Title</Label>
                    <Input
                      id="widget-title"
                      value={title}
                      onChange={(e) => setTitle(e.target.value)}
                      placeholder="Widget title"
                    />
                  </div>
                  <div className="flex flex-col gap-1.5">
                    <Label htmlFor="widget-viz">Chart type</Label>
                    <Select
                      items={WIDGET_VIZ.map((v) => ({
                        value: v,
                        label: vizLabel(v),
                      }))}
                      value={viz}
                      onValueChange={(v) => setViz(v as WidgetViz)}
                    >
                      <SelectTrigger id="widget-viz" className="w-full">
                        <SelectValue />
                      </SelectTrigger>
                      <SelectContent>
                        {WIDGET_VIZ.map((v) => (
                          <SelectItem key={v} value={v}>
                            {vizLabel(v)}
                          </SelectItem>
                        ))}
                      </SelectContent>
                    </Select>
                  </div>
                </div>

                <div className="flex min-h-56 flex-1 flex-col rounded-lg border bg-muted/30 p-3">
                  <div className="mb-2 flex items-center justify-between">
                    <span className="text-xs font-medium text-muted-foreground uppercase">
                      Preview
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
                      ) : null}
                      Refresh
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
                      <WidgetVizView viz={viz} result={preview} />
                    ) : (
                      <p className="flex h-full items-center justify-center text-xs text-muted-foreground">
                        {previewing ? "Running…" : "No preview yet"}
                      </p>
                    )}
                  </div>
                </div>

                <details className="text-xs text-muted-foreground">
                  <summary className="cursor-pointer select-none">
                    What this widget asks for
                  </summary>
                  <pre className="mt-1 overflow-x-auto rounded-md border bg-muted/30 p-2">
                    {JSON.stringify(query, null, 2)}
                  </pre>
                </details>
              </>
            ) : nlEnabled ? (
              <div className="flex min-h-32 items-center justify-center rounded-lg border border-dashed text-xs text-muted-foreground">
                Describe the widget above to generate a preview.
              </div>
            ) : null}
          </div>
        )}

        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)}>
            Cancel
          </Button>
          {keylessCreate ? null : (
            <Button
              onClick={() => {
                if (!query) return
                onSave({ title: title.trim(), viz, query })
                onOpenChange(false)
              }}
              disabled={!canSave}
            >
              {initial ? "Save changes" : "Add widget"}
            </Button>
          )}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
