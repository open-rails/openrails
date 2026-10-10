// Read-side cache contract: merchant isolation, retry/error policy, and
// complete-collection loading (a selector that silently truncates would offer
// the wrong price).
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

import { ApiError } from "@/lib/api/client"
import { adminQueries, collectPages, queryKeys } from "@/lib/queries"
import { queryClient, shouldRetry } from "@/lib/query-client"
import { toastApiError } from "@/lib/toast"
import { subscriptionChangeOptions } from "@/pages/subscriptions/subscription-change-options"
import { aPrice, aProduct, calls, client, selectMerchant, server } from "@/test/harness"

vi.mock("@/lib/toast", () => ({ toastApiError: vi.fn() }))

beforeEach(async () => {
  await server()
})
afterEach(() => {
  queryClient.clear()
  vi.clearAllMocks()
  vi.unstubAllGlobals()
})

it("isolates every cached collection by the selected merchant", () => {
  selectMerchant("merchant-a")
  expect(queryKeys.customers()).toEqual(["merchant", "merchant-a", "customers"])
  selectMerchant("merchant-b")
  expect(queryKeys.customers()).toEqual(["merchant", "merchant-b", "customers"])
  // A complete collection is cached apart from the page a screen is showing.
  expect(adminQueries.products().queryKey).not.toEqual(adminQueries.allProducts().queryKey)
  expect(adminQueries.prices().queryKey).not.toEqual(adminQueries.allPrices().queryKey)
})

it("scopes cached server state before a merchant is selected", () => {
  expect(queryKeys.dashboard()).toEqual(["merchant", "unselected", "dashboard"])
})

it("keeps same-named price histories and price migrations separate by product", async () => {
  selectMerchant("merchant-a")
  const requests = await server({
    "/admin/catalog/prices/price_premium/history": { data: [{ price_id: "premium_price" }] },
    "/admin/catalog/prices/price_basic/history": { data: [{ price_id: "basic_price" }] },
    "/admin/price-migrations": ({ query }) => ({ data: [{ id: new URLSearchParams(query).get("product_key") }] }),
  })
  const queries = client({ staleTime: Infinity })
  try {
    const premiumHistory = await queries.fetchQuery(adminQueries.priceHistory("price_premium"))
    const basicHistory = await queries.fetchQuery(adminQueries.priceHistory("price_basic"))
    const premiumBatches = await queries.fetchQuery(adminQueries.priceMigrations("premium", "monthly"))
    const basicBatches = await queries.fetchQuery(adminQueries.priceMigrations("basic", "monthly"))

    expect(premiumHistory.data).toEqual([{ price_id: "premium_price" }])
    expect(basicHistory.data).toEqual([{ price_id: "basic_price" }])
    expect(premiumBatches.data).toEqual([{ id: "premium" }])
    expect(basicBatches.data).toEqual([{ id: "basic" }])
    expect(calls(requests)).toEqual([
      "GET /admin/catalog/prices/price_premium/history",
      "GET /admin/catalog/prices/price_basic/history",
      "GET /admin/price-migrations",
      "GET /admin/price-migrations",
    ])
    expect(new URLSearchParams(requests[2].query).get("price_key")).toBe("monthly")
    expect(new URLSearchParams(requests[3].query).get("price_key")).toBe("monthly")
  } finally {
    queries.clear()
  }
})

it("reads a customer's entitlements and product access from the batch lists", async () => {
  selectMerchant("merchant-a")
  const requests = await server({
    "/admin/entitlements": { data: [{ customer_id: "cus_1", entitlement: "pro", quantity: 3 }], next_cursor: null },
    "/admin/product-access": { data: [], next_cursor: null },
  })
  const queries = client({ staleTime: Infinity })
  try {
    const held = await queries.fetchQuery(adminQueries.customerEntitlements("cus_1", 50, ""))
    await queries.fetchQuery(adminQueries.customerProductAccess("cus_1", 50, ""))
    expect(held.data[0].quantity).toBe(3)
    expect(calls(requests)).toEqual(["GET /admin/entitlements", "GET /admin/product-access"])
    for (const request of requests) {
      expect(new URLSearchParams(request.query).get("customer_id")).toBe("cus_1")
    }
  } finally {
    queries.clear()
  }
})

it("waits for the price before loading its history, and for both keys before migrations", () => {
  expect(adminQueries.priceHistory(undefined).enabled).toBe(false)
  expect(adminQueries.priceHistory("price_1").enabled).toBe(true)
  expect(adminQueries.priceMigrations(undefined, "monthly").enabled).toBe(false)
  expect(adminQueries.priceMigrations("premium", undefined).enabled).toBe(false)
  expect(adminQueries.priceMigrations("premium", "monthly").enabled).toBe(true)
})

it("retries transient failures at most twice and never a client error", () => {
  const transient = new ApiError(503, null, "unavailable")
  expect([0, 1, 2].map((attempt) => shouldRetry(attempt, transient))).toEqual([true, true, false])
  expect(shouldRetry(0, new TypeError("network error"))).toBe(true)
  expect(shouldRetry(0, new ApiError(400, null, "bad request"))).toBe(false)
})

it("reports a failed load only when the usage asked for it", async () => {
  const failing = (key: string, meta?: Record<string, unknown>) =>
    queryClient.fetchQuery({
      queryKey: [key],
      queryFn: () => Promise.reject(new ApiError(503, null, "unavailable")),
      retry: false,
      meta,
    })

  await expect(failing("silent")).rejects.toThrow("unavailable")
  expect(toastApiError).not.toHaveBeenCalled()
  await expect(failing("reported", { errorAction: "Load test data" })).rejects.toThrow("unavailable")
  expect(toastApiError).toHaveBeenCalledWith(expect.any(ApiError), "Load test data")
})

describe("complete collections", () => {
  // A smaller effective server page than the collection requests.
  const pageOf = <T,>(items: T[], query: string) => {
    const offset = Number(new URLSearchParams(query).get("cursor") ?? "0")
    const next = offset + 100
    return { data: items.slice(offset, next), next_cursor: next < items.length ? String(next) : null }
  }

  it("loads records past the first page into the price-selection workflow", async () => {
    const products = Array.from({ length: 1001 }, (_, i) => aProduct(`prod_${i}`, i))
    const prices = products.map((product, i) => aPrice(`price_${i}`, product.id))
    const requests = await server({
      "/admin/catalog/products": (request) => pageOf(products, request.query),
      "/admin/catalog/prices": (request) => pageOf(prices, request.query),
    })
    const queries = client()

    const [allProducts, allPrices] = await Promise.all([
      queries.fetchQuery(adminQueries.allProducts()),
      queries.fetchQuery(adminQueries.allPrices()),
    ])

    expect([allProducts.data.length, allPrices.data.length]).toEqual([1001, 1001])
    expect(calls(requests)).toHaveLength(22)
    const options = subscriptionChangeOptions({
      currentProduct: allProducts.data[0],
      currentCurrency: "USD",
      products: allProducts.data,
      prices: allPrices.data,
    })
    expect(options.some((option) => option.price.id === "price_1000")).toBe(true)
    // A visible page is its own request, not a slice of the collection.
    expect((await queries.fetchQuery(adminQueries.products({ limit: 100 }))).data).toHaveLength(100)
    expect(calls(requests)).toHaveLength(23)
    queries.clear()
  })

  it.each([
    ["a page fails", [{ data: ["first"], next_cursor: "1" }, "network unavailable"], "network unavailable"],
    ["a page repeats its cursor",
      [{ data: ["first"], next_cursor: "1" }, { data: ["second"], next_cursor: "1" }],
      "before all records"],
  ])("refuses to truncate the collection when %s", async (_name, pages, message) => {
    const load = vi.fn(async () => {
      const page = pages.shift()
      if (typeof page === "string") throw new Error(page)
      return page!
    })
    await expect(collectPages(load)).rejects.toThrow(message)
  })

  it("stops collecting once the caller cancels", async () => {
    const controller = new AbortController()
    const load = vi.fn(async () => {
      controller.abort()
      return { data: ["first"], next_cursor: "1" }
    })
    await expect(collectPages(load, controller.signal)).rejects.toThrow()
    expect(load).toHaveBeenCalledTimes(1)
  })
})
