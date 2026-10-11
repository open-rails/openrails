import {
  keepPreviousData,
  queryOptions,
  mutationOptions,
  type QueryClient,
} from "@tanstack/react-query"
import { selectedMerchant } from "./api/client"
import { queryKeys } from "./queries"
import * as orderAPI from "./api/order-endpoints"
import type { OrderFilters } from "./api/order-endpoints"
import * as profileAPI from "./api/invoice-profile-endpoints"
import type { InvoiceProfile } from "./api/generated/wire"

export const orderKeys = {
  root: () =>
    ["merchant", selectedMerchant() ?? "unselected", "orders"] as const,
  detail: (id: string) => [...orderKeys.root(), id] as const,
  profile: (customerId: string) =>
    [...queryKeys.customer(customerId), "invoice-profile"] as const,
}
export const orderQueries = {
  list: (filters: OrderFilters, limit: number, cursor?: string) =>
    queryOptions({
      queryKey: [...orderKeys.root(), "list", filters, limit, cursor],
      queryFn: ({ signal }) =>
        orderAPI.listOrders(filters, { limit, cursor }, signal),
      placeholderData: keepPreviousData,
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
      queryFn: ({ signal }) => profileAPI.getInvoiceProfile(customerId, signal),
      enabled: !!customerId,
    }),
}
export function usageActionMutation(client: QueryClient, customerId: string) {
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
export function invoiceProfileMutation(
  client: QueryClient,
  customerId: string
) {
  const key = orderKeys.profile(customerId)
  return mutationOptions({
    mutationFn: (profile: InvoiceProfile) =>
      profileAPI.putInvoiceProfile(customerId, profile),
    onSuccess: () => client.invalidateQueries({ queryKey: key }),
  })
}
