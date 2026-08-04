// Ask panel (#756): free-form Q&A over the metrics API. The server-side LLM
// runs compiler-validated queries as tools; the answer renders WITH the
// verbatim result of every executed query as evidence tables — numbers on
// screen come from the API responses, never from prose. One-shot: a new
// question replaces the previous answer. Each evidence query can be saved as
// a dashboard widget through the normal #755 preview/save flow.
import { HugeiconsIcon } from "@hugeicons/react"
import {
  Add01Icon,
  BubbleChatQuestionIcon,
  Cancel01Icon,
  Loading02Icon,
} from "@hugeicons/core-free-icons"
import * as React from "react"
import { Button } from "@/components/ui/button"
import { Card, CardContent } from "@/components/ui/card"
import { Input } from "@/components/ui/input"
import { ApiError } from "@/lib/api/client"
import {
  askMetrics,
  type AskEvidence,
  type AskResponse,
  type MetricsQuery,
  type WidgetViz,
} from "@/lib/api/metrics"

import { WidgetVizView } from "./widget-viz"

// suggestViz picks a sensible default viz for saving an evidence query as a
// widget (the editor lets the user override it).
function suggestViz(query: MetricsQuery): WidgetViz {
  if (query.by?.includes("time")) return "line"
  if ((query.by?.length ?? 0) > 0 || query.measures.length > 1) return "table"
  return "stat"
}

export function AskPanel({
  enabled,
  onAddWidget,
}: {
  enabled: boolean
  onAddWidget: (draft: {
    title: string
    viz: WidgetViz
    query: MetricsQuery
  }) => void
}) {
  const [question, setQuestion] = React.useState("")
  const [asking, setAsking] = React.useState(false)
  const [error, setError] = React.useState<string | null>(null)
  const [result, setResult] = React.useState<AskResponse | null>(null)
  const [asked, setAsked] = React.useState("")

  const ask = async () => {
    const q = question.trim()
    if (!q || asking) return
    setAsking(true)
    setError(null)
    setResult(null) // one-shot: a new question replaces the previous answer
    setAsked(q)
    try {
      setResult(await askMetrics(q))
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "ask failed")
    } finally {
      setAsking(false)
    }
  }

  const addAsWidget = (ev: AskEvidence) => {
    onAddWidget({
      title: asked.length > 60 ? asked.slice(0, 57) + "…" : asked,
      viz: suggestViz(ev.query),
      query: ev.query,
    })
  }

  // Enabling Ask is an operator setup step (llm.ask_enabled + an LLM key —
  // see docs/admin-console.md); an unconfigured feature is not dashboard
  // furniture, so render nothing.
  if (!enabled) {
    return null
  }

  return (
    <Card>
      <CardContent className="flex flex-col gap-3">
        <form
          className="flex items-center gap-2"
          onSubmit={(e) => {
            e.preventDefault()
            void ask()
          }}
        >
          <HugeiconsIcon
            icon={BubbleChatQuestionIcon}
            className="size-4 shrink-0 text-muted-foreground"
          />
          <Input
            value={question}
            onChange={(e) => setQuestion(e.target.value)}
            placeholder='Ask your metrics: "why did revenue dip last week?"'
            className="flex-1"
          />
          <Button type="submit" size="sm" disabled={asking || !question.trim()}>
            {asking ? (
              <HugeiconsIcon
                icon={Loading02Icon}
                className="size-3.5 animate-spin"
              />
            ) : null}
            Ask
          </Button>
        </form>

        {asking ? (
          <p className="text-xs text-muted-foreground">
            Querying your metrics…
          </p>
        ) : null}
        {error ? (
          <p className="text-xs whitespace-pre-wrap text-destructive">
            {error}
          </p>
        ) : null}

        {result ? (
          <div className="flex flex-col gap-3">
            <div className="flex items-start justify-between gap-2">
              <p className="text-sm whitespace-pre-wrap">{result.answer}</p>
              <Button
                variant="ghost"
                size="icon"
                className="size-6 shrink-0"
                aria-label="Dismiss answer"
                onClick={() => {
                  setResult(null)
                  setError(null)
                }}
              >
                <HugeiconsIcon icon={Cancel01Icon} className="size-3.5" />
              </Button>
            </div>
            {result.evidence.length > 0 ? (
              <div className="flex flex-col gap-2">
                {result.evidence.map((ev, i) => (
                  <div key={i} className="rounded-lg border bg-muted/30 p-2">
                    <div className="mb-1 flex items-center justify-between gap-2">
                      <span className="text-xs font-medium text-muted-foreground uppercase">
                        Query {i + 1} result
                      </span>
                      <Button
                        size="sm"
                        variant="outline"
                        onClick={() => addAsWidget(ev)}
                      >
                        <HugeiconsIcon icon={Add01Icon} className="size-3.5" />{" "}
                        Add as widget
                      </Button>
                    </div>
                    <div className="max-h-64 overflow-auto">
                      <WidgetVizView viz="table" result={ev} />
                    </div>
                    <details className="mt-1 text-xs text-muted-foreground">
                      <summary className="cursor-pointer select-none">
                        Query JSON
                      </summary>
                      <pre className="mt-1 overflow-x-auto rounded-md border bg-muted/30 p-2">
                        {JSON.stringify(ev.query, null, 2)}
                      </pre>
                    </details>
                  </div>
                ))}
              </div>
            ) : null}
          </div>
        ) : null}
      </CardContent>
    </Card>
  )
}
