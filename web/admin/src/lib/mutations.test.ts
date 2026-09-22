// Every console write, driven through the real endpoint + fetch path. Each
// row states the requests it issues and its exact cache blast radius, and is
// replayed with the merchant switched mid-flight to prove the refresh follows
// the merchant that started the write. Request bodies are asserted where they
// carry money or authority.
import type { MutationOptions, QueryClient } from "@tanstack/react-query"
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

import { adminMutations as M } from "@/lib/mutations"
import { queryKeys } from "@/lib/queries"
import {
  calls, client, exec, invalidated, MAX_INT64, seedCache, selectMerchant,
  server, type Recorded, type Reply,
} from "@/test/harness"

type Go = <TData, TError, TInput, TContext>(
  options: MutationOptions<TData, TError, TInput, TContext>,
  input: TInput
) => Promise<TData>
type Case = [
  name: string,
  run: (queryClient: QueryClient, go: Go) => Promise<unknown>,
  calls: string | string[],
  invalidates: string[],
  body?: unknown,
]

const customerTree = ["customer", "customer.rates"]
const catalogTree = ["catalog", "drift", "meter", "meters"]
const subTree = ["subscription", "subscriptions"]
const meterTree = ["meter", "meters"]
const price = { product_id: "prod_1", key: "pro-monthly", unit_amount: "20000000", currency: "usd", auto_renew: true }
const ratePrice = { model: "per_unit" as const, currency: "USD", per_unit: { unit_amount: "1000000", divide_by: 1 } }
const rateCard = { product_id: "prod_1", filter: {}, price: ratePrice }
const meter = { event_type: "token.used", value_property: "tokens", aggregation: "sum" as const, unit: "tokens", group_by: {} }
const refund = { amount: MAX_INT64, reason: "requested", revokeAccess: true }
const offChannel = { price_id: "price_1", transaction_id: "external-1" }
const creditLimit = { customerId: "cus_1", currency: "USD", amount: MAX_INT64 }
const application = { schema_version: 1, application_id: "catalog-test", expected_revision: 0, products: [] }
const effectiveAt = "2026-09-05T00:00:00.000Z"

const cases: Case[] = [
  ["resolves an ops finding", (c, g) => g(M.resolveFinding(c), { id: "find_1", outcome: "approve", notes: "verified" }),
    "POST /merchant/findings/find_1/resolve", ["ops"], { outcome: "approve", notes: "verified" }],
  ["refunds a payment at the int64 boundary", (c, g) => g(M.refundPayment(c, "pay_1", "cus_1", "sub_1"), refund),
    "POST /merchant/payments/pay_1/refunds", [...customerTree, "payment", "payments", "subscription"],
    { amount: MAX_INT64, reason: "requested", revoke_access: true }],
  ["cancels a subscription", (c, g) => g(M.cancelSubscription(c, "sub_1", "cus_1"), { reason: "requested", revokeAccess: true }),
    "POST /merchant/subscriptions/sub_1/cancel", [...customerTree, ...subTree], { reason: "requested", revoke_access: true }],
  ["resumes a subscription", (c, g) => g(M.resumeSubscription(c, "sub_1", "cus_1"), undefined),
    "POST /merchant/subscriptions/sub_1/resume", [...customerTree, ...subTree]],
  ["changes the subscription payment method", (c, g) => g(M.changeSubscriptionPaymentMethod(c, "sub_1", "cus_1"), "pm_1"),
    "PUT /merchant/subscriptions/sub_1/payment-method", [...customerTree, ...subTree], { payment_method_id: "pm_1" }],
  ["previews a tier change without touching the cache", (_c, g) => g(M.previewSubscriptionTierChange("sub_1"), "price_2"),
    "POST /merchant/subscriptions/sub_1/change-tier/preview", []],
  ["applies a reviewed tier change", (c, g) => g(M.changeSubscriptionTier(c, "sub_1", "cus_1"), { priceId: "price_2", idempotencyKey: "tier-key-1" }),
    "POST /merchant/subscriptions/sub_1/change-tier", [...customerTree, "payment", "payments", ...subTree], { price_id: "price_2" }],
  ["cancels a scheduled reprice", (c, g) => g(M.cancelSubscriptionReprice(c, "sub_1"), "rep_1"),
    "POST /merchant/reprices/rep_1/cancel", [...catalogTree, ...subTree]],
  ["grants an entitlement", (c, g) => g(M.grantCustomerEntitlement(c, "cus_1"), { entitlement: "premium", hours: 48 }),
    "POST /merchant/customers/cus_1/entitlements", customerTree, { entitlement: "premium", hours: 48 }],
  ["revokes an entitlement", (c, g) => g(M.revokeCustomerEntitlement(c, "cus_1"), "ent_1"),
    "DELETE /merchant/customers/cus_1/entitlements/ent_1", customerTree],
  ["grants product access until an instant", (c, g) => g(M.grantCustomerProductAccess(c, "cus_1"), { productId: "prod_1", endsAt: effectiveAt }),
    "POST /merchant/customers/cus_1/product-access", customerTree, { product_id: "prod_1", ends_at: effectiveAt }],
  ["revokes product access", (c, g) => g(M.revokeCustomerProductAccess(c, "cus_1"), "acc_1"),
    "DELETE /merchant/customers/cus_1/product-access/acc_1", customerTree],
  ["records an off-channel payment", (c, g) => g(M.recordCustomerOffChannelPayment(c, "cus_1"), offChannel),
    "POST /merchant/customers/cus_1/payments/off-channel", [...customerTree, "payment", "payments"], offChannel],
  ["asks the catalog copilot without invalidating the catalog", (_c, g) => g(M.askCatalogCopilot(), "what do we sell?"),
    "POST /merchant/catalog/ask", []],
  ["loads the live price and product behind a copilot draft", (_c, g) => g(M.loadCatalogPriceDraft(), "pro-monthly"),
    ["GET /merchant/catalog/prices/by-key/pro-monthly", "GET /merchant/catalog/products/prod_1"], []],
  ["applies a catalog application", (c, g) => g(M.applyCatalog(c), JSON.stringify(application)),
    "POST /merchant/catalog/applications", catalogTree, application],
  ["refreshes drift alone", (c, g) => g(M.refreshCatalogDrift(c), undefined),
    "POST /merchant/catalog/drift/refresh", ["drift"]],
  ["creates a product", (c, g) => g(M.createProduct(c), { key: "pro", display_name: "Pro", description: "" }),
    "POST /merchant/catalog/products", catalogTree],
  ["deactivates a product", (c, g) => g(M.setProductActive(c), { id: "prod_1", active: false }),
    "POST /merchant/catalog/products/prod_1/deactivate", catalogTree],
  ["creates a price", (c, g) => g(M.createPrice(c), price),
    "POST /merchant/catalog/prices", catalogTree, price],
  ["activates a price", (c, g) => g(M.setPriceActive(c), { id: "price_1", active: true }),
    "POST /merchant/catalog/prices/price_1/activate", catalogTree],
  ["previews affected subscribers without writing catalog state", (_c, g) => g(M.previewPriceChange(), "pro-monthly"),
    "GET /merchant/catalog/reprice-all-prior-versions/preview", []],
  ["cancels a batch of reprices", (c, g) => g(M.cancelReprices(c), ["rep_1", "rep_2"]),
    ["POST /merchant/reprices/rep_1/cancel", "POST /merchant/reprices/rep_2/cancel"], catalogTree],
  ["stores a usage meter", (c, g) => g(M.putUsageMeter(c), { key: "tokens", meter }),
    "PUT /merchant/catalog/meters/tokens", meterTree, meter],
  ["stores a default rate card", (c, g) => g(M.putDefaultUsageRateCard(c), { key: "tokens", rateCard }),
    "PUT /merchant/catalog/meters/tokens/rate-card", meterTree, rateCard],
  ["removes a default rate card", (c, g) => g(M.deleteDefaultUsageRateCard(c), "tokens"),
    "DELETE /merchant/catalog/meters/tokens/rate-card", meterTree],
  ["stores a negotiated rate", (c, g) => g(M.putCustomerUsageRateOverride(c), { customerId: "cus_1", meterKey: "tokens", override: { price: ratePrice } }),
    "PUT /merchant/customers/cus_1/rate-overrides/tokens", [...meterTree, ...customerTree, "dashboard"], { price: ratePrice }],
  ["removes a negotiated rate", (c, g) => g(M.deleteCustomerUsageRateOverride(c), { customerId: "cus_1", meterKey: "tokens" }),
    "DELETE /merchant/customers/cus_1/rate-overrides/tokens", [...meterTree, ...customerTree, "dashboard"]],
  ["updates settings without dropping the provider list", (c, g) => g(M.updateMerchantSettings(c), { profile: { display_name: "Acme" } }),
    "PUT /merchant/settings", ["settings"]],
  ["saves provider credentials", (c, g) => g(M.savePaymentProvider(c), { rail: "nmi", provider: { account_id: "gw_1" } }),
    "PUT /merchant/payment-providers/nmi", ["providers"], { account_id: "gw_1" }],
  ["archives one provider account by id", (c, g) => g(M.archivePaymentProvider(c), { rail: "nmi", id: "psp_1", allowLast: true }),
    "POST /merchant/payment-providers/nmi/accounts/psp_1/archive", ["providers"], { allow_last: true }],
  ["sets a customer credit limit at the int64 boundary", (_c, g) => g(M.setCreditLimit(), creditLimit),
    "PUT /merchant/credit-limit", [], { customer_id: "cus_1", currency: "USD", credit_limit_amount: MAX_INT64 }],
]

let requests: Recorded[]
let routes: Record<string, Reply>
beforeEach(async () => {
  routes = {
    "/merchant/catalog/prices/by-key/pro-monthly": { id: "price_1", product_id: "prod_1" },
    "POST /merchant/notifications/note_2/read": () => new Response(null, { status: 503 }),
  }
  requests = await server(routes)
})
afterEach(() => vi.unstubAllGlobals())

it.each(cases)("%s", async (_name, run, expected, invalidates, body) => {
  const queryClient = client()
  const seeded = [...seedCache(queryClient, "merchant-a"), ...seedCache(queryClient, "merchant-b")]
  selectMerchant("merchant-a")
  await run(queryClient, (options, input) => {
    selectMerchant("merchant-b") // the console switched merchants mid-flight
    return exec(queryClient, options, input)
  })
  expect(calls(requests)).toEqual(typeof expected === "string" ? [expected] : expected)
  if (body) expect(requests[0].body).toEqual(body)
  expect(invalidated(queryClient, seeded)).toEqual(
    invalidates.map((n) => `merchant-a:${n}`).sort()
  )
})

it("sends the selected merchant, the caller's tier key and a fresh refund key", async () => {
  const queryClient = client()
  selectMerchant("merchant-a")
  const once = { amount: "1000000", reason: "", revokeAccess: false }
  await exec(queryClient, M.changeSubscriptionTier(queryClient, "sub_1", "cus_1"), { priceId: "price_2", idempotencyKey: "tier-key-1" })
  await exec(queryClient, M.refundPayment(queryClient, "pay_1"), once)
  await exec(queryClient, M.refundPayment(queryClient, "pay_1"), once)
  expect(requests[0].headers.get("X-OpenRails-Merchant")).toBe("merchant-a")
  expect(requests[0].headers.get("Idempotency-Key")).toBe("tier-key-1")
  // A refund is a new operation every time it is submitted.
  const keys = requests.slice(1).map((r) => r.headers.get("Idempotency-Key"))
  expect(keys[0]).toMatch(/^[\da-f-]{36}$/)
  expect(keys[0]).not.toBe(keys[1])
  expect(requests[1].body).toEqual({ amount: "1000000", revoke_access: false })
})

describe("notification read state", () => {
  const unreadKey = () => [...queryKeys.notifications(), "unread-count"]
  const seed = (queryClient: QueryClient) => {
    queryClient.setQueryData(queryKeys.notifications(), {
      data: [{ id: "note_1", read_at: null }, { id: "note_2", read_at: null }],
    })
    queryClient.setQueryData(unreadKey(), { unread: 2 })
  }
  const started = () => {
    const queryClient = client()
    selectMerchant("merchant-a")
    seed(queryClient)
    return queryClient
  }
  const readFlags = (queryClient: QueryClient) =>
    queryClient
      .getQueryData<{ data: { read_at: string | null }[] }>(queryKeys.notifications())!
      .data.map((notification) => notification.read_at !== null)

  it("reconciles both caches from the ids the server accepted", async () => {
    const queryClient = started()
    // note_2 is answered 503 by the harness: a partial bulk result.
    const readIds = await exec(queryClient, M.markNotificationsRead(queryClient), ["note_1", "note_2"])
    expect(readIds).toEqual(["note_1"])
    expect(readFlags(queryClient)).toEqual([true, false])
    expect(queryClient.getQueryData(unreadKey())).toEqual({ unread: 1 })
  })

  it("marks one notification read", async () => {
    const queryClient = started()
    await exec(queryClient, M.markNotificationRead(queryClient), "note_1")
    expect(readFlags(queryClient)).toEqual([true, false])
    expect(queryClient.getQueryData(unreadKey())).toEqual({ unread: 1 })
  })
})

it("stores a saved dashboard on the merchant that saved it", async () => {
  const queryClient = client()
  const seeded = [...seedCache(queryClient, "merchant-a"), ...seedCache(queryClient, "merchant-b")]
  const dashboard = (merchant: string) => seeded.find(([name]) => name === `${merchant}:dashboard`)![1]
  const widgets = [{ id: "w1", title: "Revenue", viz: "stat" as const, query: { measures: ["revenue"], range: { last: "30d" } }, grid: { x: 0, y: 0, w: 3, h: 2 } }]
  const saved = { widgets, is_default: false }
  routes["PUT /merchant/dashboard"] = saved
  selectMerchant("merchant-a")
  const options = M.saveDashboard(queryClient)
  selectMerchant("merchant-b")
  await exec(queryClient, options, widgets)
  expect(queryClient.getQueryData(dashboard("merchant-a"))).toEqual(saved)
  expect(queryClient.getQueryData(dashboard("merchant-b"))).toEqual({})
})

it("walks every export page, stops on an empty one, and looks one customer up", async () => {
  const queryClient = client()
  selectMerchant("merchant-a")
  const page = (rows: unknown[], total: number) => ({ data: rows, total })
  const customers = [page([{ id: "cus_1" }], 2), page([{ id: "cus_2" }], 2)]
  const subscriptions = [page([{ id: "sub_1" }], 2), page([], 2)]
  routes["/merchant/customers"] = () => customers.shift() ?? { data: [{ id: "cus_9" }], total: 1 }
  routes["/merchant/subscriptions"] = () => subscriptions.shift()
  expect(await exec(queryClient, M.exportCustomers(), "alice")).toEqual([{ id: "cus_1" }, { id: "cus_2" }])
  expect(await exec(queryClient, M.exportSubscriptions(), { status: "past_due" })).toEqual([{ id: "sub_1" }])
  expect(await exec(queryClient, M.findCustomer(), "a@example.test")).toEqual({ id: "cus_9" })
  expect(requests.map((request) => request.query)).toEqual([
    "q=alice&limit=200&offset=0", "q=alice&limit=200&offset=200",
    "status=past_due&limit=200&offset=0", "status=past_due&limit=200&offset=200",
    "q=a%40example.test&limit=1&offset=0",
  ])
})

describe("price change", () => {
  const change = { price, copilotDraftId: "draft_1", migration: { priceKey: "pro-monthly", effectiveAt } }
  const refreshed = catalogTree.map((name) => `merchant-a:${name}`)

  it("creates the replacement price before scheduling its migration", async () => {
    const queryClient = client()
    const seeded = seedCache(queryClient, "merchant-a")
    await exec(queryClient, M.changePrice(queryClient), change)
    expect(calls(requests).slice(0, 2)).toEqual([
      "POST /merchant/catalog/prices",
      "POST /merchant/catalog/reprice-all-prior-versions",
    ])
    expect(requests[1].body).toEqual({ price_key: "pro-monthly", effective_at: effectiveAt })
    expect(invalidated(queryClient, seeded)).toEqual(refreshed)
    // Provenance is recorded out of band; the write never waits on it.
    await vi.waitFor(() => expect(calls(requests)).toContain("POST /merchant/catalog/copilot/confirm"))
  })

  it("refreshes the catalog when scheduling fails after the price was created", async () => {
    const queryClient = client()
    const seeded = seedCache(queryClient, "merchant-a")
    routes["POST /merchant/catalog/reprice-all-prior-versions"] = () =>
      Response.json({ error: { message: "schedule failed" } }, { status: 503 })
    await expect(exec(queryClient, M.changePrice(queryClient), change)).rejects.toThrow("schedule failed")
    expect(invalidated(queryClient, seeded)).toEqual(refreshed)
  })
})
