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
  aPayment, calls, client, cursorPages, exec, invalidated, MAX_INT64, seedCache, selectMerchant,
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
const price = { product_id: "prod_1", key: "monthly", unit_amount: "20000000", currency: "usd", billing_interval_hours: 720 }
const ratePrice = { model: "per_unit" as const, currency: "USD", per_unit: { unit_amount: "1000000", divide_by: 1 } }
const rateCard = { product_id: "prod_1", filter: {}, price: ratePrice }
const meter = { event_type: "token.used", value_property: "tokens", aggregation: "sum" as const, unit: "tokens", group_by: {} }
const refund = { amount: MAX_INT64, reason: "requested", revokeAccess: true }
const offChannel = { price_id: "price_1", transaction_id: "external-1" }
const creditLimit = { customerId: "cus_1", currency: "USD", amount: MAX_INT64 }
const application = { schema_version: 1, products: {} }
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
  ["loads the live price and product behind a copilot draft", (_c, g) => g(M.loadCatalogPriceDraft(), { productKey: "pro", priceKey: "monthly" }),
    ["GET /merchant/catalog/products/by-key/pro/prices/by-key/monthly", "GET /merchant/catalog/products/prod_1"], []],
  ["applies a catalog application", (c, g) => g(M.applyCatalog(c), JSON.stringify(application)),
    "POST /merchant/catalog/applications", catalogTree, application],
  ["refreshes drift alone", (c, g) => g(M.refreshCatalogDrift(c), undefined),
    "POST /merchant/catalog/drift/refresh", ["drift"]],
  ["creates a product", (c, g) => g(M.createProduct(c), { key: "pro", display_name: "Pro", description: "" }),
    "POST /merchant/catalog/products", catalogTree],
  ["archives a product", (c, g) => g(M.setProductActive(c), { id: "prod_1", active: false }),
    "PATCH /merchant/catalog/products/prod_1", catalogTree, { archived: true }],
  ["creates a price", (c, g) => g(M.createPrice(c), price),
    "POST /merchant/catalog/prices", catalogTree, price],
  ["restores a price", (c, g) => g(M.setPriceActive(c), { id: "price_1", active: true }),
    "PATCH /merchant/catalog/prices/price_1", catalogTree, { archived: false }],
  ["previews affected subscribers without writing catalog state", (_c, g) => g(M.previewPriceChange(), { productKey: "pro", priceKey: "monthly" }),
    "POST /merchant/reprice-batches/preview", [], { product_key: "pro", price_key: "monthly" }],
  ["cancels a reprice batch", (c, g) => g(M.cancelRepriceBatch(c), "rpb_1"),
    "POST /merchant/reprice-batches/rpb_1/cancel", catalogTree],
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
  ["applies a settings change without dropping the PSP list", (c, g) => g(M.updateMerchantSettings(c), { revision: "rev_1", settings: { profile: { display_name: "Acme" } } }),
    "POST /merchant/configuration/applications", ["settings"]],
  ["adds a PSP", (c, g) => g(M.createPSP(c), { key: "mobius", rail: "nmi", account_id: "gw_1", operation_id: "ba47eaf9-7307-48e0-a41d-435af9c49ef9" }),
    "POST /merchant/psps", ["psps"], { key: "mobius", rail: "nmi", account_id: "gw_1", operation_id: "ba47eaf9-7307-48e0-a41d-435af9c49ef9" }],
  ["rotates PSP credentials", (c, g) => g(M.updatePSP(c), { id: "psp_1", psp: { operation_id: "ba47eaf9-7307-48e0-a41d-435af9c49ef9", expected_revision: 0 } }),
    "PATCH /merchant/psps/psp_1", ["psps"], { operation_id: "ba47eaf9-7307-48e0-a41d-435af9c49ef9", expected_revision: 0 }],
  ["archives one PSP by id", (c, g) => g(M.archivePSP(c), { id: "psp_1", allowLast: true }),
    "POST /merchant/psps/psp_1/archive", ["psps"], { allow_last: true }],
  ["sets a customer credit limit at the int64 boundary", (_c, g) => g(M.setCreditLimit(), creditLimit),
    "PUT /merchant/customers/cus_1/credit-limit", [], { currency: "USD", amount: MAX_INT64 }],
]

let requests: Recorded[]
let routes: Record<string, Reply>
beforeEach(async () => {
  routes = {
    "/merchant/catalog/products/by-key/pro/prices/by-key/monthly": { id: "price_1", product_id: "prod_1" },
    "POST /merchant/notifications/note_2/read": () => new Response(null, { status: 503 }),
  }
  requests = await server(routes)
})
afterEach(() => vi.unstubAllGlobals())

it.each(cases)("%s", async (_name, run, expected, invalidates, body) => {
  const queryClient = client()
  const seeded = [...seedCache(queryClient, "merchant-a"), ...seedCache(queryClient, "merchant-b")]
  selectMerchant("merchant-a")
  // A PSP write refuses to dispatch once the merchant changed; the switch
  // comes after the scoped request instead.
  const pspWrite = { "adds a PSP": "POST /merchant/psps", "rotates PSP credentials": "PATCH /merchant/psps/psp_1" }[_name]
  if (pspWrite) {
    routes[pspWrite] = () => {
      selectMerchant("merchant-b")
      return {}
    }
  }
  await run(queryClient, (options, input) => {
    if (!pspWrite) selectMerchant("merchant-b")
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
  expect(requests[0].headers.get("OpenRails-Merchant")).toBe("merchant-a")
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
    queryClient.setQueryData(unreadKey(), { unread_count: 2 })
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
    expect(queryClient.getQueryData(unreadKey())).toEqual({ unread_count: 1 })
  })

  it("marks one notification read", async () => {
    const queryClient = started()
    await exec(queryClient, M.markNotificationRead(queryClient), "note_1")
    expect(readFlags(queryClient)).toEqual([true, false])
    expect(queryClient.getQueryData(unreadKey())).toEqual({ unread_count: 1 })
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
  const customers = [{ data: [{ id: "cus_1" }], next_cursor: "c1" }, { data: [{ id: "cus_2" }], next_cursor: null }]
  const subscriptions = [{ data: [{ id: "sub_1" }], next_cursor: "c2" }, { data: [], next_cursor: null }]
  routes["/merchant/customers"] = () => customers.shift() ?? { data: [{ id: "cus_9" }], next_cursor: "more" }
  routes["/merchant/subscriptions"] = () => subscriptions.shift()
  expect(await exec(queryClient, M.exportCustomers(), "alice")).toEqual([{ id: "cus_1" }, { id: "cus_2" }])
  expect(await exec(queryClient, M.exportSubscriptions(), { status: "past_due" })).toEqual([{ id: "sub_1" }])
  expect(await exec(queryClient, M.findCustomer(), "a@example.test")).toEqual({ id: "cus_9" })
  expect(requests.map((request) => request.query)).toEqual([
    "q=alice&limit=200", "q=alice&limit=200&cursor=c1",
    "status=past_due&limit=200", "status=past_due&limit=200&cursor=c2",
    "q=a%40example.test&limit=1",
  ])
})

it("exports every payment by cursor, under the list's filters", async () => {
  const queryClient = client()
  selectMerchant("merchant-a")
  routes["/merchant/payments"] = cursorPages(["pay_1", "pay_2", "pay_3"].map((id) => aPayment(id)), 2)
  const rows = await exec(queryClient, M.exportPayments(), { kind: "refund", rail: "nmi" })
  expect(rows.map((row) => row.id)).toEqual(["pay_1", "pay_2", "pay_3"])
  expect(requests.map((request) => request.query)).toEqual([
    "kind=refund&rail=nmi&limit=200", "kind=refund&rail=nmi&limit=200&cursor=2",
  ])
})

it("tells a new off-channel payment from one already recorded", async () => {
  const queryClient = client()
  selectMerchant("merchant-a")
  const recorded = aPayment("pay_1", { channel: "manual", rail: null, psp_id: null })
  for (const [status, isNew] of [[201, true], [200, false]] as const) {
    routes["POST /merchant/customers/cus_1/payments/off-channel"] = () => Response.json(recorded, { status })
    expect(await exec(queryClient, M.recordCustomerOffChannelPayment(queryClient, "cus_1"), offChannel))
      .toEqual({ payment: recorded, recorded: isNew })
  }
})

describe("price change", () => {
  const change = { price, migration: { productKey: "pro", priceKey: "monthly", effectiveAt } }
  const refreshed = catalogTree.map((name) => `merchant-a:${name}`)

  it("creates the replacement price before scheduling its migration", async () => {
    const queryClient = client()
    const seeded = seedCache(queryClient, "merchant-a")
    await exec(queryClient, M.changePrice(queryClient), change)
    expect(calls(requests).slice(0, 2)).toEqual([
      "POST /merchant/catalog/prices",
      "POST /merchant/reprice-batches",
    ])
    expect(requests[1].body).toEqual({ product_key: "pro", price_key: "monthly", effective_at: effectiveAt })
    expect(invalidated(queryClient, seeded)).toEqual(refreshed)
  })

  it("refreshes the catalog when scheduling fails after the price was created", async () => {
    const queryClient = client()
    const seeded = seedCache(queryClient, "merchant-a")
    routes["POST /merchant/reprice-batches"] = () =>
      Response.json({ error: { message: "schedule failed" } }, { status: 503 })
    await expect(exec(queryClient, M.changePrice(queryClient), change)).rejects.toThrow("schedule failed")
    expect(invalidated(queryClient, seeded)).toEqual(refreshed)
  })
})
