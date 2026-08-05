// Catalog copilot (#779): free-form Q&A over the catalog/pricing/reprice
// data, mirroring the dashboard's #756 Ask panel exactly (one-shot question,
// answer + evidence). The ONLY additional surface is drafts (Phase 2,
// flag-gated): the model never mutates anything — a draft only opens,
// pre-filled, in the normal #777 wizard (price change) or a plain
// create-price call (new tier) for a human to explicitly confirm.
import { HugeiconsIcon } from "@hugeicons/react"
import {
  BubbleChatQuestionIcon,
  Cancel01Icon,
  Loading02Icon,
  SparklesIcon,
} from "@hugeicons/core-free-icons"
import { useForm } from "@tanstack/react-form"
import { useMutation, useQueryClient } from "@tanstack/react-query"

import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Card, CardContent } from "@/components/ui/card"
import { Input } from "@/components/ui/input"
import type {
  CatalogDiffDraft,
  CopilotDraft,
  PriceChangeDraft,
} from "@/lib/api/copilot"
import { ApiError } from "@/lib/api/client"
import { formatMicros } from "@/lib/format"
import { adminMutations } from "@/lib/mutations"
import { toastApiError } from "@/lib/toast"
import { PriceChangeWizard } from "@/pages/catalog/price-wizard"

export function CatalogCopilotPanel({
  enabled,
  draftingEnabled,
}: {
  enabled: boolean
  draftingEnabled: boolean
}) {
  const askCatalog = useMutation(adminMutations.askCatalogCopilot())
  const form = useForm({
    defaultValues: { question: "" },
    onSubmit: async ({ value }) => {
      const question = value.question.trim()
      if (!question) return

      askCatalog.reset()
      try {
        await askCatalog.mutateAsync(question)
      } catch {
        // The request error renders inline below the form.
      }
    },
  })
  const result = askCatalog.data
  const error = askCatalog.error
    ? askCatalog.error instanceof ApiError
      ? askCatalog.error.message
      : "ask failed"
    : null

  // Enabling the copilot is an operator setup step (see docs/admin-console.md);
  // an unconfigured feature is not page furniture, so render nothing.
  if (!enabled) {
    return null
  }

  return (
    <Card>
      <CardContent className="flex flex-col gap-3">
        <form
          className="flex items-center gap-2"
          onSubmit={(event) => {
            event.preventDefault()
            event.stopPropagation()
            void form.handleSubmit()
          }}
        >
          <HugeiconsIcon
            icon={BubbleChatQuestionIcon}
            className="size-4 shrink-0 text-muted-foreground"
          />
          <form.Field name="question">
            {(field) => (
              <Input
                value={field.state.value}
                onBlur={field.handleBlur}
                onChange={(event) => field.handleChange(event.target.value)}
                placeholder={
                  draftingEnabled
                    ? 'Ask about your catalog, or ask for a change — e.g. "who is still on the old premium price?" or "raise premium to $12"'
                    : 'Ask about your catalog — e.g. "what do we sell?" or "who is still on the old premium price?"'
                }
                className="flex-1"
              />
            )}
          </form.Field>
          <form.Subscribe
            selector={(state) =>
              [state.values.question, state.isSubmitting] as const
            }
          >
            {([question, isSubmitting]) => (
              <Button
                type="submit"
                size="sm"
                disabled={isSubmitting || !question.trim()}
              >
                {isSubmitting ? (
                  <HugeiconsIcon
                    icon={Loading02Icon}
                    className="size-3.5 animate-spin"
                  />
                ) : null}
                Ask
              </Button>
            )}
          </form.Subscribe>
        </form>

        {askCatalog.isPending ? (
          <p className="text-xs text-muted-foreground">
            Checking your catalog…
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
                  askCatalog.reset()
                }}
              >
                <HugeiconsIcon icon={Cancel01Icon} className="size-3.5" />
              </Button>
            </div>

            {result.evidence.length > 0 && (
              <div className="flex flex-col gap-2">
                {result.evidence.map((ev, i) => (
                  <div key={i} className="rounded-lg border bg-muted/30 p-2">
                    <span className="mb-1 block text-xs font-medium text-muted-foreground uppercase">
                      {ev.tool}
                    </span>
                    <pre className="max-h-48 overflow-auto text-xs whitespace-pre-wrap">
                      {ev.summary}
                    </pre>
                  </div>
                ))}
              </div>
            )}

            {result.drafts?.map((draft, i) => (
              <DraftCard key={i} draft={draft} />
            ))}
          </div>
        ) : null}
      </CardContent>
    </Card>
  )
}

function DraftCard({ draft }: { draft: CopilotDraft }) {
  if (draft.kind === "refused" && draft.refusal) {
    return (
      <div className="rounded-lg border border-dashed p-3 text-xs">
        <p className="mb-1 flex items-center gap-2 font-medium">
          <Badge variant="secondary">refused: {draft.refusal.code}</Badge>
        </p>
        <p className="text-muted-foreground">{draft.refusal.reason}</p>
        <p className="mt-1">{draft.refusal.workaround}</p>
      </div>
    )
  }
  if (draft.kind === "price_change" && draft.price_change) {
    return <PriceChangeDraftCard draft={draft.price_change} />
  }
  if (draft.kind === "catalog_diff" && draft.catalog_diff) {
    return <CatalogDiffDraftCard draft={draft.catalog_diff} />
  }
  return null
}

// PriceChangeDraftCard: "Review in wizard" fetches the live CatalogPrice +
// product name, then opens the SAME #777 wizard used everywhere else,
// pre-filled at Step 3 — the human reviews and clicks Confirm exactly as
// they would for a hand-typed change.
function PriceChangeDraftCard({ draft }: { draft: PriceChangeDraft }) {
  const loadDraft = useMutation(adminMutations.loadCatalogPriceDraft())

  const openWizard = async () => {
    try {
      await loadDraft.mutateAsync(draft.price_key)
    } catch (err) {
      toastApiError(err, "Load draft for review")
    }
  }

  return (
    <div className="rounded-lg border bg-muted/30 p-3 text-sm">
      <div className="mb-1 flex items-center gap-2">
        <HugeiconsIcon
          icon={SparklesIcon}
          className="size-3.5 text-muted-foreground"
        />
        <span className="text-xs font-medium text-muted-foreground uppercase">
          Draft: price change
        </span>
        <Badge variant="secondary">{draft.direction}</Badge>
      </div>
      <p>{draft.review_text}</p>
      <p className="mt-1 text-xs text-muted-foreground">
        {formatMicros(draft.current_amount, draft.currency)} →{" "}
        {formatMicros(draft.new_amount, draft.currency)} ·{" "}
        {draft.affected_count.toLocaleString()} affected · requires human
        confirm
      </p>
      {loadDraft.data ? (
        <PriceChangeWizard
          price={loadDraft.data.price}
          productName={loadDraft.data.productName}
          draft={draft}
          onDone={() => loadDraft.reset()}
        />
      ) : (
        <Button
          size="sm"
          className="mt-2"
          disabled={loadDraft.isPending}
          onClick={openWizard}
        >
          {loadDraft.isPending ? "Loading…" : "Review in wizard"}
        </Button>
      )}
    </div>
  )
}

// CatalogDiffDraftCard: "Create this price" calls the SAME createPrice the
// New Price form uses — a plain, explicit human confirm, never triggered by
// the tool layer itself.
function CatalogDiffDraftCard({ draft }: { draft: CatalogDiffDraft }) {
  const queryClient = useQueryClient()
  const createDraftPrice = useMutation(
    adminMutations.createCatalogDraftPrice(queryClient)
  )

  const confirm = async () => {
    try {
      await createDraftPrice.mutateAsync({
        draftId: draft.draft_id,
        price: draft.create_price,
      })
    } catch (err) {
      toastApiError(err, "Create drafted price")
    }
  }

  return (
    <div className="rounded-lg border bg-muted/30 p-3 text-sm">
      <div className="mb-1 flex items-center gap-2">
        <HugeiconsIcon
          icon={SparklesIcon}
          className="size-3.5 text-muted-foreground"
        />
        <span className="text-xs font-medium text-muted-foreground uppercase">
          Draft: new price
        </span>
      </div>
      <p>{draft.review_text}</p>
      <p className="mt-1 text-xs text-muted-foreground">
        key: {draft.create_price.key} ·{" "}
        {formatMicros(
          draft.create_price.unit_amount,
          draft.create_price.currency
        )}
      </p>
      <Button
        size="sm"
        className="mt-2"
        disabled={createDraftPrice.isPending || createDraftPrice.isSuccess}
        onClick={confirm}
      >
        {createDraftPrice.isSuccess
          ? "Created"
          : createDraftPrice.isPending
            ? "Creating…"
            : "Create this price"}
      </Button>
    </div>
  )
}
