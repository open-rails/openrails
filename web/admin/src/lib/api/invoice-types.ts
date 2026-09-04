import type { ItemsEnvelope } from "./client"

export type InvoiceAction =
  "void" | "mark_uncollectible" | "record_payment" | "retry_collection"
export interface InvoiceContact {
  name?: string
  email: string
}
export interface InvoiceProfile {
  net_terms_days: number
  collection_method: "charge_automatically" | "send_invoice"
  po_number?: string
  tax?: Record<string, unknown>
  billing_contacts?: InvoiceContact[]
  memo?: string
}
export interface MerchantInvoice {
  id: string
  customer_id: string
  unit_decimals: number
  currency: string
  invoice_number?: string
  status: string
  period_from: string
  period_to: string
  issued_at?: string
  due_at?: string
  paid_at?: string
  finalized_at?: string
  total_amount: number
  subtotal_amount: number
  amount_paid: number
  amount_due: number
  collection_method: string
  collection_failure_count: number
  last_collection_failure_code?: string
  next_collection_attempt_at?: string
  po_number?: string
  tax?: Record<string, unknown>
  billing_contacts?: InvoiceContact[]
  memo?: string
  line_items: {
    event_type: string
    amount: number
    count: number
    dimensions?: Record<string, number>
  }[]
  available_actions: InvoiceAction[]
  payment_methods?: {
    id: string
    rail: string
    last_four?: string
    card_type?: string
  }[]
}
export interface InvoicePayment {
  id: string
  invoice_id: string
  unit_decimals: number
  currency: string
  amount: number
  status: string
  payment_method_id?: string
  rail?: string
  rail_payment_id?: string
  failure_code?: string
  failure_reason?: string
  attempted_at: string
  settled_at?: string
}
export interface InvoiceFilters {
  customer_id?: string
  currency?: string
  status?: string
  period_from?: string
  period_to?: string
}
export type InvoicePage = ItemsEnvelope<MerchantInvoice>
export interface InvoiceProfileResponse {
  customer_id: string
  profile: InvoiceProfile | null
  can_update: boolean
}
export interface InvoiceRetryResponse {
  invoice: Omit<
    MerchantInvoice,
    "customer_id" | "available_actions" | "payment_methods" | "unit_decimals"
  >
  attempt: Omit<InvoicePayment, "unit_decimals">
  replayed: boolean
}
