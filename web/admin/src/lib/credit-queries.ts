import {
  mutationOptions,
  queryOptions,
  type QueryClient,
} from "@tanstack/react-query"
import { selectedMerchant } from "@/lib/api/client"
import {
  createCreditGrant,
  listCreditGrants,
  listCreditTransactions,
  revokeCreditGrant,
} from "@/lib/api/credit-endpoints"
import type { CreditGrantInput } from "@/lib/api/credit-types"

export const creditCustomerKey = (merchant: string, customer: string) =>
  ["merchant", merchant, "customers", customer] as const

function assertMerchant(merchant: string) {
  if (!merchant || selectedMerchant() !== merchant)
    throw new Error(
      "The selected merchant changed. Reload this customer's credits."
    )
}

export const creditQueries = {
  grants: (
    merchant: string,
    customer: string,
    currency: string,
    limit: number,
    cursor: string
  ) =>
    queryOptions({
      queryKey: [
        ...creditCustomerKey(merchant, customer),
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
        ...creditCustomerKey(merchant, customer),
        "transactions",
        currency,
        limit,
        cursor,
      ],
      enabled: Boolean(merchant && customer && currency),
      queryFn: ({ signal }) => {
        assertMerchant(merchant)
        return listCreditTransactions(customer, currency, limit, cursor, signal)
      },
    }),
}

export const creditMutations = {
  grant: (client: QueryClient, merchant: string, customer: string) =>
    mutationOptions({
      mutationFn: (input: CreditGrantInput) => {
        assertMerchant(merchant)
        return createCreditGrant(customer, input)
      },
      onSettled: () =>
        client.invalidateQueries({
          queryKey: creditCustomerKey(merchant, customer),
        }),
    }),
  revoke: (client: QueryClient, merchant: string, customer: string) =>
    mutationOptions({
      mutationFn: ({ grant, reason }: { grant: string; reason: string }) => {
        assertMerchant(merchant)
        return revokeCreditGrant(customer, grant, reason)
      },
      onSettled: () =>
        client.invalidateQueries({
          queryKey: creditCustomerKey(merchant, customer),
        }),
    }),
}
