import { describe, expect, it } from "vitest"

import type { CatalogPrice, CatalogProduct } from "@/lib/api/types"
import { tierChangeOptions } from "@/pages/subscriptions/tier-change-options"

const product = (
  id: string,
  tierGroup: string | undefined,
  tierRank: number,
  archived = false
): CatalogProduct => ({
  id,
  key: id,
  display_name: id,
  description: "",
  tier_group: tierGroup,
  tier_rank: tierRank,
  archived,
  created_at: "2026-01-01T00:00:00Z",
  updated_at: "2026-01-01T00:00:00Z",
})

const price = (
  id: string,
  productId: string,
  currency = "usd",
  autoRenew = true,
  archived = false
): CatalogPrice => ({
  id,
  key: id,
  product_id: productId,
  archived,
  unit_amount: 10_000_000,
  currency,
  auto_renew: autoRenew,
  created_at: "2026-01-01T00:00:00Z",
  updated_at: "2026-01-01T00:00:00Z",
})

describe("tierChangeOptions", () => {
  it("offers only active recurring prices in the current group and currency", () => {
    const current = product("standard", "membership", 2)
    const products = [
      current,
      product("basic", "membership", 1),
      product("pro", "membership", 3),
      product("other", "storage", 4),
      product("archived", "membership", 5, true),
    ]
    const prices = [
      price("basic-usd", "basic"),
      price("pro-usd", "pro", "USD"),
      price("pro-eur", "pro", "eur"),
      price("pro-once", "pro", "usd", false),
      price("other-usd", "other"),
      price("archived-product", "archived"),
      price("archived-price", "pro", "usd", true, true),
    ]

    expect(
      tierChangeOptions({
        currentProduct: current,
        currentCurrency: "USD",
        products,
        prices,
      }).map(({ direction, price: candidate }) => [direction, candidate.id])
    ).toEqual([
      ["downgrade", "basic-usd"],
      ["upgrade", "pro-usd"],
    ])
  })

  it("fails closed when the current product has no tier group", () => {
    expect(
      tierChangeOptions({
        currentProduct: product("standalone", undefined, 0),
        currentCurrency: "usd",
        products: [product("other", undefined, 1)],
        prices: [price("other-usd", "other")],
      })
    ).toEqual([])
  })
})
