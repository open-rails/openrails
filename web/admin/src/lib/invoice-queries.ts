import {
  queryOptions,
  mutationOptions,
  type QueryClient,
} from "@tanstack/react-query"
import { getTokens } from "./api/client"
import { queryKeys } from "./queries"
import * as invoiceAPI from "./api/invoice-endpoints"
import type { InvoiceFilters, InvoiceProfile } from "./api/invoice-types"

export const invoiceKeys = {
  root: () =>
    ["merchant", getTokens()?.merchant ?? "unselected", "invoices"] as const,
  detail: (id: string) => [...invoiceKeys.root(), id] as const,
  profile: (customerId: string) =>
    [...queryKeys.customer(customerId), "invoice-profile"] as const,
}
export const invoiceQueries = {
  list: (filters: InvoiceFilters, limit: number, offset: number) =>
    queryOptions({
      queryKey: [...invoiceKeys.root(), "list", filters, limit, offset],
      queryFn: ({ signal }) =>
        invoiceAPI.listInvoices(filters, limit, offset, signal),
    }),
  detail: (id: string) =>
    queryOptions({
      queryKey: invoiceKeys.detail(id),
      queryFn: ({ signal }) => invoiceAPI.getInvoice(id, signal),
      enabled: !!id,
    }),
  payments: (id: string, limit: number, offset: number) =>
    queryOptions({
      queryKey: [...invoiceKeys.detail(id), "payments", limit, offset],
      queryFn: ({ signal }) =>
        invoiceAPI.listInvoicePayments(id, limit, offset, signal),
      enabled: !!id,
    }),
  profile: (customerId: string) =>
    queryOptions({
      queryKey: invoiceKeys.profile(customerId),
      queryFn: ({ signal }) => invoiceAPI.getInvoiceProfile(customerId, signal),
      enabled: !!customerId,
    }),
}
export function invoiceActionMutation(client: QueryClient, customerId: string) {
  const root = invoiceKeys.root(),
    customer = queryKeys.customer(customerId)
  return mutationOptions({
    mutationFn: invoiceAPI.applyInvoiceAction,
    onSettled: async () => {
      await Promise.all([
        client.invalidateQueries({ queryKey: root }),
        client.invalidateQueries({ queryKey: customer }),
      ])
    },
  })
}
export function invoiceProfileMutation(
  client: QueryClient,
  customerId: string
) {
  const key = invoiceKeys.profile(customerId)
  return mutationOptions({
    mutationFn: (profile: InvoiceProfile) =>
      invoiceAPI.putInvoiceProfile(customerId, profile),
    onSuccess: () => client.invalidateQueries({ queryKey: key }),
  })
}
