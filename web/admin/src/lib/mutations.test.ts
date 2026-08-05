import { QueryClient } from "@tanstack/react-query"
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

import { putMerchantSettings, putPaymentProvider } from "@/lib/api/endpoints"
import { adminMutations } from "@/lib/mutations"
import { queryKeys } from "@/lib/queries"

vi.mock("@/lib/api/endpoints", () => ({
  changeTeamRole: vi.fn(),
  createAlertRule: vi.fn(),
  createApiKey: vi.fn(),
  createWebhook: vi.fn(),
  deleteAlertRule: vi.fn(),
  deletePaymentProvider: vi.fn(),
  deleteWebhook: vi.fn(),
  getCreditLimit: vi.fn(),
  getTrustLevel: vi.fn(),
  inviteTeamMember: vi.fn(),
  putMerchantSettings: vi.fn(),
  putPaymentProvider: vi.fn(),
  removeTeamMember: vi.fn(),
  revokeApiKey: vi.fn(),
  revokeTeamInvite: vi.fn(),
  setCreditLimit: vi.fn(),
  testAlertRule: vi.fn(),
  updateAlertRule: vi.fn(),
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
