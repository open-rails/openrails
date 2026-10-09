import { api } from "@/lib/api/client"
import type {
  CreditGrant,
  CreditGrantInput,
  BalanceTransaction,
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

// A grant is a batch of one; the batch route is all or none.
export const createCreditGrant = async (
  customer: string,
  body: CreditGrantInput
) => {
  const out = await api<{ items: CreditGrant[] }>("/merchant/credit-grants", {
    method: "POST",
    body: { items: [{ customer_id: customer, ...body }] },
  })
  return out.items[0]
}

export const revokeCreditGrant = (
  customer: string,
  grant: string,
  reason: string
) =>
  api<CreditGrant>(
    `${customerPath(customer)}/credit-grants/${encodeURIComponent(grant)}/revoke`,
    { method: "POST", body: { reason } }
  )

export const listBalanceTransactions = (
  customer: string,
  currency: string,
  limit: number,
  cursor: string,
  signal?: AbortSignal
) =>
  api<ListPage<BalanceTransaction>>(`${customerPath(customer)}/balance/transactions`, {
    query: { currency, limit, cursor },
    signal,
  })
