// Every OpenRails route the client calls must be in the pinned route catalog.
import { expect, it, vi } from "vitest"

import { createBillingClient } from "./client"
import contract from "./generated/openrails-contract.json"

const routes = contract.routes.map(({ method, path }) => ({
  method,
  pattern: new RegExp(`^${path.replace(/\{[^}]+\}/g, "[^/]+")}$`),
}))

it("calls only routes OpenRails mounts for customers", async () => {
  const called = new Set<string>()
  const fetch = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const path = String(input).split("?")[0]
    called.add(`${init?.method ?? "GET"} ${path}`)
    return path.endsWith("/solana-cancel-tx")
      ? Response.json({ transaction: "dHg=" })
      : new Response(null, { status: 204 })
  })
  const client = createBillingClient({ fetch })
  const calls: (() => Promise<unknown>)[] = [
    () => client.listSubscriptions(),
    () => client.getSubscription("sub_1"),
    () => client.cancelSubscription("sub_1", { feedback: "why not" }),
    () => client.resumeSubscription("sub_1"),
    () => client.setSubscriptionPaymentMethod("sub_1", "pm_1"),
    () => client.cancelSubscriptionOnChain("sub_1", async () => "sig"),
    () => client.listPaymentMethods(),
    () => client.addPaymentMethod({ payment_token: "tok" }),
    () => client.removePaymentMethod("pm_1"),
    () =>
      client.setDefaultPaymentMethod({
        currency: "USD",
        paymentMethodId: "pm_1",
      }),
    () => client.listPayments(),
    () => client.listInvoices(),
    () => client.getInvoice("inv_1"),
    () => client.getStatus(),
  ]
  for (const call of calls) await call().catch(() => undefined)

  const missing = [...called].filter((c) => {
    const [method, path] = c.split(" ")
    return !routes.some((r) => r.method === method && r.pattern.test(path))
  })
  expect(missing).toEqual([])
  expect(called.size).toBe(15)
})
