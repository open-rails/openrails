import type { AdminClient } from "./api/client"
import type { AdminAPI } from "./api"
import {
  mutationOptions,
  queryOptions,
  type QueryClient,
} from "@tanstack/react-query"

import type { CreditGrantInput } from "#orck/admin/lib/api/credit-types"

export const creditCustomerKey = (merchant: string, customer: string) =>
  ["merchant", merchant, "customers", customer] as const
export function createCreditQueries(client: AdminClient, api: AdminAPI) {
  const { selectedMerchant } = client
  const scopedCustomerKey = (merchant: string, customer: string) =>
    [...client.queryRoot(merchant), "customers", customer] as const
  const {
    createCreditGrant,
    listCreditGrants,
    listBalanceTransactions,
    revokeCreditGrant,
  } = api

  function assertMerchant(merchant: string) {
    if (!merchant || selectedMerchant() !== merchant)
      throw new Error(
        "The selected merchant changed. Reload this customer's credits."
      )
  }

  const creditQueries = {
    grants: (
      merchant: string,
      customer: string,
      currency: string,
      limit: number,
      cursor: string
    ) =>
      queryOptions({
        queryKey: [
          ...scopedCustomerKey(merchant, customer),
          "credit-grants",
          currency,
          limit,
          cursor,
        ],
        enabled: Boolean(merchant && customer && currency),
        queryFn: ({ signal }) => {
          assertMerchant(merchant)
          return listCreditGrants(customer, currency, limit, cursor, signal)
        },
      }),
    transactions: (
      merchant: string,
      customer: string,
      currency: string,
      limit: number,
      cursor: string
    ) =>
      queryOptions({
        queryKey: [
          ...scopedCustomerKey(merchant, customer),
          "transactions",
          currency,
          limit,
          cursor,
        ],
        enabled: Boolean(merchant && customer && currency),
        queryFn: ({ signal }) => {
          assertMerchant(merchant)
          return listBalanceTransactions(
            customer,
            currency,
            limit,
            cursor,
            signal
          )
        },
      }),
  }

  const creditMutations = {
    grant: (client: QueryClient, merchant: string, customer: string) =>
      mutationOptions({
        mutationFn: (input: CreditGrantInput) => {
          assertMerchant(merchant)
          return createCreditGrant(customer, input)
        },
        onSettled: () =>
          client.invalidateQueries({
            queryKey: scopedCustomerKey(merchant, customer),
          }),
      }),
    revoke: (client: QueryClient, merchant: string, customer: string) =>
      mutationOptions({
        mutationFn: ({ grant, reason }: { grant: string; reason: string }) => {
          assertMerchant(merchant)
          return revokeCreditGrant(grant, reason)
        },
        onSettled: () =>
          client.invalidateQueries({
            queryKey: scopedCustomerKey(merchant, customer),
          }),
      }),
  }
  return { assertMerchant, creditQueries, creditMutations }
}
