import { QueryClient, QueryObserver } from "@tanstack/react-query"
import { renderToStaticMarkup } from "react-dom/server"
import { afterEach, expect, it, vi } from "vitest"
import * as endpoints from "@/lib/api/endpoints"
import type {
  CustomerBillingProfile,
  PaymentMethodResponse,
} from "@/lib/api/types"
import { adminQueries, queryKeys } from "@/lib/queries"
import { CollectionDefaultBadges } from "./collection-default-badges"

afterEach(() => {
  vi.restoreAllMocks()
  vi.unstubAllGlobals()
})

it("labels collection defaults with their currencies and removes cleared badges", () => {
  const first = renderToStaticMarkup(
    <CollectionDefaultBadges currencies={["EUR", "USD"]} />
  )
  expect(first).toContain("Collection default · EUR")
  expect(first).toContain("Collection default · USD")
  expect(first).not.toContain("subscription")
  expect(
    renderToStaticMarkup(<CollectionDefaultBadges currencies={[]} />)
  ).not.toContain("Collection default")
  expect(renderToStaticMarkup(<CollectionDefaultBadges />)).not.toContain(
    "Collection default"
  )
})

it("refreshes profile and saved-method caches after the server deletes or clears a default", async () => {
  vi.stubGlobal("localStorage", { removeItem: vi.fn() })
  vi.stubGlobal("sessionStorage", { getItem: () => null })
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Infinity } },
  })
  const method: PaymentMethodResponse = {
    id: "pm-a",
    object: "payment_method",
    type: "card",
    rail: "nmi",
    livemode: false,
    created: 0,
    collection_default_currencies: ["USD"],
  }
  let methods = [method]
  const profile = (): CustomerBillingProfile => ({
    customer_id: "customer-a",
    subscriptions: [],
    entitlements: [],
    payments: [],
    payment_methods: methods,
    credit_balance: [],
    product_access: [],
  })
  vi.spyOn(endpoints, "getCustomerProfile").mockImplementation(async () =>
    profile()
  )
  vi.spyOn(endpoints, "getCustomerPaymentMethods").mockImplementation(
    async () => ({ object: "list", data: methods })
  )
  const profileQuery = adminQueries.customer("customer-a")
  const methodsQuery = adminQueries.customerPaymentMethods("customer-a")
  await client.fetchQuery(profileQuery)
  await client.fetchQuery(methodsQuery)
  const profileObserver = new QueryObserver(client, profileQuery)
  const methodsObserver = new QueryObserver(client, methodsQuery)
  const unsubscribeProfile = profileObserver.subscribe(() => {})
  const unsubscribeMethods = methodsObserver.subscribe(() => {})
  try {
    expect(
      profileObserver.getCurrentResult().data?.payment_methods[0]
        .collection_default_currencies
    ).toEqual(["USD"])
    // This is the same customer subtree invalidated by Refresh payment methods.
    methods = [{ ...method, collection_default_currencies: [] }]
    await client.invalidateQueries({
      queryKey: queryKeys.customer("customer-a"),
    })
    const current = profileObserver.getCurrentResult().data!.payment_methods[0]
    expect(
      renderToStaticMarkup(
        <CollectionDefaultBadges
          currencies={current.collection_default_currencies}
        />
      )
    ).not.toContain("Collection default")
    expect(
      methodsObserver.getCurrentResult().data?.data[0]
        .collection_default_currencies
    ).toEqual([])
    methods = []
    await client.invalidateQueries({
      queryKey: queryKeys.customer("customer-a"),
    })
    expect(profileObserver.getCurrentResult().data?.payment_methods).toEqual([])
    expect(methodsObserver.getCurrentResult().data?.data).toEqual([])
  } finally {
    unsubscribeProfile()
    unsubscribeMethods()
    profileObserver.destroy()
    methodsObserver.destroy()
    client.clear()
  }
})
