// Add/edit widget dialog (#755, NL-first): a natural-language prompt is THE
// way to create and refine widget queries — the LLM writes the query, the
// dialog previews it live through the real /metrics/query endpoint, and the
// only human knobs are the viz-type toggle and the title. Editing an existing
// widget seeds the current query and sends it as base_query so instructions
// like "make it weekly" refine instead of starting over; direct title/viz
// edits never touch the LLM. With no LLM configured, creation shows a pointed
// empty-state (existing widgets stay directly editable).
import * as React from "react"
import { Loader2Icon, SparklesIcon } from "lucide-react"

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
import {
  generateWidget,
  metricsQuery,
  WIDGET_VIZ,
  type MetricsQuery,
  type MetricsResult,
  type Widget,
  type WidgetViz,
} from "@/lib/api/metrics"

import { WidgetVizView } from "./widget-viz"

const KEYLESS_MESSAGE =
  "Widget creation needs an LLM key: set llm.api_key (env LLM_API_KEY) — see docs/admin-console.md"

export function WidgetEditor({
  open,
  onOpenChange,
  initial,
  nlEnabled,
  onSave,
}: {
  open: boolean
  onOpenChange: (open: boolean) => void
  initial: Widget | null
  nlEnabled: boolean
  onSave: (data: { title: string; viz: WidgetViz; query: MetricsQuery }) => void
}) {
  const [title, setTitle] = React.useState("")
  const [viz, setViz] = React.useState<WidgetViz>("line")
  const [query, setQuery] = React.useState<MetricsQuery | null>(null)
  const [prompt, setPrompt] = React.useState("")
  const [generating, setGenerating] = React.useState(false)
  const [genError, setGenError] = React.useState<string | null>(null)
  const [preview, setPreview] = React.useState<MetricsResult | null>(null)
  const [previewError, setPreviewError] = React.useState<string | null>(null)
  const [previewing, setPreviewing] = React.useState(false)

  const runPreview = React.useCallback(async (q: MetricsQuery) => {
    setPreviewing(true)
    setPreviewError(null)
    try {
      setPreview(await metricsQuery(q))
    } catch (err) {
      setPreview(null)
      setPreviewError(err instanceof ApiError ? err.message : "query failed")
    } finally {
      setPreviewing(false)
    }
  }, [])

  // Reset per open; editing seeds the widget's current state and previews it.
  React.useEffect(() => {
    if (!open) return
    // eslint-disable-next-line react-hooks/set-state-in-effect -- deliberate: reset the dialog's draft state each time it opens
    setPrompt("")
    setGenError(null)
    setPreview(null)
    setPreviewError(null)
    if (initial) {
      setTitle(initial.title)
      setViz(initial.viz)
      setQuery(initial.query)
      void runPreview(initial.query)
    } else {
      setTitle("")
      setViz("line")
      setQuery(null)
    }
  }, [open, initial, runPreview])

  const generate = async () => {
    if (!prompt.trim()) return
    setGenerating(true)
    setGenError(null)
    try {
      // The current query (saved or freshly generated) rides as base_query so
      // the prompt refines it; absent, the LLM starts fresh.
      const res = await generateWidget(prompt.trim(), query ?? undefined)
      setTitle(res.title)
      setViz(res.viz)
      setQuery(res.query)
      setPrompt("")
      void runPreview(res.query)
    } catch (err) {
      setGenError(err instanceof ApiError ? err.message : "generation failed")
    } finally {
      setGenerating(false)
    }
  }

  const canSave = query !== null && title.trim().length > 0
  const keylessCreate = !nlEnabled && !initial

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="flex max-h-[90vh] flex-col overflow-hidden sm:max-w-2xl">
        <DialogHeader>
          <DialogTitle>{initial ? "Edit widget" : "Add widget"}</DialogTitle>
          <DialogDescription>
            {keylessCreate
              ? "Natural-language widget creation is not available on this deployment."
              : initial
                ? "Refine the widget with an instruction, or edit the title and viz directly — preview before saving."
                : "Describe what the widget should show — the query is generated, previewed live, then saved."}
          </DialogDescription>
        </DialogHeader>

        {keylessCreate ? (
          <div className="text-muted-foreground flex min-h-32 items-center justify-center rounded-lg border border-dashed p-6 text-center text-sm">
            {KEYLESS_MESSAGE}
          </div>
        ) : (
          <div className="flex min-h-0 flex-1 flex-col gap-4 overflow-y-auto">
            {nlEnabled ? (
              <div className="flex flex-col gap-2">
                <Textarea
                  placeholder={
                    query
                      ? 'Refine it — e.g. "make it weekly" or "split by rail"'
                      : 'e.g. "count of users who cancelled per day, for the past 7 days"'
                  }
                  value={prompt}
                  onChange={(e) => setPrompt(e.target.value)}
                  rows={3}
                />
                <div className="flex items-center gap-2">
                  <Button size="sm" onClick={generate} disabled={generating || !prompt.trim()}>
                    {generating ? (
                      <Loader2Icon className="size-3.5 animate-spin" />
                    ) : (
                      <SparklesIcon className="size-3.5" />
                    )}
                    {query ? "Refine" : "Generate"}
                  </Button>
                  <span className="text-muted-foreground text-xs">
                    {query
                      ? "The instruction edits the current query — the preview updates."
                      : "The generated widget previews below — adjust title and viz before saving."}
                  </span>
                </div>
                {genError ? <p className="text-destructive text-xs whitespace-pre-wrap">{genError}</p> : null}
              </div>
            ) : (
              <p className="text-muted-foreground rounded-md border border-dashed p-2 text-xs">
                {KEYLESS_MESSAGE}. Title and viz stay editable below.
              </p>
            )}

            {query ? (
              <>
                <div className="grid grid-cols-[1fr_auto] gap-2">
                  <div className="flex flex-col gap-1">
                    <Label htmlFor="widget-title">Title</Label>
                    <Input
                      id="widget-title"
                      value={title}
                      onChange={(e) => setTitle(e.target.value)}
                      placeholder="Widget title"
                    />
                  </div>
                  <div className="flex flex-col gap-1">
                    <Label>Viz</Label>
                    <Select value={viz} onValueChange={(v) => setViz(v as WidgetViz)}>
                      <SelectTrigger className="w-28">
                        <SelectValue />
                      </SelectTrigger>
                      <SelectContent>
                        {WIDGET_VIZ.map((v) => (
                          <SelectItem key={v} value={v}>
                            {v}
                          </SelectItem>
                        ))}
                      </SelectContent>
                    </Select>
                  </div>
                </div>

                <div className="bg-muted/30 flex min-h-56 flex-1 flex-col rounded-lg border p-3">
                  <div className="mb-2 flex items-center justify-between">
                    <span className="text-muted-foreground text-xs font-medium uppercase">Preview</span>
                    <Button
                      size="sm"
                      variant="outline"
                      onClick={() => void runPreview(query)}
                      disabled={previewing}
                    >
                      {previewing ? <Loader2Icon className="size-3.5 animate-spin" /> : null}
                      Refresh
                    </Button>
                  </div>
                  <div className="min-h-0 flex-1">
                    {previewError ? (
                      <p className="text-destructive text-xs whitespace-pre-wrap">{previewError}</p>
                    ) : preview ? (
                      <WidgetVizView viz={viz} result={preview} />
                    ) : (
                      <p className="text-muted-foreground flex h-full items-center justify-center text-xs">
                        {previewing ? "Running…" : "No preview yet"}
                      </p>
                    )}
                  </div>
                </div>

                <details className="text-muted-foreground text-xs">
                  <summary className="cursor-pointer select-none">Query JSON</summary>
                  <pre className="bg-muted/30 mt-1 overflow-x-auto rounded-md border p-2">
                    {JSON.stringify(query, null, 2)}
                  </pre>
                </details>
              </>
            ) : nlEnabled ? (
              <div className="text-muted-foreground flex min-h-32 items-center justify-center rounded-lg border border-dashed text-xs">
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
