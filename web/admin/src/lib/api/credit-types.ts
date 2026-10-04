export type { CreditGrant, CreditTransaction, ListPage } from "./generated/wire"

export interface CreditGrantInput {
  currency: string
  // Native units as an exact int64 decimal string (docs/money-wire.md).
  amount: string
  source: "admin"
  source_id: string
  expires_at?: string // RFC3339
  description?: string
}
