import { api } from "@/lib/api/client"
import type {
  CreditGrant,
  CreditGrantInput,
  CreditTransaction,
  ListPage,
} from "./credit-types"

const customerPath = (id: string) =>
  `/merchant/customers/${encodeURIComponent(id)}`

export const listCreditGrants = (
  customer: string,
  currency: string,
  limit: number,
  cursor: string,
  signal?: AbortSignal
) =>
  api<ListPage<CreditGrant>>(`${customerPath(customer)}/credit-grants`, {
    query: { currency, limit, cursor },
    signal,
  })

export const createCreditGrant = (customer: string, body: CreditGrantInput) =>
  api<CreditGrant>(`${customerPath(customer)}/credit-grants`, {
    method: "POST",
    body,
  })

export const revokeCreditGrant = (
  customer: string,
  grant: string,
  reason: string
) =>
  api<CreditGrant>(
    `${customerPath(customer)}/credit-grants/${encodeURIComponent(grant)}/revoke`,
    { method: "POST", body: { reason } }
  )

export const listCreditTransactions = (
  customer: string,
  currency: string,
  limit: number,
  cursor: string,
  signal?: AbortSignal
) =>
  api<ListPage<CreditTransaction>>(`${customerPath(customer)}/transactions`, {
    query: { currency, limit, cursor },
    signal,
  })
