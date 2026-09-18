// Read-side cache contract: merchant isolation, retry/error policy, and
// complete-collection loading (a selector that silently truncates would offer
// the wrong price).
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

import { ApiError } from "@/lib/api/client"
import { adminQueries, collectCatalogPages, queryKeys } from "@/lib/queries"
import { queryClient, shouldRetry } from "@/lib/query-client"
import { toastApiError } from "@/lib/toast"
import { tierChangeOptions } from "@/pages/subscriptions/tier-change-options"
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
  // A smaller effective server page than the 200 the collection requests.
  const pageOf = <T,>(items: T[], query: string) => {
    const offset = Number(new URLSearchParams(query).get("offset"))
    return { items: items.slice(offset, offset + 100), total: items.length, limit: 100, offset }
  }

  it("loads records past the first page into the price-selection workflow", async () => {
    const products = Array.from({ length: 1001 }, (_, i) => aProduct(`prod_${i}`, i))
    const prices = products.map((product, i) => aPrice(`price_${i}`, product.id))
    const requests = await server({
      "/merchant/catalog/products": (request) => pageOf(products, request.query),
      "/merchant/catalog/prices": (request) => pageOf(prices, request.query),
    })
    const queries = client()

    const [allProducts, allPrices] = await Promise.all([
      queries.fetchQuery(adminQueries.allProducts()),
      queries.fetchQuery(adminQueries.allPrices()),
    ])

    expect([allProducts.items.length, allPrices.items.length]).toEqual([1001, 1001])
    expect(calls(requests)).toHaveLength(22)
    const options = tierChangeOptions({
      currentProduct: allProducts.items[0],
      currentCurrency: "USD",
      products: allProducts.items,
      prices: allPrices.items,
    })
    expect(options.some((option) => option.price.id === "price_1000")).toBe(true)
    // A visible page is its own request, not a slice of the collection.
    expect((await queries.fetchQuery(adminQueries.products({ limit: 100, offset: 0 }))).items).toHaveLength(100)
    expect(calls(requests)).toHaveLength(23)
    queries.clear()
  })

  it.each([
    ["a page fails", [{ items: ["first"], total: 2, limit: 1, offset: 0 }, "network unavailable"], "network unavailable"],
    ["a later page comes back empty",
      [{ items: ["first"], total: 2, limit: 1, offset: 0 }, { items: [], total: 2, limit: 1, offset: 1 }],
      "before all records"],
    ["the first page is empty but records are promised",
      [{ items: [], total: 1001, limit: 1000, offset: 0 }], "before all records"],
  ])("refuses to truncate the collection when %s", async (_name, pages, message) => {
    const load = vi.fn(async () => {
      const page = pages.shift()
      if (typeof page === "string") throw new Error(page)
      return page!
    })
    await expect(collectCatalogPages(load)).rejects.toThrow(message)
  })

  it("stops collecting once the caller cancels", async () => {
    const controller = new AbortController()
    const load = vi.fn(async () => {
      controller.abort()
      return { items: ["first"], total: 2, limit: 1, offset: 0 }
    })
    await expect(collectCatalogPages(load, controller.signal)).rejects.toThrow()
    expect(load).toHaveBeenCalledTimes(1)
  })
})
