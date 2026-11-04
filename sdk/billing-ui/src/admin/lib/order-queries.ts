import type { AdminClient } from "./api/client"
import type { AdminAPI } from "./api"
import type { createQueries } from "./queries"
import {
  keepPreviousData,
  queryOptions,
  mutationOptions,
  type QueryClient,
} from "@tanstack/react-query"

import type { OrderFilters } from "./api/order-endpoints"

import type { InvoiceProfile } from "#orck/client/generated/wire"
export function createOrderQueries(
  client: AdminClient,
  api: AdminAPI,
  queries: ReturnType<typeof createQueries>
) {
  const { queryRoot } = client
  const { queryKeys } = queries
  const orderAPI = api
  const profileAPI = api

  const orderKeys = {
    root: () => [...queryRoot(), "orders"] as const,
    detail: (id: string) => [...orderKeys.root(), id] as const,
    profile: (customerId: string) =>
      [...queryKeys.customer(customerId), "invoice-profile"] as const,
  }

  const orderQueries = {
    list: (filters: OrderFilters, limit: number, cursor?: string) =>
      queryOptions({
        queryKey: [...orderKeys.root(), "list", filters, limit, cursor],
        queryFn: ({ signal }) =>
          orderAPI.listOrders(filters, { limit, cursor }, signal),
        placeholderData: keepPreviousData,
      }),
    // The orders of ids in one request, for a page naming what its rows'
    // orders bought.
    byIds: (ids: string[]) =>
      queryOptions({
        queryKey: [...orderKeys.root(), "ids", ids],
        queryFn: ({ signal }) =>
          orderAPI.listOrders({ ids: ids.join(",") }, {}, signal),
        enabled: ids.length > 0,
      }),
    detail: (id: string) =>
      queryOptions({
        queryKey: orderKeys.detail(id),
        queryFn: ({ signal }) => orderAPI.getOrder(id, signal),
        enabled: !!id,
      }),
    payments: (id: string, limit: number, cursor?: string) =>
      queryOptions({
        queryKey: [...orderKeys.detail(id), "payments", limit, cursor],
        queryFn: ({ signal }) =>
          orderAPI.listOrderPayments(id, { limit, cursor }, signal),
        enabled: !!id,
        placeholderData: keepPreviousData,
      }),
    // The customer's invoice profile: the terms and document fields their
    // next usage orders take.
    profile: (customerId: string) =>
      queryOptions({
        queryKey: orderKeys.profile(customerId),
        queryFn: ({ signal }) =>
          profileAPI.getInvoiceProfile(customerId, signal),
        enabled: !!customerId,
      }),
  }

  function usageActionMutation(client: QueryClient, customerId: string) {
    const root = orderKeys.root(),
      customer = queryKeys.customer(customerId)
    return mutationOptions({
      mutationFn: orderAPI.applyUsageAction,
      onSettled: async () => {
        await Promise.all([
          client.invalidateQueries({ queryKey: root }),
          client.invalidateQueries({ queryKey: customer }),
        ])
      },
    })
  }

  function invoiceProfileMutation(client: QueryClient, customerId: string) {
    const key = orderKeys.profile(customerId)
    return mutationOptions({
      mutationFn: (profile: InvoiceProfile) =>
        profileAPI.putInvoiceProfile(customerId, profile),
      onSuccess: () => client.invalidateQueries({ queryKey: key }),
    })
  }
  return {
    orderKeys,
    orderQueries,
    usageActionMutation,
    invoiceProfileMutation,
  }
}
