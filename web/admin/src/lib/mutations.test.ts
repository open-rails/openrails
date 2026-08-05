import { QueryClient } from "@tanstack/react-query"
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

import { askCatalogCopilot, confirmCopilotDraft } from "@/lib/api/copilot"
import {
  cancelReprice,
  cancelSubscription,
  changeSubscriptionPaymentMethod,
  createOffChannelPayment,
  createPrice,
  createProduct,
  deactivatePrice,
  getPriceByKey,
  getProduct,
  grantEntitlement,
  grantProductAccess,
  listCustomers,
  listPayments,
  listSubscriptions,
  markNotificationRead,
  previewRepriceAllPriorVersions,
  publishCatalog,
  putMerchantSettings,
  putPaymentProvider,
  refreshCatalogDrift,
  refundPayment,
  repriceAllPriorVersions,
  resolveFinding,
  resumeSubscription,
  revokeEntitlement,
  revokeProductAccess,
} from "@/lib/api/endpoints"
import { askMetrics, generateWidget, putDashboard } from "@/lib/api/metrics"
import { adminMutations } from "@/lib/mutations"
import { queryKeys } from "@/lib/queries"

vi.mock("@/lib/api/copilot", () => ({
  askCatalogCopilot: vi.fn(),
  confirmCopilotDraft: vi.fn(),
}))

vi.mock("@/lib/api/endpoints", () => ({
  activatePrice: vi.fn(),
  activateProduct: vi.fn(),
  cancelReprice: vi.fn(),
  cancelSubscription: vi.fn(),
  changeTeamRole: vi.fn(),
  changeSubscriptionPaymentMethod: vi.fn(),
  createAlertRule: vi.fn(),
  createApiKey: vi.fn(),
  createOffChannelPayment: vi.fn(),
  createPrice: vi.fn(),
  createProduct: vi.fn(),
  createWebhook: vi.fn(),
  deactivatePrice: vi.fn(),
  deactivateProduct: vi.fn(),
  deleteAlertRule: vi.fn(),
  deletePaymentProvider: vi.fn(),
  deleteWebhook: vi.fn(),
  getCreditLimit: vi.fn(),
  getPriceByKey: vi.fn(),
  getProduct: vi.fn(),
  getTrustLevel: vi.fn(),
  grantEntitlement: vi.fn(),
  grantProductAccess: vi.fn(),
  inviteTeamMember: vi.fn(),
  listCustomers: vi.fn(),
  listPayments: vi.fn(),
  listSubscriptions: vi.fn(),
  markNotificationRead: vi.fn(),
  putMerchantSettings: vi.fn(),
  putPaymentProvider: vi.fn(),
  previewRepriceAllPriorVersions: vi.fn(),
  publishCatalog: vi.fn(),
  removeTeamMember: vi.fn(),
  refreshCatalogDrift: vi.fn(),
  refundPayment: vi.fn(),
  repriceAllPriorVersions: vi.fn(),
  resolveFinding: vi.fn(),
  resumeSubscription: vi.fn(),
  revokeApiKey: vi.fn(),
  revokeEntitlement: vi.fn(),
  revokeProductAccess: vi.fn(),
  revokeTeamInvite: vi.fn(),
  setCreditLimit: vi.fn(),
  testAlertRule: vi.fn(),
  updateAlertRule: vi.fn(),
  updateProduct: vi.fn(),
}))

vi.mock("@/lib/api/metrics", () => ({
  askMetrics: vi.fn(),
  generateWidget: vi.fn(),
  putDashboard: vi.fn(),
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
  vi.mocked(cancelSubscription).mockResolvedValue({} as never)
  vi.mocked(changeSubscriptionPaymentMethod).mockResolvedValue({
    success: true,
    message: "ok",
  })
  vi.mocked(resumeSubscription).mockResolvedValue({ status: "queued" })
  vi.mocked(refundPayment).mockResolvedValue({} as never)
  vi.mocked(resolveFinding).mockResolvedValue({} as never)
  vi.mocked(createOffChannelPayment).mockResolvedValue({
    payment_id: "payment-1",
    status: "created",
  })
  vi.mocked(grantEntitlement).mockResolvedValue({} as never)
  vi.mocked(grantProductAccess).mockResolvedValue({} as never)
  vi.mocked(revokeEntitlement).mockResolvedValue({ message: "ok" })
  vi.mocked(revokeProductAccess).mockResolvedValue({ message: "ok" })
  vi.mocked(askCatalogCopilot).mockResolvedValue({
    answer: "The catalog has one product.",
    evidence: [],
  })
  vi.mocked(confirmCopilotDraft).mockResolvedValue({ message: "ok" })
  vi.mocked(getPriceByKey).mockResolvedValue({
    id: "price-1",
    product_id: "product-1",
  } as never)
  vi.mocked(getProduct).mockResolvedValue({
    id: "product-1",
    display_name: "Pro",
  } as never)
  vi.mocked(publishCatalog).mockResolvedValue({ plan: {} })
  vi.mocked(refreshCatalogDrift).mockResolvedValue({
    new_events: 2,
    resolved_events: 1,
  } as never)
})

afterEach(() => vi.unstubAllGlobals())

describe("notification mutations", () => {
  it("marks one notification read and reconciles both notification caches", async () => {
    const queryClient = new QueryClient()
    const notificationsKey = queryKeys.notifications()
    const unreadKey = [...notificationsKey, "unread-count"] as const
    queryClient.setQueryData(notificationsKey, {
      data: [
        { id: "notification-1", read_at: null },
        { id: "notification-2", read_at: null },
      ],
    })
    queryClient.setQueryData(unreadKey, { unread: 2 })
    vi.mocked(markNotificationRead).mockResolvedValueOnce({
      id: "notification-1",
      read: true,
    })

    await queryClient
      .getMutationCache()
      .build(queryClient, adminMutations.markNotificationRead(queryClient))
      .execute("notification-1")

    const notifications = queryClient.getQueryData<{
      data: Array<{ id: string; read_at: string | null }>
    }>(notificationsKey)
    expect(markNotificationRead).toHaveBeenCalledWith("notification-1")
    expect(notifications?.data[0].read_at).toEqual(expect.any(String))
    expect(notifications?.data[1].read_at).toBeNull()
    expect(queryClient.getQueryData(unreadKey)).toEqual({ unread: 1 })
  })

  it("updates only successfully read notifications in a partial bulk result", async () => {
    const queryClient = new QueryClient()
    const notificationsKey = queryKeys.notifications()
    const unreadKey = [...notificationsKey, "unread-count"] as const
    queryClient.setQueryData(notificationsKey, {
      data: [
        { id: "notification-1", read_at: null },
        { id: "notification-2", read_at: null },
      ],
    })
    queryClient.setQueryData(unreadKey, { unread: 2 })
    vi.mocked(markNotificationRead)
      .mockResolvedValueOnce({ id: "notification-1", read: true })
      .mockRejectedValueOnce(new Error("unavailable"))

    const readIds = await queryClient
      .getMutationCache()
      .build(queryClient, adminMutations.markNotificationsRead(queryClient))
      .execute(["notification-1", "notification-2"])

    const notifications = queryClient.getQueryData<{
      data: Array<{ id: string; read_at: string | null }>
    }>(notificationsKey)
    expect(readIds).toEqual(["notification-1"])
    expect(notifications?.data[0].read_at).toEqual(expect.any(String))
    expect(notifications?.data[1].read_at).toBeNull()
    expect(queryClient.getQueryData(unreadKey)).toEqual({ unread: 1 })
  })
})

describe("dashboard AI mutations", () => {
  it("routes metrics questions through the ask endpoint", async () => {
    const queryClient = new QueryClient()
    vi.mocked(askMetrics).mockResolvedValueOnce({
      answer: "Revenue increased.",
      evidence: [],
    })

    const result = await queryClient
      .getMutationCache()
      .build(queryClient, adminMutations.askMetrics())
      .execute("How did revenue change?")

    expect(result.answer).toBe("Revenue increased.")
    expect(askMetrics).toHaveBeenCalledWith("How did revenue change?")
  })

  it("generates a widget from a prompt and optional base query", async () => {
    const queryClient = new QueryClient()
    const baseQuery = {
      measures: ["revenue"],
      range: { last: "30d" },
    }
    vi.mocked(generateWidget).mockResolvedValueOnce({
      title: "Weekly revenue",
      viz: "line",
      query: baseQuery,
    })

    const result = await queryClient
      .getMutationCache()
      .build(queryClient, adminMutations.generateDashboardWidget())
      .execute({ prompt: "Make it weekly", baseQuery })

    expect(result.title).toBe("Weekly revenue")
    expect(generateWidget).toHaveBeenCalledWith("Make it weekly", baseQuery)
  })
})

describe("dashboard persistence mutation", () => {
  it("saves widgets and updates only the initiating merchant dashboard", async () => {
    const queryClient = new QueryClient()
    sessionStorage.setItem(
      "openrails.admin.tokens",
      JSON.stringify({ access_token: "token", merchant: "merchant-a" })
    )
    const dashboardAKey = queryKeys.dashboard()
    const options = adminMutations.saveDashboard(queryClient)
    queryClient.setQueryData(dashboardAKey, { widgets: [] })

    sessionStorage.setItem(
      "openrails.admin.tokens",
      JSON.stringify({ access_token: "token", merchant: "merchant-b" })
    )
    const dashboardBKey = queryKeys.dashboard()
    queryClient.setQueryData(dashboardBKey, { widgets: [] })

    const widgets = [
      {
        id: "widget-1",
        title: "Revenue",
        viz: "stat" as const,
        query: { measures: ["revenue"], range: { last: "30d" } },
        grid: { x: 0, y: 0, w: 3, h: 2 },
      },
    ]
    const saved = { widgets, is_default: false }
    vi.mocked(putDashboard).mockResolvedValueOnce(saved)

    await queryClient
      .getMutationCache()
      .build(queryClient, options)
      .execute(widgets)

    expect(putDashboard).toHaveBeenCalledWith(widgets)
    expect(queryClient.getQueryData(dashboardAKey)).toEqual(saved)
    expect(queryClient.getQueryData(dashboardBKey)).toEqual({ widgets: [] })
  })
})

describe("customer lookup mutation", () => {
  it("returns the first customer matching the submitted term", async () => {
    const queryClient = new QueryClient()
    vi.mocked(listCustomers).mockResolvedValueOnce({
      data: [{ id: "customer-1", email: "alice@example.com" }],
      total: 1,
    } as never)

    const customer = await queryClient
      .getMutationCache()
      .build(queryClient, adminMutations.findCustomer())
      .execute("alice@example.com")

    expect(customer).toEqual({
      id: "customer-1",
      email: "alice@example.com",
    })
    expect(listCustomers).toHaveBeenCalledWith("alice@example.com", 1, 0)
  })
})

describe("list export mutations", () => {
  it("exports every customer page for the current search", async () => {
    const queryClient = new QueryClient()
    vi.mocked(listCustomers)
      .mockResolvedValueOnce({
        data: [{ id: "customer-1" }],
        total: 2,
      } as never)
      .mockResolvedValueOnce({
        data: [{ id: "customer-2" }],
        total: 2,
      } as never)

    const rows = await queryClient
      .getMutationCache()
      .build(queryClient, adminMutations.exportCustomers())
      .execute("alice")

    expect(rows).toEqual([{ id: "customer-1" }, { id: "customer-2" }])
    expect(listCustomers).toHaveBeenNthCalledWith(1, "alice", 200, 0)
    expect(listCustomers).toHaveBeenNthCalledWith(2, "alice", 200, 200)
  })

  it("exports every subscription page with the active filters", async () => {
    const queryClient = new QueryClient()
    const filters = { status: "past_due", rail: "nmi" }
    vi.mocked(listSubscriptions)
      .mockResolvedValueOnce({
        data: [{ id: "subscription-1" }],
        total: 2,
      } as never)
      .mockResolvedValueOnce({
        data: [{ id: "subscription-2" }],
        total: 2,
      } as never)

    const rows = await queryClient
      .getMutationCache()
      .build(queryClient, adminMutations.exportSubscriptions())
      .execute(filters)

    expect(rows).toEqual([{ id: "subscription-1" }, { id: "subscription-2" }])
    expect(listSubscriptions).toHaveBeenNthCalledWith(1, filters, 200, 0)
    expect(listSubscriptions).toHaveBeenNthCalledWith(2, filters, 200, 200)
  })

  it("stops payment export when the API returns an empty page", async () => {
    const queryClient = new QueryClient()
    const filters = { refunds_only: true, user_id: "customer-1" }
    vi.mocked(listPayments)
      .mockResolvedValueOnce({
        data: [{ id: "payment-1" }],
        total: 2,
      } as never)
      .mockResolvedValueOnce({ data: [], total: 2 } as never)

    const rows = await queryClient
      .getMutationCache()
      .build(queryClient, adminMutations.exportPayments())
      .execute(filters)

    expect(rows).toEqual([{ id: "payment-1" }])
    expect(listPayments).toHaveBeenNthCalledWith(1, filters, 200, 0)
    expect(listPayments).toHaveBeenNthCalledWith(2, filters, 200, 200)
  })
})

describe("ops mutations", () => {
  it("resolves a finding and refreshes the ops tree", async () => {
    const queryClient = new QueryClient()
    const findingsKey = [
      ...queryKeys.ops(),
      "findings",
      { limit: 100, offset: 0 },
    ] as const
    const repairAlertsKey = [
      ...queryKeys.ops(),
      "repair-alerts",
      { limit: 50, offset: 0 },
    ] as const
    const customerKey = queryKeys.customer("customer-1")
    queryClient.setQueryData(findingsKey, { items: [] })
    queryClient.setQueryData(repairAlertsKey, { data: [] })
    queryClient.setQueryData(customerKey, {})

    await queryClient
      .getMutationCache()
      .build(queryClient, adminMutations.resolveFinding(queryClient))
      .execute({
        id: "finding-1",
        outcome: "approve",
        notes: "verified",
      })

    expect(resolveFinding).toHaveBeenCalledWith(
      "finding-1",
      "approve",
      "verified"
    )
    expect(queryClient.getQueryState(findingsKey)?.isInvalidated).toBe(true)
    expect(queryClient.getQueryState(repairAlertsKey)?.isInvalidated).toBe(true)
    expect(queryClient.getQueryState(customerKey)?.isInvalidated).toBe(false)
  })

  it("keeps ops invalidation scoped to the initiating merchant", async () => {
    const queryClient = new QueryClient()
    sessionStorage.setItem(
      "openrails.admin.tokens",
      JSON.stringify({ access_token: "token", merchant: "merchant-a" })
    )
    const opsAKey = queryKeys.ops()
    const options = adminMutations.resolveFinding(queryClient)
    queryClient.setQueryData(opsAKey, {})

    sessionStorage.setItem(
      "openrails.admin.tokens",
      JSON.stringify({ access_token: "token", merchant: "merchant-b" })
    )
    const opsBKey = queryKeys.ops()
    queryClient.setQueryData(opsBKey, {})

    await queryClient
      .getMutationCache()
      .build(queryClient, options)
      .execute({ id: "finding-1", outcome: "ignore", notes: "expected" })

    expect(queryClient.getQueryState(opsAKey)?.isInvalidated).toBe(true)
    expect(queryClient.getQueryState(opsBKey)?.isInvalidated).toBe(false)
  })
})

describe("payment mutations", () => {
  it("refunds a payment and refreshes its related records", async () => {
    const queryClient = new QueryClient()
    const paymentKey = queryKeys.payment("payment-1")
    const paymentListKey = [
      ...queryKeys.payments(),
      { filters: {}, limit: 50, offset: 0 },
    ] as const
    const customerKey = queryKeys.customer("customer-1")
    const subscriptionKey = queryKeys.subscription("subscription-1")
    const catalogKey = queryKeys.catalog()
    queryClient.setQueryData(paymentKey, { id: "payment-1" })
    queryClient.setQueryData(paymentListKey, { data: [] })
    queryClient.setQueryData(customerKey, { customer_id: "customer-1" })
    queryClient.setQueryData(subscriptionKey, { id: "subscription-1" })
    queryClient.setQueryData(catalogKey, { items: [] })

    await queryClient
      .getMutationCache()
      .build(
        queryClient,
        adminMutations.refundPayment(
          queryClient,
          "payment-1",
          "customer-1",
          "subscription-1"
        )
      )
      .execute({
        amount: 5_000_000,
        reason: "requested",
        revokeAccess: true,
      })

    expect(refundPayment).toHaveBeenCalledWith(
      "payment-1",
      5_000_000,
      "requested",
      true
    )
    expect(queryClient.getQueryState(paymentKey)?.isInvalidated).toBe(true)
    expect(queryClient.getQueryState(paymentListKey)?.isInvalidated).toBe(true)
    expect(queryClient.getQueryState(customerKey)?.isInvalidated).toBe(true)
    expect(queryClient.getQueryState(subscriptionKey)?.isInvalidated).toBe(true)
    expect(queryClient.getQueryState(catalogKey)?.isInvalidated).toBe(false)
  })

  it("keeps refund invalidation scoped to the initiating merchant", async () => {
    const queryClient = new QueryClient()
    sessionStorage.setItem(
      "openrails.admin.tokens",
      JSON.stringify({ access_token: "token", merchant: "merchant-a" })
    )
    const paymentAKey = queryKeys.payment("payment-1")
    const customerAKey = queryKeys.customer("customer-1")
    const subscriptionAKey = queryKeys.subscription("subscription-1")
    const options = adminMutations.refundPayment(
      queryClient,
      "payment-1",
      "customer-1",
      "subscription-1"
    )
    queryClient.setQueryData(paymentAKey, {})
    queryClient.setQueryData(customerAKey, {})
    queryClient.setQueryData(subscriptionAKey, {})

    sessionStorage.setItem(
      "openrails.admin.tokens",
      JSON.stringify({ access_token: "token", merchant: "merchant-b" })
    )
    const paymentBKey = queryKeys.payment("payment-1")
    const customerBKey = queryKeys.customer("customer-1")
    const subscriptionBKey = queryKeys.subscription("subscription-1")
    queryClient.setQueryData(paymentBKey, {})
    queryClient.setQueryData(customerBKey, {})
    queryClient.setQueryData(subscriptionBKey, {})

    await queryClient
      .getMutationCache()
      .build(queryClient, options)
      .execute({ amount: 5_000_000, reason: "", revokeAccess: false })

    expect(queryClient.getQueryState(paymentAKey)?.isInvalidated).toBe(true)
    expect(queryClient.getQueryState(customerAKey)?.isInvalidated).toBe(true)
    expect(queryClient.getQueryState(subscriptionAKey)?.isInvalidated).toBe(
      true
    )
    expect(queryClient.getQueryState(paymentBKey)?.isInvalidated).toBe(false)
    expect(queryClient.getQueryState(customerBKey)?.isInvalidated).toBe(false)
    expect(queryClient.getQueryState(subscriptionBKey)?.isInvalidated).toBe(
      false
    )
  })
})

describe("subscription mutations", () => {
  it("cancels a subscription and refreshes subscription and customer data", async () => {
    const queryClient = new QueryClient()
    const subscriptionKey = queryKeys.subscription("subscription-1")
    const customerKey = queryKeys.customer("customer-1")
    const paymentsKey = queryKeys.payments()
    queryClient.setQueryData(subscriptionKey, { id: "subscription-1" })
    queryClient.setQueryData(customerKey, { customer_id: "customer-1" })
    queryClient.setQueryData(paymentsKey, { data: [] })

    await queryClient
      .getMutationCache()
      .build(
        queryClient,
        adminMutations.cancelSubscription(
          queryClient,
          "subscription-1",
          "customer-1"
        )
      )
      .execute({ reason: "requested", revokeAccess: true })

    expect(cancelSubscription).toHaveBeenCalledWith(
      "subscription-1",
      "requested",
      true
    )
    expect(queryClient.getQueryState(subscriptionKey)?.isInvalidated).toBe(true)
    expect(queryClient.getQueryState(customerKey)?.isInvalidated).toBe(true)
    expect(queryClient.getQueryState(paymentsKey)?.isInvalidated).toBe(false)
  })

  it("routes resume and payment-method changes through their endpoints", async () => {
    const queryClient = new QueryClient()

    await queryClient
      .getMutationCache()
      .build(
        queryClient,
        adminMutations.resumeSubscription(
          queryClient,
          "subscription-1",
          "customer-1"
        )
      )
      .execute()
    await queryClient
      .getMutationCache()
      .build(
        queryClient,
        adminMutations.changeSubscriptionPaymentMethod(
          queryClient,
          "subscription-1",
          "customer-1"
        )
      )
      .execute("payment-method-1")

    expect(resumeSubscription).toHaveBeenCalledWith("subscription-1")
    expect(changeSubscriptionPaymentMethod).toHaveBeenCalledWith(
      "subscription-1",
      "payment-method-1"
    )
  })

  it("cancels a scheduled reprice and refreshes subscription and catalog data", async () => {
    const queryClient = new QueryClient()
    const scheduledKey = [
      ...queryKeys.subscription("subscription-1"),
      "reprices",
      "scheduled",
    ] as const
    const catalogRepricesKey = [...queryKeys.catalog(), "reprices"] as const
    const paymentsKey = queryKeys.payments()
    queryClient.setQueryData(scheduledKey, { items: [] })
    queryClient.setQueryData(catalogRepricesKey, { items: [] })
    queryClient.setQueryData(paymentsKey, { data: [] })

    await queryClient
      .getMutationCache()
      .build(
        queryClient,
        adminMutations.cancelSubscriptionReprice(queryClient, "subscription-1")
      )
      .execute("reprice-1")

    expect(cancelReprice).toHaveBeenCalledWith("reprice-1")
    expect(queryClient.getQueryState(scheduledKey)?.isInvalidated).toBe(true)
    expect(queryClient.getQueryState(catalogRepricesKey)?.isInvalidated).toBe(
      true
    )
    expect(queryClient.getQueryState(paymentsKey)?.isInvalidated).toBe(false)
  })

  it("keeps subscription invalidation scoped to the initiating merchant", async () => {
    const queryClient = new QueryClient()
    sessionStorage.setItem(
      "openrails.admin.tokens",
      JSON.stringify({ access_token: "token", merchant: "merchant-a" })
    )
    const subscriptionAKey = queryKeys.subscription("subscription-1")
    const customerAKey = queryKeys.customer("customer-1")
    const options = adminMutations.cancelSubscription(
      queryClient,
      "subscription-1",
      "customer-1"
    )
    queryClient.setQueryData(subscriptionAKey, {})
    queryClient.setQueryData(customerAKey, {})

    sessionStorage.setItem(
      "openrails.admin.tokens",
      JSON.stringify({ access_token: "token", merchant: "merchant-b" })
    )
    const subscriptionBKey = queryKeys.subscription("subscription-1")
    const customerBKey = queryKeys.customer("customer-1")
    queryClient.setQueryData(subscriptionBKey, {})
    queryClient.setQueryData(customerBKey, {})

    await queryClient
      .getMutationCache()
      .build(queryClient, options)
      .execute({ reason: "requested", revokeAccess: false })

    expect(queryClient.getQueryState(subscriptionAKey)?.isInvalidated).toBe(
      true
    )
    expect(queryClient.getQueryState(customerAKey)?.isInvalidated).toBe(true)
    expect(queryClient.getQueryState(subscriptionBKey)?.isInvalidated).toBe(
      false
    )
    expect(queryClient.getQueryState(customerBKey)?.isInvalidated).toBe(false)
  })
})

describe("customer mutations", () => {
  it("changes entitlements and refreshes only that customer tree", async () => {
    const queryClient = new QueryClient()
    const customerKey = queryKeys.customer("customer-1")
    const paymentMethodsKey = [...customerKey, "payment-methods"] as const
    const customerListKey = [
      ...queryKeys.customers(),
      { q: "", limit: 50, offset: 0 },
    ] as const
    queryClient.setQueryData(customerKey, { customer_id: "customer-1" })
    queryClient.setQueryData(paymentMethodsKey, { data: [] })
    queryClient.setQueryData(customerListKey, { data: [] })

    await queryClient
      .getMutationCache()
      .build(
        queryClient,
        adminMutations.grantCustomerEntitlement(queryClient, "customer-1")
      )
      .execute({ entitlement: "premium", hours: 48 })
    await queryClient
      .getMutationCache()
      .build(
        queryClient,
        adminMutations.revokeCustomerEntitlement(queryClient, "customer-1")
      )
      .execute("entitlement-1")

    expect(grantEntitlement).toHaveBeenCalledWith("customer-1", "premium", 48)
    expect(revokeEntitlement).toHaveBeenCalledWith(
      "customer-1",
      "entitlement-1"
    )
    expect(queryClient.getQueryState(customerKey)?.isInvalidated).toBe(true)
    expect(queryClient.getQueryState(paymentMethodsKey)?.isInvalidated).toBe(
      true
    )
    expect(queryClient.getQueryState(customerListKey)?.isInvalidated).toBe(
      false
    )
  })

  it("routes product access grants and revocations through their endpoints", async () => {
    const queryClient = new QueryClient()

    await queryClient
      .getMutationCache()
      .build(
        queryClient,
        adminMutations.grantCustomerProductAccess(queryClient, "customer-1")
      )
      .execute({
        productId: "product-1",
        endsAt: "2026-09-05T00:00:00.000Z",
      })
    await queryClient
      .getMutationCache()
      .build(
        queryClient,
        adminMutations.revokeCustomerProductAccess(queryClient, "customer-1")
      )
      .execute("grant-1")

    expect(grantProductAccess).toHaveBeenCalledWith(
      "customer-1",
      "product-1",
      "2026-09-05T00:00:00.000Z"
    )
    expect(revokeProductAccess).toHaveBeenCalledWith("customer-1", "grant-1")
  })

  it("records off-channel payments and refreshes customer and payment data", async () => {
    const queryClient = new QueryClient()
    const customerKey = queryKeys.customer("customer-1")
    const paymentsKey = [
      ...queryKeys.payments(),
      { filters: {}, limit: 50, offset: 0 },
    ] as const
    const subscriptionsKey = queryKeys.subscriptions()
    queryClient.setQueryData(customerKey, { customer_id: "customer-1" })
    queryClient.setQueryData(paymentsKey, { data: [] })
    queryClient.setQueryData(subscriptionsKey, { data: [] })
    const payment = {
      price_id: "price-1",
      transaction_id: "external-1",
      amount: 12_500_000,
    }

    await queryClient
      .getMutationCache()
      .build(
        queryClient,
        adminMutations.recordCustomerOffChannelPayment(
          queryClient,
          "customer-1"
        )
      )
      .execute(payment)

    expect(createOffChannelPayment).toHaveBeenCalledWith("customer-1", payment)
    expect(queryClient.getQueryState(customerKey)?.isInvalidated).toBe(true)
    expect(queryClient.getQueryState(paymentsKey)?.isInvalidated).toBe(true)
    expect(queryClient.getQueryState(subscriptionsKey)?.isInvalidated).toBe(
      false
    )
  })

  it("keeps customer invalidation scoped to the merchant that started it", async () => {
    const queryClient = new QueryClient()
    sessionStorage.setItem(
      "openrails.admin.tokens",
      JSON.stringify({ access_token: "token", merchant: "merchant-a" })
    )
    const customerAKey = queryKeys.customer("customer-1")
    const paymentsAKey = queryKeys.payments()
    const options = adminMutations.recordCustomerOffChannelPayment(
      queryClient,
      "customer-1"
    )
    queryClient.setQueryData(customerAKey, {})
    queryClient.setQueryData(paymentsAKey, {})

    sessionStorage.setItem(
      "openrails.admin.tokens",
      JSON.stringify({ access_token: "token", merchant: "merchant-b" })
    )
    const customerBKey = queryKeys.customer("customer-1")
    const paymentsBKey = queryKeys.payments()
    queryClient.setQueryData(customerBKey, {})
    queryClient.setQueryData(paymentsBKey, {})

    await queryClient
      .getMutationCache()
      .build(queryClient, options)
      .execute({ price_id: "price-1", transaction_id: "external-1" })

    expect(queryClient.getQueryState(customerAKey)?.isInvalidated).toBe(true)
    expect(queryClient.getQueryState(paymentsAKey)?.isInvalidated).toBe(true)
    expect(queryClient.getQueryState(customerBKey)?.isInvalidated).toBe(false)
    expect(queryClient.getQueryState(paymentsBKey)?.isInvalidated).toBe(false)
  })
})

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
  it("asks the catalog copilot without invalidating catalog data", async () => {
    const queryClient = new QueryClient()
    const catalogKey = queryKeys.catalog()
    queryClient.setQueryData(catalogKey, { items: [] })

    const result = await queryClient
      .getMutationCache()
      .build(queryClient, adminMutations.askCatalogCopilot())
      .execute("what do we sell?")

    expect(askCatalogCopilot).toHaveBeenCalledWith("what do we sell?")
    expect(result.answer).toBe("The catalog has one product.")
    expect(queryClient.getQueryState(catalogKey)?.isInvalidated).toBe(false)
  })

  it("loads the live price and product for a copilot draft", async () => {
    const queryClient = new QueryClient()

    const result = await queryClient
      .getMutationCache()
      .build(queryClient, adminMutations.loadCatalogPriceDraft())
      .execute("pro-monthly")

    expect(getPriceByKey).toHaveBeenCalledWith("pro-monthly")
    expect(getProduct).toHaveBeenCalledWith("product-1")
    expect(result.productName).toBe("Pro")
  })

  it("publishes an applied manifest and invalidates the catalog", async () => {
    const queryClient = new QueryClient()
    const catalogKey = queryKeys.catalog()
    queryClient.setQueryData(catalogKey, { items: [] })
    const manifest = { products: [] }

    await queryClient
      .getMutationCache()
      .build(queryClient, adminMutations.publishCatalog(queryClient))
      .execute({ manifest, planOnly: false })

    expect(publishCatalog).toHaveBeenCalledWith(manifest, {
      insert: true,
      overwrite: true,
    })
    expect(queryClient.getQueryState(catalogKey)?.isInvalidated).toBe(true)
  })

  it("previews a manifest without invalidating the catalog", async () => {
    const queryClient = new QueryClient()
    const catalogKey = queryKeys.catalog()
    queryClient.setQueryData(catalogKey, { items: [] })
    const manifest = { products: [] }

    await queryClient
      .getMutationCache()
      .build(queryClient, adminMutations.publishCatalog(queryClient))
      .execute({ manifest, planOnly: true })

    expect(publishCatalog).toHaveBeenCalledWith(manifest, { plan_only: true })
    expect(queryClient.getQueryState(catalogKey)?.isInvalidated).toBe(false)
  })

  it("invalidates the merchant that started a catalog publish", async () => {
    const queryClient = new QueryClient()
    sessionStorage.setItem(
      "openrails.admin.tokens",
      JSON.stringify({ access_token: "token", merchant: "merchant-a" })
    )
    const merchantAKey = queryKeys.catalog()
    const options = adminMutations.publishCatalog(queryClient)
    queryClient.setQueryData(merchantAKey, { items: [] })

    sessionStorage.setItem(
      "openrails.admin.tokens",
      JSON.stringify({ access_token: "token", merchant: "merchant-b" })
    )
    const merchantBKey = queryKeys.catalog()
    queryClient.setQueryData(merchantBKey, { items: [] })

    await queryClient
      .getMutationCache()
      .build(queryClient, options)
      .execute({ manifest: { products: [] }, planOnly: false })

    expect(queryClient.getQueryState(merchantAKey)?.isInvalidated).toBe(true)
    expect(queryClient.getQueryState(merchantBKey)?.isInvalidated).toBe(false)
  })

  it("refreshes only drift queries after a scan", async () => {
    const queryClient = new QueryClient()
    const driftKey = [...queryKeys.catalogDrift(), { limit: 200 }] as const
    const productsKey = [...queryKeys.catalog(), "products"] as const
    queryClient.setQueryData(driftKey, { items: [] })
    queryClient.setQueryData(productsKey, { items: [] })

    await queryClient
      .getMutationCache()
      .build(queryClient, adminMutations.refreshCatalogDrift(queryClient))
      .execute()

    expect(refreshCatalogDrift).toHaveBeenCalledOnce()
    expect(queryClient.getQueryState(driftKey)?.isInvalidated).toBe(true)
    expect(queryClient.getQueryState(productsKey)?.isInvalidated).toBe(false)
  })

  it("creates a copilot-drafted price and records its provenance", async () => {
    const queryClient = new QueryClient()
    const catalogKey = queryKeys.catalog()
    queryClient.setQueryData(catalogKey, { items: [] })
    const price = {
      product_id: "product-1",
      key: "pro-monthly",
      unit_amount: 12_000_000,
      currency: "usd",
      auto_renew: true,
    }

    await queryClient
      .getMutationCache()
      .build(queryClient, adminMutations.createCatalogDraftPrice(queryClient))
      .execute({ draftId: "draft-1", price })

    expect(createPrice).toHaveBeenCalledWith(price)
    expect(confirmCopilotDraft).toHaveBeenCalledWith(
      "draft-1",
      "catalog_diff",
      "pro-monthly"
    )
    expect(queryClient.getQueryState(catalogKey)?.isInvalidated).toBe(true)
  })

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
        copilotDraftId: "draft-1",
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
    expect(confirmCopilotDraft).toHaveBeenCalledWith(
      "draft-1",
      "price_change",
      "pro-monthly"
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
          copilotDraftId: "draft-1",
          migration: {
            priceKey: "pro-monthly",
            effectiveAt: "2026-09-05T00:00:00.000Z",
          },
        })
    ).rejects.toThrow("schedule failed")

    expect(confirmCopilotDraft).not.toHaveBeenCalled()
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
