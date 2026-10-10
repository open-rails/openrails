import { api, type PageRequest } from "./client"
import { getCustomer, updateCustomer } from "./endpoints"
import type {
  CreatePaymentParams,
  Invoice,
  InvoiceCollection,
  InvoiceProfile,
  ListPage,
  Payment,
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
// An invoice's payments are payments naming it.
export const listInvoicePayments = (
  id: string,
  page: PageRequest,
  signal?: AbortSignal
) =>
  api<ListPage<Payment>>("/admin/payments", {
    query: { invoice_id: id, ...page },
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
export function applyInvoiceAction(
  request: InvoiceActionRequest
): Promise<Invoice | InvoiceCollection | Payment> {
  if (request.action === "record_payment") {
    const body: CreatePaymentParams = {
      invoice_id: request.id,
      amount: request.amount ?? "",
      transaction_id: request.reference ?? "",
    }
    return api<Payment>("/admin/payments", { method: "POST", body })
  }
  const path = {
    void: "void",
    mark_uncollectible: "uncollectible",
    retry_collection: "retry-collection",
  }[request.action]
  const body: RetryInvoiceCollectionParams | undefined =
    request.action === "retry_collection"
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
