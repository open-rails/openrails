import { api } from "./client"

// Billing analysis is intentionally provider-neutral. Provider adapters
// populate these facts; the merchant console only presents the normalized
// daily activity and outstanding cases.
export interface BillingAnalysisFilters {
  from: string
  to: string
  timezone?: string
  provider?: string
  status?: string
}

export interface BillingAnalysisDay {
  date: string
  signups: number
  rebills: number
  failed_rebills: number
  open_unbilled: number
  unbilled?: BillingAnalysisUnbilledMember[]
}

export interface BillingAnalysisUnbilledMember {
  id: string
  customer_id?: string
  subscription_id?: string
  email?: string
  provider?: string
  amount?: string | number
  currency?: string
  unbilled_since: string
  resolved_at?: string
  last_failed_at?: string
  failure_count: number
  failure_code?: string
  failure_reason?: string
  status: string
}

export interface BillingAnalysisResponse {
  range: { from: string; to: string }
  timezone: string
  providers: string[]
  daily: BillingAnalysisDay[]
  unbilled: BillingAnalysisUnbilledMember[]
}

/** GET /v1/merchant/billing-analysis.
 *
 * The endpoint reads persisted provider evidence; it does not trigger a
 * provider pull. `status` filters the unbilled member list while daily counts
 * remain the complete activity for the selected range.
 */
export const getBillingAnalysis = (
  filters: BillingAnalysisFilters,
  signal?: AbortSignal
) =>
  api<BillingAnalysisResponse>("/merchant/billing-analysis", {
    query: { ...filters },
    signal,
  })
