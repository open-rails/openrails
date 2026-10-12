// Every route the client calls must be in OpenRails' route catalog
// (src/client/generated/routes.ts), beneath the client's /billing mount.
import { expect, it, vi } from "vitest"

import { createBillingClient } from "./client"
import { OPENRAILS_ROUTES } from "./generated/routes"

const routes = OPENRAILS_ROUTES.map(({ method, path }) => ({
  method,
  pattern: new RegExp(`^/billing${path.replace(/\{[^}]+\}/g, "[^/]+")}$`),
}))

it("calls only routes OpenRails mounts for customers", async () => {
  const called = new Set<string>()
  const fetch = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const path = String(input).split("?")[0]
    called.add(`${init?.method ?? "GET"} ${path}`)
    return new Response(null, { status: 204 })
  })
  const client = createBillingClient({ fetch })
  const calls: (() => Promise<unknown>)[] = [
    () => client.listSubscriptions(),
    () => client.getSubscription("sub_1"),
    () => client.cancelSubscription("sub_1", { reason: "why not" }),
    () => client.resumeSubscription("sub_1"),
    () => client.setSubscriptionPaymentMethod("sub_1", "pm_1"),
    () => client.previewSubscriptionChange("sub_1", { priceId: "price_1" }),
    () =>
      client.changeSubscription("sub_1", {
        priceId: "price_1",
        idempotencyKey: "k",
      }),
    () => client.listPaymentMethods(),
    () => client.addPaymentMethod({ psp_id: "psp_1", token: "tok" }),
    () => client.confirmPaymentMethod("pm_1"),
    () => client.getConfig(),
    () => client.getCustomerConfig(),
    () => client.createNetworkLinkChallenge(),
    () => client.createNetworkLink({ challengeId: "lnc_1", idToken: "t" }),
    () => client.confirmSubscriptionChange("sub_1"),
    () => client.removePaymentMethod("pm_1"),
    () =>
      client.setDefaultPaymentMethod({
        currency: "USD",
        paymentMethodId: "pm_1",
      }),
    () =>
      client.setLowBalanceLine({
        currency: "USD",
        amount: "2500000",
        runwayHours: null,
      }),
    () => client.listPayments(),
    () => client.listOrders({ origin: "usage" }),
    () => client.getOrder("ord_1"),
    () =>
      client.createOrder({
        lines: [{ price_id: "price_1" }],
        idempotencyKey: "k",
      }),
    () =>
      client.payOrder("ord_1", {
        payment: { payment_method_id: "pm_1" },
        expectedTotal: "1",
        idempotencyKey: "k",
      }),
    () => client.confirmOrder("ord_1"),
    () => client.getUsage({ currency: "USD" }),
    () => client.getEntitlements(),
  ]
  for (const call of calls) await call().catch(() => undefined)

  const missing = [...called].filter((c) => {
    const [method, path] = c.split(" ")
    return !routes.some((r) => r.method === method && r.pattern.test(path))
  })
  expect(missing).toEqual([])
  expect(called.size).toBe(calls.length)
})
