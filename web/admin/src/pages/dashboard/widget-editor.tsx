// Add/edit widget dialog: natural-language prompt (only when the deployment
// has an LLM configured — fail-closed) and a schema-driven manual builder.
// Both paths end in the same place: a live PREVIEW through the real
// /metrics/query endpoint, then save. The server validates again on PUT.
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
import { Badge } from "@/components/ui/badge"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { Switch } from "@/components/ui/switch"
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { Textarea } from "@/components/ui/textarea"
import { useApiData } from "@/hooks/use-api-data"
import { ApiError } from "@/lib/api/client"
import {
  generateWidget,
  metricsQuery,
  metricsSchema,
  WIDGET_VIZ,
  type MetricsQuery,
  type MetricsResult,
  type MetricsSchema,
  type Widget,
  type WidgetViz,
} from "@/lib/api/metrics"
import { cn } from "@/lib/utils"

import { RANGE_PRESETS } from "./lib"
import { WidgetVizView } from "./widget-viz"

interface FilterRow {
  dim: string
  values: string
}

interface BuilderState {
  measures: string[]
  by: string[] // non-time dims
  time: boolean
  grain: string
  rangePreset: string // one of RANGE_PRESETS values or "custom"
  from: string
  to: string
  filters: FilterRow[]
  compare: boolean
  // Pass-through for query knobs the builder doesn't surface (order/limit
  // from generated queries) so editing never silently drops them.
  order?: MetricsQuery["order"]
  limit?: number
}

function stateFromQuery(q: MetricsQuery): BuilderState {
  const by = q.by ?? []
  return {
    measures: [...(q.measures ?? [])],
    by: by.filter((d) => d !== "time"),
    time: by.includes("time"),
    grain: q.grain || "day",
    rangePreset: q.range?.last && RANGE_PRESETS.some((p) => p.value === q.range.last) ? q.range.last : q.range?.last ? "custom-last" : "custom",
    from: q.range?.from ?? "",
    to: q.range?.to ?? "",
    filters: Object.entries(q.filters ?? {}).map(([dim, values]) => ({ dim, values: values.join(", ") })),
    compare: q.compare === "previous_period",
    order: q.order,
    limit: q.limit,
  }
}

function queryFromState(s: BuilderState, rawLast?: string): MetricsQuery {
  const q: MetricsQuery = {
    measures: s.measures,
    range:
      s.rangePreset === "custom"
        ? { from: s.from, to: s.to }
        : { last: s.rangePreset === "custom-last" ? rawLast || "30d" : s.rangePreset },
  }
  const by = [...(s.time ? ["time"] : []), ...s.by]
  if (by.length) q.by = by
  if (s.time) q.grain = s.grain
  const filters: Record<string, string[]> = {}
  for (const f of s.filters) {
    const values = f.values
      .split(",")
      .map((v) => v.trim())
      .filter(Boolean)
    if (f.dim && values.length) filters[f.dim] = values
  }
  if (Object.keys(filters).length) q.filters = filters
  if (s.compare) q.compare = "previous_period"
  if (s.order?.length) q.order = s.order
  if (s.limit) q.limit = s.limit
  return q
}

const defaultState: BuilderState = {
  measures: [],
  by: [],
  time: true,
  grain: "day",
  rangePreset: "30d",
  from: "",
  to: "",
  filters: [],
  compare: false,
}

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
  const { data: schema } = useApiData(metricsSchema, [])

  const [title, setTitle] = React.useState("")
  const [viz, setViz] = React.useState<WidgetViz>("line")
  const [builder, setBuilder] = React.useState<BuilderState>(defaultState)
  const [rawLast, setRawLast] = React.useState<string | undefined>(undefined)
  const [prompt, setPrompt] = React.useState("")
  const [generating, setGenerating] = React.useState(false)
  const [genError, setGenError] = React.useState<string | null>(null)
  const [preview, setPreview] = React.useState<MetricsResult | null>(null)
  const [previewError, setPreviewError] = React.useState<string | null>(null)
  const [previewing, setPreviewing] = React.useState(false)

  // Reset per open.
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
      setBuilder(stateFromQuery(initial.query))
      setRawLast(initial.query.range?.last)
    } else {
      setTitle("")
      setViz("line")
      setBuilder(defaultState)
      setRawLast(undefined)
    }
  }, [open, initial])

  const query = React.useMemo(() => queryFromState(builder, rawLast), [builder, rawLast])
  const canPreview = builder.measures.length > 0
  const canSave = canPreview && title.trim().length > 0

  const runPreview = async (q: MetricsQuery) => {
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
  }

  const generate = async () => {
    if (!prompt.trim()) return
    setGenerating(true)
    setGenError(null)
    try {
      const res = await generateWidget(prompt.trim())
      setTitle(res.title)
      setViz(res.viz)
      setBuilder(stateFromQuery(res.query))
      setRawLast(res.query.range?.last)
      void runPreview(res.query)
    } catch (err) {
      setGenError(err instanceof ApiError ? err.message : "generation failed")
    } finally {
      setGenerating(false)
    }
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="flex max-h-[90vh] flex-col overflow-hidden sm:max-w-4xl">
        <DialogHeader>
          <DialogTitle>{initial ? "Edit widget" : "Add widget"}</DialogTitle>
          <DialogDescription>
            {nlEnabled
              ? "Describe the widget in plain language, or build the query by hand — preview before saving."
              : "Pick measures, dimensions and a range from the metrics schema — preview before saving."}
          </DialogDescription>
        </DialogHeader>

        <div className="grid min-h-0 flex-1 gap-4 overflow-y-auto md:grid-cols-2">
          <div className="flex flex-col gap-4">
            <Tabs defaultValue={nlEnabled && !initial ? "nl" : "manual"}>
              {nlEnabled ? (
                <TabsList>
                  <TabsTrigger value="nl">
                    <SparklesIcon className="size-3.5" /> Describe it
                  </TabsTrigger>
                  <TabsTrigger value="manual">Build manually</TabsTrigger>
                </TabsList>
              ) : null}
              {nlEnabled ? (
                <TabsContent value="nl" className="flex flex-col gap-2 pt-2">
                  <Textarea
                    placeholder='e.g. "count of users who cancelled per day, for the past 7 days"'
                    value={prompt}
                    onChange={(e) => setPrompt(e.target.value)}
                    rows={3}
                  />
                  <div className="flex items-center gap-2">
                    <Button size="sm" onClick={generate} disabled={generating || !prompt.trim()}>
                      {generating ? <Loader2Icon className="size-3.5 animate-spin" /> : <SparklesIcon className="size-3.5" />}
                      Generate
                    </Button>
                    <span className="text-muted-foreground text-xs">
                      The result lands in the builder below — tweak it before saving.
                    </span>
                  </div>
                  {genError ? <p className="text-destructive text-xs">{genError}</p> : null}
                </TabsContent>
              ) : null}
              <TabsContent value="manual" className="pt-2">
                {schema ? (
                  <BuilderForm schema={schema} state={builder} onChange={setBuilder} />
                ) : (
                  <p className="text-muted-foreground text-sm">Loading schema…</p>
                )}
              </TabsContent>
            </Tabs>
          </div>

          <div className="flex flex-col gap-3">
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
                  disabled={!canPreview || previewing}
                >
                  {previewing ? <Loader2Icon className="size-3.5 animate-spin" /> : null}
                  Run preview
                </Button>
              </div>
              <div className="min-h-0 flex-1">
                {previewError ? (
                  <p className="text-destructive text-xs whitespace-pre-wrap">{previewError}</p>
                ) : preview ? (
                  <WidgetVizView viz={viz} result={preview} />
                ) : (
                  <p className="text-muted-foreground flex h-full items-center justify-center text-xs">
                    {canPreview ? "Run preview to see live data" : "Pick at least one measure"}
                  </p>
                )}
              </div>
            </div>
          </div>
        </div>

        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)}>
            Cancel
          </Button>
          <Button
            onClick={() => {
              onSave({ title: title.trim(), viz, query })
              onOpenChange(false)
            }}
            disabled={!canSave}
          >
            {initial ? "Save changes" : "Add widget"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

// --- schema-driven manual builder ------------------------------------------------

function TogglePill({
  active,
  onClick,
  children,
  title,
}: {
  active: boolean
  onClick: () => void
  children: React.ReactNode
  title?: string
}) {
  return (
    <button type="button" onClick={onClick} title={title}>
      <Badge variant={active ? "default" : "outline"} className={cn("cursor-pointer select-none")}>
        {children}
      </Badge>
    </button>
  )
}

function BuilderForm({
  schema,
  state,
  onChange,
}: {
  schema: MetricsSchema
  state: BuilderState
  onChange: (s: BuilderState) => void
}) {
  const set = (patch: Partial<BuilderState>) => onChange({ ...state, ...patch })

  // Dims the currently selected measures all support (plus time).
  const allowedDims = React.useMemo(() => {
    const selected = schema.measures.filter((m) => state.measures.includes(m.name))
    if (selected.length === 0) return schema.dimensions.map((d) => d.name).filter((d) => d !== "time")
    const sets = selected.map((m) => new Set(m.dims))
    return schema.dimensions
      .map((d) => d.name)
      .filter((d) => d !== "time" && sets.every((s) => s.has(d)))
  }, [schema, state.measures])

  const toggleMeasure = (name: string) =>
    set({
      measures: state.measures.includes(name)
        ? state.measures.filter((m) => m !== name)
        : [...state.measures, name],
    })

  const toggleDim = (name: string) =>
    set({ by: state.by.includes(name) ? state.by.filter((d) => d !== name) : [...state.by, name] })

  return (
    <div className="flex flex-col gap-3">
      <div>
        <Label className="mb-1 block">Measures</Label>
        <div className="flex max-h-36 flex-wrap gap-1 overflow-y-auto rounded-md border p-2">
          {schema.measures.map((m) => (
            <TogglePill
              key={m.name}
              active={state.measures.includes(m.name)}
              onClick={() => toggleMeasure(m.name)}
              title={m.description}
            >
              {m.name}
            </TogglePill>
          ))}
        </div>
      </div>

      <div>
        <Label className="mb-1 block">Group by</Label>
        <div className="flex max-h-28 flex-wrap gap-1 overflow-y-auto rounded-md border p-2">
          <TogglePill active={state.time} onClick={() => set({ time: !state.time })} title="bucket by time">
            time
          </TogglePill>
          {allowedDims.map((d) => (
            <TogglePill key={d} active={state.by.includes(d)} onClick={() => toggleDim(d)}>
              {d}
            </TogglePill>
          ))}
        </div>
      </div>

      <div className="grid grid-cols-2 gap-2">
        {state.time ? (
          <div className="flex flex-col gap-1">
            <Label>Grain</Label>
            <Select value={state.grain} onValueChange={(v) => set({ grain: v })}>
              <SelectTrigger>
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {schema.grains.map((g) => (
                  <SelectItem key={g} value={g}>
                    {g}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
        ) : null}
        <div className="flex flex-col gap-1">
          <Label>Range</Label>
          <Select value={state.rangePreset} onValueChange={(v) => set({ rangePreset: v })}>
            <SelectTrigger>
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              {RANGE_PRESETS.map((p) => (
                <SelectItem key={p.value} value={p.value}>
                  {p.label}
                </SelectItem>
              ))}
              <SelectItem value="custom">Custom dates</SelectItem>
            </SelectContent>
          </Select>
        </div>
      </div>

      {state.rangePreset === "custom" ? (
        <div className="grid grid-cols-2 gap-2">
          <div className="flex flex-col gap-1">
            <Label htmlFor="range-from">From</Label>
            <Input
              id="range-from"
              type="date"
              value={state.from}
              onChange={(e) => set({ from: e.target.value })}
            />
          </div>
          <div className="flex flex-col gap-1">
            <Label htmlFor="range-to">To</Label>
            <Input id="range-to" type="date" value={state.to} onChange={(e) => set({ to: e.target.value })} />
          </div>
        </div>
      ) : null}

      <div>
        <div className="mb-1 flex items-center justify-between">
          <Label>Filters</Label>
          <Button
            size="sm"
            variant="ghost"
            className="h-6 px-2 text-xs"
            onClick={() => set({ filters: [...state.filters, { dim: "", values: "" }] })}
          >
            + add filter
          </Button>
        </div>
        {state.filters.map((f, i) => {
          const dimDef = schema.dimensions.find((d) => d.name === f.dim)
          return (
            <div key={i} className="mb-1 grid grid-cols-[10rem_1fr_auto] items-center gap-1">
              <Select
                value={f.dim || undefined}
                onValueChange={(v) => {
                  const filters = [...state.filters]
                  filters[i] = { ...filters[i], dim: v }
                  set({ filters })
                }}
              >
                <SelectTrigger className="h-8">
                  <SelectValue placeholder="dimension" />
                </SelectTrigger>
                <SelectContent>
                  {allowedDims.map((d) => (
                    <SelectItem key={d} value={d}>
                      {d}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
              <Input
                className="h-8"
                placeholder={dimDef?.values ? dimDef.values.join(" | ") : "comma-separated values"}
                value={f.values}
                onChange={(e) => {
                  const filters = [...state.filters]
                  filters[i] = { ...filters[i], values: e.target.value }
                  set({ filters })
                }}
              />
              <Button
                size="sm"
                variant="ghost"
                className="h-8 px-2 text-xs"
                onClick={() => set({ filters: state.filters.filter((_, j) => j !== i) })}
              >
                ×
              </Button>
            </div>
          )
        })}
      </div>

      <div className="flex items-center gap-2">
        <Switch id="compare" checked={state.compare} onCheckedChange={(v) => set({ compare: v })} />
        <Label htmlFor="compare" className="text-sm">
          Compare with previous period
        </Label>
      </div>
    </div>
  )
}
