// Ask panel: one-shot metrics Q&A. The server's LLM runs compiler-validated
// queries; on-screen numbers come from each query's verbatim result, never
// from prose. The answer and evidence are read-only.
import { HugeiconsIcon } from "@hugeicons/react"
import {
  BubbleChatQuestionIcon,
  Cancel01Icon,
  Loading02Icon,
} from "@hugeicons/core-free-icons"
import * as React from "react"
import { useMutation } from "@tanstack/react-query"
import { Button } from "@/components/ui/button"
import { Card, CardContent } from "@/components/ui/card"
import { Input } from "@/components/ui/input"
import { ApiError } from "@/lib/api/client"
import { adminMutations } from "@/lib/mutations"

import { WidgetVizView } from "./widget-viz"

export function AskPanel({ enabled }: { enabled: boolean }) {
  const [question, setQuestion] = React.useState("")
  const askQuestion = useMutation(adminMutations.askMetrics())
  const asking = askQuestion.isPending
  const result = askQuestion.data
  const error = askQuestion.error
    ? askQuestion.error instanceof ApiError
      ? askQuestion.error.message
      : "ask failed"
    : null

  const ask = () => {
    const q = question.trim()
    if (!q || asking) return
    askQuestion.mutate(q)
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
            ask()
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
          <p
            className="text-xs whitespace-pre-wrap text-destructive"
            role="alert"
          >
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
                onClick={() => askQuestion.reset()}
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
