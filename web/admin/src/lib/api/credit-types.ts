export interface CreditGrant {
  id: string
  customer_id: string
  currency: string
  amount: number
  spent_amount: number
  remaining_amount: number
  revoked_amount: number
  expired_amount: number
  state: "active" | "scheduled" | "spent" | "expired" | "revoked" | "terminated"
  source_type: string
  source_id: string
  reason?: string
  starts_at: string
  expires_at?: string
  created_at: string
  terminated_at?: string
  termination_reason?: string
}

export interface CreditGrantPage {
  unit_decimals: number
  grants: CreditGrant[]
  total: number
  limit: number
  offset: number
  can_grant: boolean
  can_revoke: boolean
}

export interface CreditGrantInput {
  currency: string
  amount: number
  source: "admin"
  source_id: string
  expires_at?: number
  description?: string
}

export interface CreditRevocation {
  grant: CreditGrant
  replayed: boolean
}

export interface CreditTransactionPage {
  unit_decimals: number
  transactions: {
    id: string
    customer_id: string
    amount: number
    currency: string
    transaction_type: string
    status: string
    source?: string | null
    created_at: string
  }[]
  total: number
  limit: number
  offset: number
}
