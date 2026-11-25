import type { AdminClient } from "./api/client"
import type { AdminAPI } from "./api"
import type { createQueries } from "./queries"
import {
  keepPreviousData,
  queryOptions,
  mutationOptions,
  type QueryClient,
} from "@tanstack/react-query"

import type { InvoiceFilters } from "./api/invoice-endpoints"

import type { InvoiceProfile } from "#orck/client/generated/wire"
export function createInvoiceQueries(
  client: AdminClient,
  api: AdminAPI,
  queries: ReturnType<typeof createQueries>
) {
  const { queryRoot } = client
  const { queryKeys } = queries
  const invoiceAPI = api
  const profileAPI = api

  const invoiceKeys = {
    root: () => [...queryRoot(), "invoices"] as const,
    detail: (id: string) => [...invoiceKeys.root(), id] as const,
    profile: (customerId: string) =>
      [...queryKeys.customer(customerId), "invoice-profile"] as const,
  }

  const invoiceQueries = {
    list: (filters: InvoiceFilters, limit: number, cursor?: string) =>
      queryOptions({
        queryKey: [...invoiceKeys.root(), "list", filters, limit, cursor],
        queryFn: ({ signal }) =>
          invoiceAPI.listInvoices(filters, { limit, cursor }, signal),
        placeholderData: keepPreviousData,
      }),
    // The invoices of ids in one request, for a page naming what its rows'
    // invoices bought.
    byIds: (ids: string[]) =>
      queryOptions({
        queryKey: [...invoiceKeys.root(), "ids", ids],
        queryFn: ({ signal }) =>
          invoiceAPI.listInvoices({ ids: ids.join(",") }, {}, signal),
        enabled: ids.length > 0,
      }),
    detail: (id: string) =>
      queryOptions({
        queryKey: invoiceKeys.detail(id),
        queryFn: ({ signal }) => invoiceAPI.getInvoice(id, signal),
        enabled: !!id,
      }),
    payments: (id: string, limit: number, cursor?: string) =>
      queryOptions({
        queryKey: [...invoiceKeys.detail(id), "payments", limit, cursor],
        queryFn: ({ signal }) =>
          invoiceAPI.listInvoicePayments(id, { limit, cursor }, signal),
        enabled: !!id,
        placeholderData: keepPreviousData,
      }),
    // The customer's invoice profile: the terms and document fields their
    // next usage invoices take.
    profile: (customerId: string) =>
      queryOptions({
        queryKey: invoiceKeys.profile(customerId),
        queryFn: ({ signal }) =>
          profileAPI.getInvoiceProfile(customerId, signal),
        enabled: !!customerId,
      }),
  }

  function usageActionMutation(client: QueryClient, customerId: string) {
    const root = invoiceKeys.root(),
      customer = queryKeys.customer(customerId)
    return mutationOptions({
      mutationFn: invoiceAPI.applyUsageAction,
      onSettled: async () => {
        await Promise.all([
          client.invalidateQueries({ queryKey: root }),
          client.invalidateQueries({ queryKey: customer }),
        ])
      },
    })
  }

  function invoiceProfileMutation(client: QueryClient, customerId: string) {
    const key = invoiceKeys.profile(customerId)
    return mutationOptions({
      mutationFn: (profile: InvoiceProfile) =>
        profileAPI.putInvoiceProfile(customerId, profile),
      onSuccess: () => client.invalidateQueries({ queryKey: key }),
    })
  }
  return {
    invoiceKeys,
    invoiceQueries,
    usageActionMutation,
    invoiceProfileMutation,
  }
}
