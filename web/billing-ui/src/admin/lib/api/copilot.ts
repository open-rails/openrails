import type { AdminClient } from "./client"

import type {
  AskCatalogParams,
  CatalogAnswer,
} from "#orck/client/generated/wire"

export type {
  CatalogAnswer,
  CatalogDraft,
  NewPriceDraft,
  PriceChangeDraft,
} from "#orck/client/generated/wire"
export function createCopilot(client: AdminClient) {
  const { api } = client

  // askCatalog is not mounted unless the deployment enables the assistant.
  const askCatalog = (question: string) =>
    api<CatalogAnswer>("/console/catalog/ask", {
      method: "POST",
      body: { question } satisfies AskCatalogParams,
    })
  return { askCatalog }
}
