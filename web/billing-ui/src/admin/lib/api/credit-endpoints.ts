import type { AdminClient } from "./client"

import type {
  CreditGrant,
  CreditGrantInput,
  BalanceTransaction,
  ListPage,
} from "./credit-types"

const customerPath = (id: string) =>
  `/admin/customers/${encodeURIComponent(id)}`
export function createCreditEndpoints(client: AdminClient) {
  const { api } = client

  const listCreditGrants = (
    customer: string,
    currency: string,
    limit: number,
    cursor: string,
    signal?: AbortSignal
  ) =>
    api<ListPage<CreditGrant>>("/admin/credit-grants", {
      query: { customer_id: customer, currency, limit, cursor },
      signal,
    })

  // A grant is a batch of one; the batch route is all or none.
  const createCreditGrant = async (
    customer: string,
    body: CreditGrantInput
  ) => {
    const out = await api<{ items: CreditGrant[] }>("/admin/credit-grants", {
      method: "POST",
      body: { items: [{ customer_id: customer, ...body }] },
    })
    return out.items[0]
  }

  const revokeCreditGrant = (grant: string, reason: string) =>
    api<CreditGrant>(
      `/admin/credit-grants/${encodeURIComponent(grant)}/revoke`,
      { method: "POST", body: { reason } }
    )

  const listBalanceTransactions = (
    customer: string,
    currency: string,
    limit: number,
    cursor: string,
    signal?: AbortSignal
  ) =>
    api<ListPage<BalanceTransaction>>(
      `${customerPath(customer)}/balance/transactions`,
      {
        query: { currency, limit, cursor },
        signal,
      }
    )
  return {
    listCreditGrants,
    createCreditGrant,
    revokeCreditGrant,
    listBalanceTransactions,
  }
}
