import { api } from "./client"

// Billing analysis is intentionally provider-neutral. Provider adapters
// populate these facts; the merchant console only presents the normalized
// daily activity and outstanding cases.
export interface BillingAnalysisFilters {
  from: string
  to: string
  timezone?: string
  provider?: string
}

export interface BillingAnalysisDay {
  date: string
  signups: number
  rebills: number
  settled_other: number
  failed_signups: number
  failed_rebills: number
  failed_other: number
  open_unbilled: number
  delinquent_users: number
  charges: BillingAnalysisCharge[]
  failures: BillingAnalysisFailure[]
  unbilled?: BillingAnalysisUnbilledMember[]
}

export interface BillingAnalysisCharge {
  day: string
  kind: string
  provider: string
  psp_id?: string
  event_key: string
  transaction_id?: string
  subscription_ref?: string
  customer_ref?: string
  customer_email?: string
  order_ref?: string
  amount_cents: string | number
  currency?: string
  occurred_at: string
  source?: string
  raw?: unknown
}

export interface BillingAnalysisFailure extends BillingAnalysisCharge {
  decline_code?: string
  decline_reason?: string
  failure_count: number
}

export interface BillingAnalysisUnbilledMember {
  id: string
  subscription_id?: string
  customer_ref?: string
  email?: string
  provider?: string
  amount_cents?: string | number
  currency?: string
  unbilled_since: string
  resolved_at?: string
  last_failed_at?: string
  failure_count: number
  failure_code?: string
  failure_reason?: string
  status: string
}

export interface BillingAnalysisDelinquent {
  provider: string
  psp_id?: string
  subscription_ref?: string
  customer_ref?: string
  customer_email?: string
  status?: string
  next_billing_at?: string
  obligation_key?: string
}

export interface BillingAnalysisResponse {
  range: { from: string; to: string }
  timezone: string
  providers: string[]
  daily: BillingAnalysisDay[]
  unbilled: BillingAnalysisUnbilledMember[]
  delinquent: BillingAnalysisDelinquent[]
}

/** GET /v1/merchant/billing-analysis.
 *
 * The endpoint reads persisted provider evidence; it does not trigger a
 * provider pull. Daily rows include the raw normalized success and failure
 * evidence for that calendar day.
 */
export const getBillingAnalysis = (
  filters: BillingAnalysisFilters,
  signal?: AbortSignal
) =>
  api<BillingAnalysisResponse>("/merchant/billing-analysis", {
    query: { ...filters },
    signal,
  })
