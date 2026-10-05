// Catalog assistant: read-only Q&A over the catalog, and drafts of price
// changes when drafting is enabled. A draft changes nothing: it opens,
// pre-filled, in the price-change wizard or the new-price form for a person
// to confirm.
import { api } from "./client"
import type { AskCatalogParams, CatalogAnswer } from "./generated/wire"

export type {
  CatalogAnswer,
  CatalogDraft,
  NewPriceDraft,
  PriceChangeDraft,
} from "./generated/wire"

// askCatalog is not mounted unless the deployment enables the assistant.
export const askCatalog = (question: string) =>
  api<CatalogAnswer>("/merchant/catalog/ask", {
    method: "POST",
    body: { question } satisfies AskCatalogParams,
  })
