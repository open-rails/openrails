import { api, type PageRequest } from "./client"
import type {
  RecordExternalPaymentParams,
  ListPage,
  Order,
  Payment,
} from "./generated/wire"

// Orders are listed newest first.
export interface OrderFilters {
  customer_id?: string
  origin?: string
  status?: string
  // overdue "true" keeps the open orders still owed past their due date.
  overdue?: string
}

export const listOrders = (
  filters: OrderFilters,
  page: PageRequest,
  signal?: AbortSignal
) =>
  api<ListPage<Order>>("/admin/orders", {
    query: { ...filters, ...page },
    signal,
  })
export const getOrder = (id: string, signal?: AbortSignal) =>
  api<Order>(`/admin/orders/${id}`, { signal })
// An order's payments are payments naming it.
export const listOrderPayments = (
  id: string,
  page: PageRequest,
  signal?: AbortSignal
) =>
  api<ListPage<Payment>>("/admin/payments", {
    query: { order_id: id, ...page },
    signal,
  })

// UsageAction is what staff do to an unpaid usage order.
export type UsageAction =
  "waive_balance" | "mark_uncollectible" | "record_payment"
export interface UsageActionRequest {
  id: string
  action: UsageAction
  amount?: string
  reference?: string
}
export function applyUsageAction(
  request: UsageActionRequest
): Promise<Order | Payment> {
  if (request.action === "record_payment") {
    const body: RecordExternalPaymentParams = {
      order_id: request.id,
      amount: request.amount ?? "",
      transaction_id: request.reference ?? "",
    }
    return api<Payment>("/admin/payments", { method: "POST", body })
  }
  const path = {
    waive_balance: "waive-balance",
    mark_uncollectible: "mark-uncollectible",
  }[request.action]
  return api<Order>(`/admin/orders/${request.id}/${path}`, {
    method: "POST",
  })
}
