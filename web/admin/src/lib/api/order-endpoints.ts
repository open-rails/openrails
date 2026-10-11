import { api, type PageRequest } from "./client"
import type {
  CreatePaymentParams,
  ListPage,
  Order,
  Payment,
  RetryOrderCollectionParams,
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
  "void" | "mark_uncollectible" | "record_payment" | "retry_collection"
export interface UsageActionRequest {
  id: string
  action: UsageAction
  amount?: string
  reference?: string
  paymentMethodId?: string
  idempotencyKey?: string
}
export function applyUsageAction(
  request: UsageActionRequest
): Promise<Order | Payment> {
  if (request.action === "record_payment") {
    const body: CreatePaymentParams = {
      order_id: request.id,
      amount: request.amount ?? "",
      transaction_id: request.reference ?? "",
    }
    return api<Payment>("/admin/payments", { method: "POST", body })
  }
  const path = {
    void: "void",
    mark_uncollectible: "mark-uncollectible",
    retry_collection: "retry-collection",
  }[request.action]
  const body: RetryOrderCollectionParams | undefined =
    request.action === "retry_collection"
      ? { payment_method_id: request.paymentMethodId }
      : undefined
  return api<Order>(`/admin/orders/${request.id}/${path}`, {
    method: "POST",
    headers:
      request.action === "retry_collection"
        ? { "Idempotency-Key": request.idempotencyKey ?? "" }
        : undefined,
    body,
  })
}
