import { QueryClient } from "@tanstack/react-query"
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

import {
  cancelReprice,
  createPrice,
  createProduct,
  deactivatePrice,
  previewRepriceAllPriorVersions,
  putMerchantSettings,
  putPaymentProvider,
  repriceAllPriorVersions,
} from "@/lib/api/endpoints"
import { adminMutations } from "@/lib/mutations"
import { queryKeys } from "@/lib/queries"

vi.mock("@/lib/api/endpoints", () => ({
  activatePrice: vi.fn(),
  activateProduct: vi.fn(),
  cancelReprice: vi.fn(),
  changeTeamRole: vi.fn(),
  createAlertRule: vi.fn(),
  createApiKey: vi.fn(),
  createPrice: vi.fn(),
  createProduct: vi.fn(),
  createWebhook: vi.fn(),
  deactivatePrice: vi.fn(),
  deactivateProduct: vi.fn(),
  deleteAlertRule: vi.fn(),
  deletePaymentProvider: vi.fn(),
  deleteWebhook: vi.fn(),
  getCreditLimit: vi.fn(),
  getTrustLevel: vi.fn(),
  inviteTeamMember: vi.fn(),
  putMerchantSettings: vi.fn(),
  putPaymentProvider: vi.fn(),
  previewRepriceAllPriorVersions: vi.fn(),
  removeTeamMember: vi.fn(),
  repriceAllPriorVersions: vi.fn(),
  revokeApiKey: vi.fn(),
  revokeTeamInvite: vi.fn(),
  setCreditLimit: vi.fn(),
  testAlertRule: vi.fn(),
  updateAlertRule: vi.fn(),
  updateProduct: vi.fn(),
}))

const storage = (): Storage => {
  const values = new Map<string, string>()
  return {
    get length() {
      return values.size
    },
    clear: () => values.clear(),
    getItem: (key) => values.get(key) ?? null,
    key: (index) => [...values.keys()][index] ?? null,
    removeItem: (key) => values.delete(key),
    setItem: (key, value) => values.set(key, value),
  }
}

beforeEach(() => {
  vi.stubGlobal("localStorage", storage())
  vi.stubGlobal("sessionStorage", storage())
  vi.clearAllMocks()
  vi.mocked(putMerchantSettings).mockResolvedValue({ message: "ok" })
  vi.mocked(putPaymentProvider).mockResolvedValue({
    payment_provider: {} as never,
  })
  vi.mocked(createProduct).mockResolvedValue({} as never)
  vi.mocked(createPrice).mockResolvedValue({} as never)
  vi.mocked(deactivatePrice).mockResolvedValue({} as never)
  vi.mocked(previewRepriceAllPriorVersions).mockResolvedValue({
    matched: 3,
  } as never)
  vi.mocked(repriceAllPriorVersions).mockResolvedValue({} as never)
  vi.mocked(cancelReprice).mockResolvedValue({ message: "ok" })
})

afterEach(() => vi.unstubAllGlobals())

describe("settings mutations", () => {
  it("updates merchant settings and invalidates only their base query", async () => {
    const queryClient = new QueryClient()
    const settingsKey = queryKeys.settings()
    const providersKey = [...settingsKey, "payment-providers"] as const
    queryClient.setQueryData(settingsKey, { profile: {} })
    queryClient.setQueryData(providersKey, { data: [] })

    const settings = { profile: { display_name: "Acme" } }
    await queryClient
      .getMutationCache()
      .build(queryClient, adminMutations.updateMerchantSettings(queryClient))
      .execute(settings)

    expect(putMerchantSettings).toHaveBeenCalledWith(settings)
    expect(queryClient.getQueryState(settingsKey)?.isInvalidated).toBe(true)
    expect(queryClient.getQueryState(providersKey)?.isInvalidated).toBe(false)
  })

  it("invalidates the provider list after saving credentials", async () => {
    const queryClient = new QueryClient()
    const providersKey = [...queryKeys.settings(), "payment-providers"] as const
    queryClient.setQueryData(providersKey, { data: [] })

    const provider = {
      account_id: "gateway-1",
      credentials: { security_key: "secret" },
    }
    await queryClient
      .getMutationCache()
      .build(queryClient, adminMutations.savePaymentProvider(queryClient))
      .execute({ rail: "nmi", provider })

    expect(putPaymentProvider).toHaveBeenCalledWith("nmi", provider)
    expect(queryClient.getQueryState(providersKey)?.isInvalidated).toBe(true)
  })

  it("keeps invalidation scoped to the merchant that started the request", async () => {
    const queryClient = new QueryClient()
    sessionStorage.setItem(
      "openrails.admin.tokens",
      JSON.stringify({ access_token: "token", merchant: "merchant-a" })
    )
    const merchantAKey = queryKeys.settings()
    const options = adminMutations.updateMerchantSettings(queryClient)
    queryClient.setQueryData(merchantAKey, { profile: {} })

    sessionStorage.setItem(
      "openrails.admin.tokens",
      JSON.stringify({ access_token: "token", merchant: "merchant-b" })
    )
    const merchantBKey = queryKeys.settings()
    queryClient.setQueryData(merchantBKey, { profile: {} })

    await queryClient
      .getMutationCache()
      .build(queryClient, options)
      .execute({ profile: { display_name: "Acme" } })

    expect(queryClient.getQueryState(merchantAKey)?.isInvalidated).toBe(true)
    expect(queryClient.getQueryState(merchantBKey)?.isInvalidated).toBe(false)
  })
})

describe("catalog mutations", () => {
  it("creates a product and invalidates the catalog tree", async () => {
    const queryClient = new QueryClient()
    const productsKey = [
      ...queryKeys.catalog(),
      "products",
      { limit: 1000 },
    ] as const
    const pricesKey = [...queryKeys.catalog(), "prices"] as const
    queryClient.setQueryData(productsKey, { items: [] })
    queryClient.setQueryData(pricesKey, { items: [] })

    const product = {
      key: "pro",
      display_name: "Pro",
      description: "Pro plan",
    }
    await queryClient
      .getMutationCache()
      .build(queryClient, adminMutations.createProduct(queryClient))
      .execute(product)

    expect(createProduct).toHaveBeenCalledWith(product)
    expect(queryClient.getQueryState(productsKey)?.isInvalidated).toBe(true)
    expect(queryClient.getQueryState(pricesKey)?.isInvalidated).toBe(true)
  })

  it("deactivates a price through the active-state mutation", async () => {
    const queryClient = new QueryClient()
    const pricesKey = [...queryKeys.catalog(), "prices"] as const
    queryClient.setQueryData(pricesKey, { items: [] })

    await queryClient
      .getMutationCache()
      .build(queryClient, adminMutations.setPriceActive(queryClient))
      .execute({ id: "price-1", active: false })

    expect(deactivatePrice).toHaveBeenCalledWith("price-1")
    expect(queryClient.getQueryState(pricesKey)?.isInvalidated).toBe(true)
  })

  it("previews affected subscribers without writing catalog state", async () => {
    const queryClient = new QueryClient()

    const result = await queryClient
      .getMutationCache()
      .build(queryClient, adminMutations.previewPriceChange())
      .execute("pro-monthly")

    expect(previewRepriceAllPriorVersions).toHaveBeenCalledWith("pro-monthly")
    expect(result).toEqual({ matched: 3 })
  })

  it("creates a replacement price before scheduling its migration", async () => {
    const queryClient = new QueryClient()
    const catalogKey = queryKeys.catalog()
    queryClient.setQueryData(catalogKey, { items: [] })
    const price = {
      product_id: "product-1",
      unit_amount: 20_000_000,
      currency: "usd",
      access_duration_hours: 720,
      auto_renew: true,
      key: "pro-monthly",
    }

    await queryClient
      .getMutationCache()
      .build(queryClient, adminMutations.changePrice(queryClient))
      .execute({
        price,
        migration: {
          priceKey: "pro-monthly",
          effectiveAt: "2026-09-05T00:00:00.000Z",
        },
      })

    expect(createPrice).toHaveBeenCalledWith(price)
    expect(repriceAllPriorVersions).toHaveBeenCalledWith(
      "pro-monthly",
      "2026-09-05T00:00:00.000Z"
    )
    expect(vi.mocked(createPrice).mock.invocationCallOrder[0]).toBeLessThan(
      vi.mocked(repriceAllPriorVersions).mock.invocationCallOrder[0]
    )
    expect(queryClient.getQueryState(catalogKey)?.isInvalidated).toBe(true)
  })

  it("refreshes catalog state when scheduling fails after price creation", async () => {
    const queryClient = new QueryClient()
    const catalogKey = queryKeys.catalog()
    queryClient.setQueryData(catalogKey, { items: [] })
    vi.mocked(repriceAllPriorVersions).mockRejectedValueOnce(
      new Error("schedule failed")
    )

    await expect(
      queryClient
        .getMutationCache()
        .build(queryClient, adminMutations.changePrice(queryClient))
        .execute({
          price: {
            product_id: "product-1",
            unit_amount: 20_000_000,
            currency: "usd",
            access_duration_hours: 720,
            auto_renew: true,
            key: "pro-monthly",
          },
          migration: {
            priceKey: "pro-monthly",
            effectiveAt: "2026-09-05T00:00:00.000Z",
          },
        })
    ).rejects.toThrow("schedule failed")

    expect(queryClient.getQueryState(catalogKey)?.isInvalidated).toBe(true)
  })

  it("cancels all pending reprices and refreshes the catalog", async () => {
    const queryClient = new QueryClient()
    const repricesKey = [...queryKeys.catalog(), "reprices"] as const
    queryClient.setQueryData(repricesKey, { items: [] })

    await queryClient
      .getMutationCache()
      .build(queryClient, adminMutations.cancelReprices(queryClient))
      .execute(["reprice-1", "reprice-2"])

    expect(cancelReprice).toHaveBeenCalledTimes(2)
    expect(cancelReprice).toHaveBeenCalledWith("reprice-1")
    expect(cancelReprice).toHaveBeenCalledWith("reprice-2")
    expect(queryClient.getQueryState(repricesKey)?.isInvalidated).toBe(true)
  })
})
