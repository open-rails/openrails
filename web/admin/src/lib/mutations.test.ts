// Every console write, driven through the real endpoint + fetch path. Each
// row states the requests it issues and its exact cache blast radius, and is
// replayed with the merchant switched mid-flight to prove the refresh follows
// the merchant that started the write. Request bodies are asserted where they
// carry money or authority.
import type { MutationOptions, QueryClient } from "@tanstack/react-query"
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

import type { Meter } from "@/lib/api/generated/wire"
import { adminMutations as M } from "@/lib/mutations"
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
const storedMeter = { ...meter, key: "tokens", revision: 3 } as unknown as Meter
const refund = { amount: MAX_INT64, reason: "requested", revokeAccess: true }
const creditLimit = { customerId: "cus_1", currency: "USD", amount: MAX_INT64 }
const application = { schema_version: 1, products: {} }
const effectiveAt = "2026-09-05T00:00:00.000Z"

const cases: Case[] = [
  ["resolves an ops finding", (c, g) => g(M.resolveFinding(c), { id: "find_1", outcome: "approve", notes: "verified" }),
    "POST /admin/findings/find_1/resolve", ["ops"], { outcome: "approve", notes: "verified" }],
  ["refunds a payment at the int64 boundary", (c, g) => g(M.refundPayment(c, "pay_1", "cus_1", "sub_1"), refund),
    "POST /admin/payments/pay_1/refunds", [...customerTree, "payment", "payments", "subscription"],
    { amount: MAX_INT64, reason: "requested", revoke_access: true }],
  ["cancels a subscription", (c, g) => g(M.cancelSubscription(c, "sub_1", "cus_1"), { reason: "requested", revokeAccess: true }),
    "POST /admin/subscriptions/sub_1/cancel", [...customerTree, ...subTree], { reason: "requested", revoke_access: true }],
  ["resumes a subscription", (c, g) => g(M.resumeSubscription(c, "sub_1", "cus_1"), undefined),
    "POST /admin/subscriptions/sub_1/resume", [...customerTree, ...subTree]],
  ["changes the subscription payment method", (c, g) => g(M.changeSubscriptionPaymentMethod(c, "sub_1", "cus_1"), "pm_1"),
    "PUT /admin/subscriptions/sub_1/payment-method", [...customerTree, ...subTree], { payment_method_id: "pm_1" }],
  ["previews a change without touching the cache", (_c, g) => g(M.previewSubscriptionChange("sub_1"), { price_id: "price_2" }),
    "POST /admin/subscriptions/sub_1/change/preview", [], { price_id: "price_2" }],
  ["applies a reviewed change", (c, g) => g(M.changeSubscription(c, "sub_1", "cus_1"), { change: { price_id: "price_2", quantity: 3 }, idempotencyKey: "change-key-1" }),
    "POST /admin/subscriptions/sub_1/change", [...customerTree, "payment", "payments", ...subTree], { price_id: "price_2", quantity: 3 }],
  ["grants a product until an instant", (c, g) => g(M.grantCustomerProductAccess(c, "cus_1"), { productId: "prod_1", endsAt: effectiveAt }),
    "POST /admin/product-access", customerTree, { items: [{ customer_id: "cus_1", product_id: "prod_1", ends_at: effectiveAt }] }],
  ["grants a product for hours with a note", (c, g) => g(M.grantCustomerProductAccess(c, "cus_1"), { productId: "prod_1", hours: 48, note: "support fix" }),
    "POST /admin/product-access", customerTree, { items: [{ customer_id: "cus_1", product_id: "prod_1", hours: 48, note: "support fix" }] }],
  ["revokes product access with a reason", (c, g) => g(M.revokeCustomerProductAccess(c, "cus_1"), { grantId: "acc_1", reason: "refund" }),
    "POST /admin/product-access/acc_1/revoke", customerTree, { reason: "refund" }],
  ["asks the catalog copilot without invalidating the catalog", (_c, g) => g(M.askCatalogCopilot(), "what do we sell?"),
    "POST /admin/catalog/ask", []],
  ["loads the live price and product behind a copilot draft", (_c, g) => g(M.loadCatalogPriceDraft(), { productKey: "pro", priceKey: "monthly" }),
    "GET /admin/catalog/products", []],
  ["applies a catalog application", (c, g) => g(M.applyCatalog(c), { document: JSON.stringify(application), force: false }),
    "POST /admin/catalog/applications", catalogTree, application],
  ["re-reads the PSPs and refreshes drift alone", (c, g) => g(M.refreshPSPs(c), undefined),
    "POST /admin/psps/refresh", ["drift"]],
  ["creates a product", (c, g) => g(M.createProduct(c), { key: "pro", display_name: "Pro", description: "" }),
    "POST /admin/catalog/products", catalogTree],
  ["archives a product", (c, g) => g(M.setProductActive(c), { id: "prod_1", active: false, revision: 4 }),
    "PATCH /admin/catalog/products/prod_1", catalogTree, { archived: true, expected_revision: 4 }],
  ["creates a price", (c, g) => g(M.createPrice(c), price),
    "POST /admin/catalog/prices", catalogTree, price],
  ["restores a price", (c, g) => g(M.setPriceActive(c), { id: "price_1", active: true, revision: 2 }),
    "PATCH /admin/catalog/prices/price_1", catalogTree, { archived: false, expected_revision: 2 }],
  ["previews affected subscribers without writing catalog state", (_c, g) => g(M.previewPriceChange(), { productKey: "pro", priceKey: "monthly" }),
    "POST /admin/price-migrations/preview", [], { product_key: "pro", price_key: "monthly" }],
  ["cancels a price migration", (c, g) => g(M.cancelPriceMigration(c), "pmig_1"),
    "POST /admin/price-migrations/pmig_1/cancel", catalogTree],
  ["stores a usage meter", (c, g) => g(M.putUsageMeter(c), { key: "tokens", meter }),
    "PUT /admin/catalog/meters/tokens", meterTree, meter],
  ["stores a default rate card", (c, g) => g(M.putDefaultUsageRateCard(c), { meter: storedMeter, rateCard }),
    "PUT /admin/catalog/meters/tokens", meterTree, { ...meter, rate_card: rateCard, expected_revision: 3 }],
  ["removes a default rate card", (c, g) => g(M.deleteDefaultUsageRateCard(c), storedMeter),
    "PUT /admin/catalog/meters/tokens", meterTree, { ...meter, rate_card: null, expected_revision: 3 }],
  ["stores a negotiated rate", (c, g) => g(M.putCustomerUsageRateOverride(c), { customerId: "cus_1", meterKey: "tokens", override: { price: ratePrice } }),
    "PUT /admin/catalog/rate-overrides/cus_1/tokens", [...meterTree, ...customerTree, "dashboard"], { price: ratePrice }],
  ["removes a negotiated rate", (c, g) => g(M.deleteCustomerUsageRateOverride(c), { customerId: "cus_1", meterKey: "tokens" }),
    "DELETE /admin/catalog/rate-overrides/cus_1/tokens", [...meterTree, ...customerTree, "dashboard"]],
  ["applies a settings change without dropping the PSP list", (c, g) => g(M.updateMerchantSettings(c), { revision: "rev_1", settings: { profile: { display_name: "Acme" } } }),
    "PATCH /admin/configuration", ["settings"]],
  ["adds a PSP", (c, g) => g(M.createPSP(c), { key: "mobius", rail: "nmi", account_id: "gw_1", operation_id: "ba47eaf9-7307-48e0-a41d-435af9c49ef9" }),
    "POST /admin/psps", ["psps"], { key: "mobius", rail: "nmi", account_id: "gw_1", operation_id: "ba47eaf9-7307-48e0-a41d-435af9c49ef9" }],
  ["rotates PSP credentials", (c, g) => g(M.updatePSP(c), { id: "psp_1", psp: { operation_id: "ba47eaf9-7307-48e0-a41d-435af9c49ef9", expected_revision: 0 } }),
    "PATCH /admin/psps/psp_1", ["psps"], { operation_id: "ba47eaf9-7307-48e0-a41d-435af9c49ef9", expected_revision: 0 }],
  ["archives one PSP by id", (c, g) => g(M.archivePSP(c), { id: "psp_1", allowLast: true }),
    "PATCH /admin/psps/psp_1", ["psps"], { archived: true, allow_last: true }],
  ["sets a customer credit limit at the int64 boundary", (_c, g) => g(M.setCreditLimit(), creditLimit),
    "PATCH /admin/customers/cus_1", [], { credit_limits: [{ currency: "USD", amount: MAX_INT64 }] }],
]

let requests: Recorded[]
let routes: Record<string, Reply>
beforeEach(async () => {
  routes = {
    "/admin/catalog/products": { data: [{ id: "prod_1", key: "pro", display_name: "Pro", prices: [{ id: "price_1", key: "monthly", product_id: "prod_1" }] }] },
    "POST /admin/product-access": { items: [{ id: "acc_1" }] },
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
  const pspWrite = { "adds a PSP": "POST /admin/psps", "rotates PSP credentials": "PATCH /admin/psps/psp_1" }[_name]
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

it("sends the selected merchant, the caller's change key and a fresh refund key", async () => {
  const queryClient = client()
  selectMerchant("merchant-a")
  const once = { amount: "1000000", reason: "", revokeAccess: false }
  await exec(queryClient, M.changeSubscription(queryClient, "sub_1", "cus_1"), { change: { price_id: "price_2" }, idempotencyKey: "change-key-1" })
  await exec(queryClient, M.refundPayment(queryClient, "pay_1"), once)
  await exec(queryClient, M.refundPayment(queryClient, "pay_1"), once)
  expect(requests[0].headers.get("OpenRails-Merchant")).toBe("merchant-a")
  expect(requests[0].headers.get("Idempotency-Key")).toBe("change-key-1")
  // A refund is a new operation every time it is submitted.
  const keys = requests.slice(1).map((r) => r.headers.get("Idempotency-Key"))
  expect(keys[0]).toMatch(/^[\da-f-]{36}$/)
  expect(keys[0]).not.toBe(keys[1])
  expect(requests[1].body).toEqual({ amount: "1000000", revoke_access: false })
})

it("stores a saved dashboard on the merchant that saved it", async () => {
  const queryClient = client()
  const seeded = [...seedCache(queryClient, "merchant-a"), ...seedCache(queryClient, "merchant-b")]
  const dashboard = (merchant: string) => seeded.find(([name]) => name === `${merchant}:dashboard`)![1]
  const widgets = [{ id: "w1", title: "Revenue", viz: "stat" as const, query: { measures: ["revenue"], range: { last: "30d" } }, grid: { x: 0, y: 0, w: 3, h: 2 } }]
  const saved = { widgets, is_default: false }
  routes["PUT /admin/dashboard"] = saved
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
  routes["/admin/customers"] = () => customers.shift() ?? { data: [{ id: "cus_9" }], next_cursor: "more" }
  routes["/admin/subscriptions"] = () => subscriptions.shift()
  expect(await exec(queryClient, M.exportCustomers(), "alice")).toEqual([{ id: "cus_1" }, { id: "cus_2" }])
  expect(await exec(queryClient, M.exportSubscriptions(), { status: "past_due" })).toEqual([{ id: "sub_1" }])
  expect(await exec(queryClient, M.findCustomer(), "a@example.test")).toEqual({ id: "cus_9" })
  expect(requests.map((request) => request.query)).toEqual([
    "search=alice&limit=200", "search=alice&limit=200&cursor=c1",
    "status=past_due&limit=200", "status=past_due&limit=200&cursor=c2",
    "search=a%40example.test&limit=1",
  ])
})

it("exports every payment by cursor, under the list's filters", async () => {
  const queryClient = client()
  selectMerchant("merchant-a")
  routes["/admin/payments"] = cursorPages(["pay_1", "pay_2", "pay_3"].map((id) => aPayment(id)), 2)
  const rows = await exec(queryClient, M.exportPayments(), { kind: "refund", rail: "nmi" })
  expect(rows.map((row) => row.id)).toEqual(["pay_1", "pay_2", "pay_3"])
  expect(requests.map((request) => request.query)).toEqual([
    "kind=refund&rail=nmi&limit=200", "kind=refund&rail=nmi&limit=200&cursor=2",
  ])
})

describe("price change", () => {
  const change = { price, migration: { productKey: "pro", priceKey: "monthly", effectiveAt } }
  const refreshed = catalogTree.map((name) => `merchant-a:${name}`)

  it("creates the replacement price before migrating to it", async () => {
    const queryClient = client()
    const seeded = seedCache(queryClient, "merchant-a")
    routes["POST /admin/catalog/prices"] = { id: "price_2" }
    await exec(queryClient, M.changePrice(queryClient), change)
    expect(calls(requests).slice(0, 2)).toEqual([
      "POST /admin/catalog/prices",
      "POST /admin/price-migrations",
    ])
    expect(requests[1].body).toEqual({ product_key: "pro", price_key: "monthly", to_price_id: "price_2", effective_at: effectiveAt })
    expect(invalidated(queryClient, seeded)).toEqual(refreshed)
  })

  it("refreshes the catalog when scheduling fails after the price was created", async () => {
    const queryClient = client()
    const seeded = seedCache(queryClient, "merchant-a")
    routes["POST /admin/price-migrations"] = () =>
      Response.json({ error: { message: "schedule failed" } }, { status: 503 })
    await expect(exec(queryClient, M.changePrice(queryClient), change)).rejects.toThrow("schedule failed")
    expect(invalidated(queryClient, seeded)).toEqual(refreshed)
  })
})
