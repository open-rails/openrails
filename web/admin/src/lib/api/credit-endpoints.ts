import { api } from "@/lib/api/client"
import type {
  CreditGrantInput,
  CreditGrantPage,
  CreditRevocation,
  CreditTransactionPage,
} from "./credit-types"

const customerPath = (id: string) =>
  `/merchant/customers/${encodeURIComponent(id)}`

export const listCreditGrants = (
  customer: string,
  currency: string,
  limit: number,
  offset: number,
  signal?: AbortSignal
) =>
  api<CreditGrantPage>(`${customerPath(customer)}/credits`, {
    query: { currency, limit, offset },
    signal,
  })

export const createCreditGrant = (customer: string, body: CreditGrantInput) =>
  api<{ ID: string; Replayed: boolean }>(`${customerPath(customer)}/credits`, {
    method: "POST",
    body,
  })

export const revokeCreditGrant = (
  customer: string,
  grant: string,
  reason: string
) =>
  api<CreditRevocation>(
    `${customerPath(customer)}/credits/${encodeURIComponent(grant)}`,
    { method: "DELETE", body: { reason } }
  )

export const listCreditTransactions = (
  customer: string,
  currency: string,
  limit: number,
  offset: number,
  signal?: AbortSignal
) =>
  api<CreditTransactionPage>(`${customerPath(customer)}/credit-transactions`, {
    query: { currency, limit, offset },
    signal,
  })
