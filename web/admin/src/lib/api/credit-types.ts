export interface CreditGrant {
  id: string
  customer_id: string
  currency: string
  // Native units as exact int64 decimal strings (docs/money-wire.md).
  amount: string
  spent_amount: string
  remaining_amount: string
  revoked_amount: string
  expired_amount: string
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
  amount: string
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
    amount: string
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
