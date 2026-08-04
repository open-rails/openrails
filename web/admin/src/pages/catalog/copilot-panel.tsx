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
import * as React from "react"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Card, CardContent } from "@/components/ui/card"
import { Input } from "@/components/ui/input"
import {
  askCatalogCopilot,
  confirmCopilotDraft,
  type CatalogDiffDraft,
  type CopilotAskResponse,
  type CopilotDraft,
  type PriceChangeDraft,
} from "@/lib/api/copilot"
import { ApiError } from "@/lib/api/client"
import { createPrice, getPriceByKey, getProduct } from "@/lib/api/endpoints"
import type { CatalogPrice } from "@/lib/api/types"
import { formatMicros } from "@/lib/format"
import { toastApiError } from "@/lib/toast"
import { PriceChangeWizard } from "@/pages/catalog/price-wizard"

export function CatalogCopilotPanel({
  enabled,
  draftingEnabled,
  onCatalogChanged,
}: {
  enabled: boolean
  draftingEnabled: boolean
  onCatalogChanged: () => void
}) {
  const [question, setQuestion] = React.useState("")
  const [asking, setAsking] = React.useState(false)
  const [error, setError] = React.useState<string | null>(null)
  const [result, setResult] = React.useState<CopilotAskResponse | null>(null)

  const ask = async () => {
    const q = question.trim()
    if (!q || asking) return
    setAsking(true)
    setError(null)
    setResult(null) // one-shot: a new question replaces the previous answer
    try {
      setResult(await askCatalogCopilot(q))
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "ask failed")
    } finally {
      setAsking(false)
    }
  }

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
            placeholder={
              draftingEnabled
                ? 'Ask about your catalog, or ask for a change — e.g. "who is still on the old premium price?" or "raise premium to $12"'
                : 'Ask about your catalog — e.g. "what do we sell?" or "who is still on the old premium price?"'
            }
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
                  setResult(null)
                  setError(null)
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
              <DraftCard
                key={i}
                draft={draft}
                onCatalogChanged={onCatalogChanged}
              />
            ))}
          </div>
        ) : null}
      </CardContent>
    </Card>
  )
}

function DraftCard({
  draft,
  onCatalogChanged,
}: {
  draft: CopilotDraft
  onCatalogChanged: () => void
}) {
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
    return (
      <PriceChangeDraftCard
        draft={draft.price_change}
        onCatalogChanged={onCatalogChanged}
      />
    )
  }
  if (draft.kind === "catalog_diff" && draft.catalog_diff) {
    return (
      <CatalogDiffDraftCard
        draft={draft.catalog_diff}
        onCatalogChanged={onCatalogChanged}
      />
    )
  }
  return null
}

// PriceChangeDraftCard: "Review in wizard" fetches the live CatalogPrice +
// product name, then opens the SAME #777 wizard used everywhere else,
// pre-filled at Step 3 — the human reviews and clicks Confirm exactly as
// they would for a hand-typed change.
function PriceChangeDraftCard({
  draft,
  onCatalogChanged,
}: {
  draft: PriceChangeDraft
  onCatalogChanged: () => void
}) {
  const [reviewing, setReviewing] = React.useState(false)
  const [price, setPrice] = React.useState<CatalogPrice | null>(null)
  const [productName, setProductName] = React.useState("")
  const [loading, setLoading] = React.useState(false)

  const openWizard = async () => {
    setLoading(true)
    try {
      const p = await getPriceByKey(draft.price_key)
      const product = await getProduct(p.product_id)
      setPrice(p)
      setProductName(product.display_name)
      setReviewing(true)
    } catch (err) {
      toastApiError(err, "Load draft for review")
    } finally {
      setLoading(false)
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
      {reviewing && price ? (
        <PriceChangeWizard
          price={price}
          productName={productName}
          draft={draft}
          onDone={() => {
            setReviewing(false)
            onCatalogChanged()
          }}
        />
      ) : (
        <Button
          size="sm"
          className="mt-2"
          disabled={loading}
          onClick={openWizard}
        >
          {loading ? "Loading…" : "Review in wizard"}
        </Button>
      )}
    </div>
  )
}

// CatalogDiffDraftCard: "Create this price" calls the SAME createPrice the
// New Price form uses — a plain, explicit human confirm, never triggered by
// the tool layer itself.
function CatalogDiffDraftCard({
  draft,
  onCatalogChanged,
}: {
  draft: CatalogDiffDraft
  onCatalogChanged: () => void
}) {
  const [busy, setBusy] = React.useState(false)
  const [created, setCreated] = React.useState(false)

  const confirm = async () => {
    setBusy(true)
    try {
      await createPrice(draft.create_price)
      confirmCopilotDraft(
        draft.draft_id,
        "catalog_diff",
        draft.create_price.key
      ).catch(() => {})
      setCreated(true)
      onCatalogChanged()
    } catch (err) {
      toastApiError(err, "Create drafted price")
    } finally {
      setBusy(false)
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
        disabled={busy || created}
        onClick={confirm}
      >
        {created ? "Created" : busy ? "Creating…" : "Create this price"}
      </Button>
    </div>
  )
}
