import { api, ApiError, type PageRequest } from "./client"
import type {
  CreateInvoicePaymentParams,
  Invoice,
  InvoiceCollection,
  InvoicePayment,
  InvoiceProfile,
  ListPage,
  RetryInvoiceCollectionParams,
} from "./generated/wire"

export type InvoiceAction = NonNullable<Invoice["available_actions"]>[number]

// Invoices are listed newest period first; period bounds are the period start.
export interface InvoiceFilters {
  customer_id?: string
  currency?: string
  status?: string
  period_from?: string
  period_to?: string
}

export const listInvoices = (
  filters: InvoiceFilters,
  page: PageRequest,
  signal?: AbortSignal
) =>
  api<ListPage<Invoice>>("/merchant/invoices", {
    query: { ...filters, ...page },
    signal,
  })
export const getInvoice = (id: string, signal?: AbortSignal) =>
  api<Invoice>(`/merchant/invoices/${id}`, { signal })
export const listInvoicePayments = (
  id: string,
  page: PageRequest,
  signal?: AbortSignal
) =>
  api<ListPage<InvoicePayment>>(`/merchant/invoices/${id}/payments`, {
    query: { ...page },
    signal,
  })
// getInvoiceProfile is null while the customer has none.
export const getInvoiceProfile = async (
  customerId: string,
  signal?: AbortSignal
) => {
  try {
    return await api<InvoiceProfile>(
      `/merchant/customers/${customerId}/invoice-profile`,
      { signal }
    )
  } catch (error) {
    if (error instanceof ApiError && error.code === "resource_not_found")
      return null
    throw error
  }
}
export const putInvoiceProfile = (
  customerId: string,
  profile: InvoiceProfile
) =>
  api<InvoiceProfile>(`/merchant/customers/${customerId}/invoice-profile`, {
    method: "PUT",
    body: profile,
  })
export interface InvoiceActionRequest {
  id: string
  action: InvoiceAction
  amount?: string
  reference?: string
  paymentMethodId?: string
  idempotencyKey?: string
}
export function applyInvoiceAction(request: InvoiceActionRequest) {
  const path = {
    void: "void",
    mark_uncollectible: "uncollectible",
    record_payment: "payments",
    retry_collection: "retry-collection",
  }[request.action]
  const body:
    CreateInvoicePaymentParams | RetryInvoiceCollectionParams | undefined =
    request.action === "record_payment"
      ? { amount: request.amount, reference: request.reference }
      : request.action === "retry_collection"
        ? { payment_method_id: request.paymentMethodId }
        : undefined
  return api<Invoice | InvoiceCollection>(
    `/merchant/invoices/${request.id}/${path}`,
    {
      method: "POST",
      headers:
        request.action === "retry_collection"
          ? { "Idempotency-Key": request.idempotencyKey ?? "" }
          : undefined,
      body,
    }
  )
}
