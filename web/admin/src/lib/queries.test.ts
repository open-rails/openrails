// Read-side cache contract: merchant isolation, retry/error policy, and
// complete-collection loading (a selector that silently truncates would offer
// the wrong price).
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

import { ApiError } from "@/lib/api/client"
import type { CatalogPrice, CatalogProduct } from "@/lib/api/types"
import { adminQueries, collectCatalogPages, queryKeys } from "@/lib/queries"
import { queryClient, shouldRetry } from "@/lib/query-client"
import { toastApiError } from "@/lib/toast"
import { tierChangeOptions } from "@/pages/subscriptions/tier-change-options"
import { calls, client, selectMerchant, server } from "@/test/harness"

vi.mock("@/lib/toast", () => ({ toastApiError: vi.fn() }))

beforeEach(async () => {
  await server()
})
afterEach(() => {
  queryClient.clear()
  vi.clearAllMocks()
  vi.unstubAllGlobals()
})

describe("cache keys", () => {
  it("isolates every cached collection by the selected merchant", () => {
    selectMerchant("merchant-a")
    expect(queryKeys.customers()).toEqual(["merchant", "merchant-a", "customers"])
    selectMerchant("merchant-b")
    expect(queryKeys.customers()).toEqual(["merchant", "merchant-b", "customers"])
    expect(adminQueries.products().queryKey).not.toEqual(
      adminQueries.allProducts().queryKey
    )
    expect(adminQueries.prices().queryKey).not.toEqual(
      adminQueries.allPrices().queryKey
    )
  })

  it("uses an explicit scope before a merchant is selected", () => {
    expect(queryKeys.dashboard()).toEqual([
      "merchant",
      "unselected",
      "dashboard",
    ])
    expect(queryKeys.usageMeter("tokens")).toEqual([
      ...queryKeys.usageMeters(),
      "tokens",
    ])
    expect(queryKeys.customerUsageRates("cus_1")).toEqual([
      ...queryKeys.customer("cus_1"),
      "usage-rates",
    ])
  })
})

describe("failure policy", () => {
  it("retries transient failures at most twice and never a client error", () => {
    const transient = new ApiError(503, null, "unavailable")
    expect([0, 1, 2].map((attempt) => shouldRetry(attempt, transient))).toEqual([
      true,
      true,
      false,
    ])
    expect(shouldRetry(0, new TypeError("network error"))).toBe(true)
    expect(shouldRetry(0, new ApiError(400, null, "bad request"))).toBe(false)
  })

  it("reports an error only when the usage asked for it", async () => {
    const error = new ApiError(503, null, "unavailable")
    await expect(
      queryClient.fetchQuery({
        queryKey: ["test", "silent"],
        queryFn: () => Promise.reject(error),
        retry: false,
      })
    ).rejects.toBe(error)
    expect(toastApiError).not.toHaveBeenCalled()

    await expect(
      queryClient.fetchQuery({
        queryKey: ["test", "reported"],
        queryFn: () => Promise.reject(error),
        retry: false,
        meta: { errorAction: "Load test data" },
      })
    ).rejects.toBe(error)
    expect(toastApiError).toHaveBeenCalledWith(error, "Load test data")
    expect(adminQueries.merchantSettings().meta).toBeUndefined()
    expect(adminQueries.merchantSettings("Load settings").meta).toEqual({
      errorAction: "Load settings",
    })
  })
})

describe("complete collections", () => {
  // A smaller effective server page than the 200 the collection requests.
  const pageOf = <T,>(items: T[], query: string) => {
    const offset = Number(new URLSearchParams(query).get("offset"))
    return { items: items.slice(offset, offset + 100), total: items.length, limit: 100, offset }
  }

  it("loads records past the first page into the price-selection workflow", async () => {
    const products: CatalogProduct[] = Array.from({ length: 1001 }, (_, i) => ({
      id: `prod_${i}`,
      key: `prod_${i}`,
      display_name: `Product ${i}`,
      description: "",
      tier_group: "membership",
      tier_rank: i,
      archived: false,
      created_at: "2026-01-01",
      updated_at: "2026-01-01",
    }))
    const prices: CatalogPrice[] = products.map((item, i) => ({
      id: `price_${i}`,
      key: `price_${i}`,
      product_id: item.id,
      unit_amount: "1000000",
      currency: "USD",
      auto_renew: true,
      archived: false,
      created_at: "2026-01-01",
      updated_at: "2026-01-01",
    }))
    const requests = await server({
      "/merchant/catalog/products": (request) => pageOf(products, request.query),
      "/merchant/catalog/prices": (request) => pageOf(prices, request.query),
    })
    const queries = client()

    const [allProducts, allPrices] = await Promise.all([
      queries.fetchQuery(adminQueries.allProducts()),
      queries.fetchQuery(adminQueries.allPrices()),
    ])

    expect([allProducts.items.length, allPrices.items.length]).toEqual([
      1001, 1001,
    ])
    expect(calls(requests)).toHaveLength(22)
    const options = tierChangeOptions({
      currentProduct: allProducts.items[0],
      currentCurrency: "USD",
      products: allProducts.items,
      prices: allPrices.items,
    })
    expect(options.some((option) => option.price.id === "price_1000")).toBe(true)

    // A visible page is a separate request and a separate cache entry.
    const first = await queries.fetchQuery(
      adminQueries.products({ limit: 100, offset: 0 })
    )
    expect(first.items).toHaveLength(100)
    expect(calls(requests)).toHaveLength(23)
    queries.clear()
  })

  it.each([
    [
      "a page fails",
      [{ items: ["first"], total: 2, limit: 1, offset: 0 }, "network unavailable"],
      "network unavailable",
    ],
    [
      "a later page comes back empty",
      [
        { items: ["first"], total: 2, limit: 1, offset: 0 },
        { items: [], total: 2, limit: 1, offset: 1 },
      ],
      "before all records",
    ],
    [
      "the first page is empty but records are promised",
      [{ items: [], total: 1001, limit: 1000, offset: 0 }],
      "before all records",
    ],
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
    await expect(
      collectCatalogPages(load, controller.signal)
    ).rejects.toThrow()
    expect(load).toHaveBeenCalledTimes(1)
  })
})
