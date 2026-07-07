// #779 catalog copilot: read-only Q&A (always, when configured) + flag-gated
// Phase 2 drafting. Types mirror internal/modules/copilot's wire shapes
// exactly. The model NEVER mutates — a draft is only ever opened, pre-filled,
// in the normal price-change wizard / new-price dialog for a human to confirm.
import { api } from "./client"

export interface CopilotEvidence {
  tool: string
  args: string
  summary: string
}

// CreatePriceDraft mirrors the #777 wizard/PriceDialog's create-price body —
// the SAME shape createPrice() in endpoints.ts already sends.
export interface CreatePriceDraft {
  product_id: string
  key: string
  unit_amount: number
  currency: string
  access_duration_hours?: number
  auto_renew: boolean
  trial_unit_amount?: number
  trial_duration_hours?: number
  providers?: string[]
}

export interface RepriceDraft {
  price_key: string
  effective_at: string
}

export interface PriceChangeDraft {
  draft_id: string
  drafted_by: string
  price_key: string
  current_amount: number
  new_amount: number
  currency: string
  direction: "increase" | "decrease" | "unchanged"
  migration_mode: "grandfather" | "migrate"
  affected_count: number
  review_text: string
  create_price: CreatePriceDraft
  reprice?: RepriceDraft
}

export interface CatalogDiffDraft {
  draft_id: string
  drafted_by: string
  product_key: string
  review_text: string
  create_price: CreatePriceDraft
}

export interface DraftRefusal {
  code: string
  reason: string
  workaround: string
}

export interface CopilotDraft {
  kind: "price_change" | "catalog_diff" | "refused"
  price_change?: PriceChangeDraft
  catalog_diff?: CatalogDiffDraft
  refusal?: DraftRefusal
}

export interface CopilotAskResponse {
  answer: string
  evidence: CopilotEvidence[]
  drafts?: CopilotDraft[]
}

// askCatalogCopilot: free-form catalog question -> LLM-run catalog/pricing
// lookups + an answer, plus any drafts the model proposed. 501 when
// llm.catalog_copilot_enabled / the LLM key are not configured.
export const askCatalogCopilot = (question: string) =>
  api<CopilotAskResponse>("/merchant/catalog/ask", { method: "POST", body: { question } })

// confirmCopilotDraft: fire-and-forget audit-provenance log, called AFTER a
// human has already confirmed a copilot-drafted change through the normal
// wizard/create-price flow. Never a mutation itself.
export const confirmCopilotDraft = (draftId: string, kind: string, priceKey?: string) =>
  api<{ message: string }>("/merchant/catalog/copilot/confirm", {
    method: "POST",
    body: { draft_id: draftId, kind, price_key: priceKey },
  })
