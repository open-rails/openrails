import { QueryClient } from "@tanstack/react-query"
import { afterEach, describe, expect, it, vi } from "vitest"
import * as endpoints from "@/lib/api/endpoints"
import type { CatalogPrice, CatalogProduct } from "@/lib/api/types"
import { adminQueries, collectCatalogPages } from "@/lib/queries"
import { tierChangeOptions } from "@/pages/subscriptions/tier-change-options"

const clients: QueryClient[] = []
afterEach(() => {
  for (const client of clients) client.clear()
  clients.length = 0
  vi.restoreAllMocks()
  vi.unstubAllGlobals()
})
function client() {
  vi.stubGlobal("localStorage", { removeItem: vi.fn() })
  vi.stubGlobal("sessionStorage", { getItem: () => null })
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  clients.push(client)
  return client
}
// Use a smaller effective server cap than the collection requests.
const pageOf = <T>(items: T[], offset: number) => ({
  items: items.slice(offset, offset + 100),
  total: items.length,
  limit: 100,
  offset,
})
describe("catalog pagination", () => {
  it("loads records after 1000 into the actual price-selection workflow", async () => {
    const products: CatalogProduct[] = Array.from({ length: 1001 }, (_, i) => ({
      id: `product-${i}`,
      key: `product-${i}`,
      display_name: `Product ${i}`,
      description: "",
      tier_group: "membership",
      tier_rank: i,
      archived: false,
      created_at: "2026-01-01",
      updated_at: "2026-01-01",
    }))
    const prices: CatalogPrice[] = products.map((product, i) => ({
      id: `price-${i}`,
      key: `price-${i}`,
      product_id: product.id,
      unit_amount: 1000000,
      currency: "USD",
      auto_renew: true,
      archived: false,
      created_at: "2026-01-01",
      updated_at: "2026-01-01",
    }))
    const loadProducts = vi
      .spyOn(endpoints, "listProducts")
      .mockImplementation(async (_limit, offset) => pageOf(products, offset))
    const loadPrices = vi
      .spyOn(endpoints, "listPrices")
      .mockImplementation(async (_limit, offset) => pageOf(prices, offset))
    const queries = client()
    const [allProducts, allPrices] = await Promise.all([
      queries.fetchQuery(adminQueries.allProducts()),
      queries.fetchQuery(adminQueries.allPrices()),
    ])
    expect(allProducts.items).toHaveLength(1001)
    expect(allPrices.items).toHaveLength(1001)
    expect(loadProducts.mock.calls.map((call) => call[1])).toEqual(
      Array.from({ length: 11 }, (_, i) => i * 100)
    )
    expect(loadPrices.mock.calls.map((call) => call[1])).toEqual(
      Array.from({ length: 11 }, (_, i) => i * 100)
    )
    const options = tierChangeOptions({
      currentProduct: allProducts.items[0],
      currentCurrency: "USD",
      products: allProducts.items,
      prices: allPrices.items,
    })
    expect(options.some((option) => option.price.id === "price-1000")).toBe(
      true
    )
  })
  it("keeps visible pages separate from collect-all caches", async () => {
    const products = Array.from(
      { length: 1001 },
      (_, i) => ({ id: String(i) }) as CatalogProduct
    )
    const load = vi
      .spyOn(endpoints, "listProducts")
      .mockImplementation(async (_limit, offset) => pageOf(products, offset))
    const queries = client()
    const first = await queries.fetchQuery(
      adminQueries.products({ limit: 100, offset: 0 })
    )
    expect(first.items).toHaveLength(100)
    expect(load).toHaveBeenCalledTimes(1)
    const last = await queries.fetchQuery(
      adminQueries.products({ limit: first.limit, offset: 1000 })
    )
    expect(last.items.map((item) => item.id)).toEqual(["1000"])
    expect(adminQueries.products().queryKey).not.toEqual(
      adminQueries.allProducts().queryKey
    )
    expect(adminQueries.prices().queryKey).not.toEqual(
      adminQueries.allPrices().queryKey
    )
  })
  it("does not silently offer a truncated selector when a page fails", async () => {
    const load = vi
      .fn()
      .mockResolvedValueOnce({
        items: ["first"],
        total: 2,
        limit: 1,
        offset: 0,
      })
      .mockRejectedValueOnce(new Error("network unavailable"))
    await expect(collectCatalogPages(load)).rejects.toThrow(
      "network unavailable"
    )
  })
  it("rejects a premature empty page instead of truncating", async () => {
    const load = vi
      .fn()
      .mockResolvedValueOnce({
        items: ["first"],
        total: 2,
        limit: 1,
        offset: 0,
      })
      .mockResolvedValueOnce({ items: [], total: 2, limit: 1, offset: 1 })
    await expect(collectCatalogPages(load)).rejects.toThrow(
      "before all records"
    )
    expect(load).toHaveBeenCalledTimes(2)
  })
  it("rejects an empty first page that still promises records", async () => {
    const load = vi
      .fn()
      .mockResolvedValue({ items: [], total: 1001, limit: 1000, offset: 0 })
    await expect(collectCatalogPages(load)).rejects.toThrow(
      "before all records"
    )
    expect(load).toHaveBeenCalledTimes(1)
  })

  it("stops collecting after the caller cancels", async () => {
    const controller = new AbortController()
    const load = vi.fn(async () => {
      controller.abort()
      return { items: ["first"], total: 2, limit: 1, offset: 0 }
    })
    await expect(collectCatalogPages(load, controller.signal)).rejects.toThrow()
    expect(load).toHaveBeenCalledTimes(1)
  })
})
