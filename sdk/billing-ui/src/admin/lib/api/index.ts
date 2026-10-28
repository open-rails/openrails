import type { AdminClient } from "./client"
import { createEndpoints } from "./endpoints"
import { createMetrics } from "./metrics"
import { createCopilot } from "./copilot"
import { createCreditEndpoints } from "./credit-endpoints"
import { createOrderEndpoints } from "./order-endpoints"
import { createInvoiceProfileEndpoints } from "./invoice-profile-endpoints"
import { createImportEndpoints } from "./import-endpoints"

export function createAdminAPI(client: AdminClient) {
  const endpoints = createEndpoints(client)
  return {
    ...endpoints,
    ...createMetrics(client),
    ...createCopilot(client),
    ...createCreditEndpoints(client),
    ...createOrderEndpoints(client),
    ...createInvoiceProfileEndpoints(endpoints),
    ...createImportEndpoints(client),
  }
}

export type AdminAPI = ReturnType<typeof createAdminAPI>
