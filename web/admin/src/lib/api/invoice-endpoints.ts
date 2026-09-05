import { api } from "./client"
import type { ItemsEnvelope } from "./client"
import type {
  InvoiceFilters,
  InvoicePage,
  MerchantInvoice,
  InvoicePayment,
  InvoiceProfile,
  InvoiceProfileResponse,
  InvoiceRetryResponse,
  InvoiceAction,
} from "./invoice-types"

export const listInvoices = (
  filters: InvoiceFilters,
  limit: number,
  offset: number,
  signal?: AbortSignal
) =>
  api<InvoicePage>("/merchant/invoices", {
    query: { ...filters, limit, offset },
    signal,
  })
export const getInvoice = (id: string, signal?: AbortSignal) =>
  api<MerchantInvoice>(`/merchant/invoices/${id}`, { signal })
export const listInvoicePayments = (
  id: string,
  limit: number,
  offset: number,
  signal?: AbortSignal
) =>
  api<ItemsEnvelope<InvoicePayment>>(`/merchant/invoices/${id}/payments`, {
    query: { limit, offset },
    signal,
  })
export const getInvoiceProfile = (customerId: string, signal?: AbortSignal) =>
  api<InvoiceProfileResponse>(
    `/merchant/customers/${customerId}/invoice-profile`,
    { signal }
  )
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
  amount?: number
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
  return api<MerchantInvoice | InvoiceRetryResponse>(
    `/merchant/invoices/${request.id}/${path}`,
    {
      method: "POST",
      headers:
        request.action === "retry_collection"
          ? { "Idempotency-Key": request.idempotencyKey ?? "" }
          : undefined,
      body:
        request.action === "record_payment"
          ? { amount: request.amount, reference: request.reference }
          : request.action === "retry_collection"
            ? { payment_method_id: request.paymentMethodId }
            : undefined,
    }
  )
}
