import type { AdminClient } from "./client"
import { type PageRequest } from "./client"

import type {
  RecordExternalPaymentParams,
  ListPage,
  Invoice,
  Payment,
} from "#orck/client/generated/wire"

// Invoices are listed newest first.
export interface InvoiceFilters {
  customer_id?: string
  origin?: string
  status?: string
  // overdue "true" keeps the open invoices still owed past their due date.
  overdue?: string
  // ids reads the invoices of these ids, comma-separated, and takes no other
  // filter or page.
  ids?: string
}

// UsageAction is what staff do to an unpaid usage invoice.
export type UsageAction =
  "waive_balance" | "mark_uncollectible" | "record_payment"

export interface UsageActionRequest {
  id: string
  action: UsageAction
  amount?: string
  reference?: string
}
export function createInvoiceEndpoints(client: AdminClient) {
  const { api } = client

  const listInvoices = (
    filters: InvoiceFilters,
    page: PageRequest,
    signal?: AbortSignal
  ) =>
    api<ListPage<Invoice>>("/admin/invoices", {
      query: { ...filters, ...page },
      signal,
    })

  const getInvoice = (id: string, signal?: AbortSignal) =>
    api<Invoice>(`/admin/invoices/${id}`, { signal })

  // An invoice's payments are payments naming it.
  const listInvoicePayments = (
    id: string,
    page: PageRequest,
    signal?: AbortSignal
  ) =>
    api<ListPage<Payment>>("/admin/payments", {
      query: { invoice_id: id, ...page },
      signal,
    })

  function applyUsageAction(
    request: UsageActionRequest
  ): Promise<Invoice | Payment> {
    if (request.action === "record_payment") {
      const body: RecordExternalPaymentParams = {
        invoice_id: request.id,
        amount: request.amount ?? "",
        transaction_id: request.reference ?? "",
      }
      return api<Payment>("/admin/payments", { method: "POST", body })
    }
    const path = {
      waive_balance: "waive-balance",
      mark_uncollectible: "mark-uncollectible",
    }[request.action]
    return api<Invoice>(`/admin/invoices/${request.id}/${path}`, {
      method: "POST",
    })
  }
  return { listInvoices, getInvoice, listInvoicePayments, applyUsageAction }
}
