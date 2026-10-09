import { api, type PageRequest } from "./client"
import { getCustomer, updateCustomer } from "./endpoints"
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
  // overdue "true" keeps the open invoices still owed past their due date.
  overdue?: string
  period_starts_after?: string
  period_starts_before?: string
}

export const listInvoices = (
  filters: InvoiceFilters,
  page: PageRequest,
  signal?: AbortSignal
) =>
  api<ListPage<Invoice>>("/admin/invoices", {
    query: { ...filters, ...page },
    signal,
  })
export const getInvoice = (id: string, signal?: AbortSignal) =>
  api<Invoice>(`/admin/invoices/${id}`, { signal })
export const listInvoicePayments = (
  id: string,
  page: PageRequest,
  signal?: AbortSignal
) =>
  api<ListPage<InvoicePayment>>(`/admin/invoices/${id}/payments`, {
    query: { ...page },
    signal,
  })
// getInvoiceProfile is the customer's invoice_profile setting; null while it
// has none.
export const getInvoiceProfile = async (
  customerId: string,
  signal?: AbortSignal
) => (await getCustomer(customerId, signal)).settings.invoice_profile ?? null
export const putInvoiceProfile = async (
  customerId: string,
  profile: InvoiceProfile
) => {
  const { settings } = await updateCustomer(customerId, {
    invoice_profile: profile,
  })
  return settings.invoice_profile
}
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
    `/admin/invoices/${request.id}/${path}`,
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
