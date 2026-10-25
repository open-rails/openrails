import { test as base, expect, type Page } from "@playwright/test"
import type {} from "./app"

const test = base.extend({
  page: async ({ page }, run, info) => {
    await page.goto("/")
    await expect(page.locator("body")).toHaveAttribute("data-ready", "true")
    await run(page)
    await info.attach("client-methods", {
      body: JSON.stringify(await page.evaluate(() => window.calls)),
      contentType: "application/json",
    })
  },
})
async function card(page: Page, decline = "") {
  return page.evaluate(async (decline) => {
    const response = await fetch("/__fixture/card", {
      method: "POST",
      body: JSON.stringify({ decline }),
    })
    if (!response.ok) throw new Error("external card fixture failed")
    return (await response.json()) as { token: string }
  }, decline)
}
async function authenticate(page: Page, secret: string) {
  const result = await page.evaluate(async (secret) => {
    const response = await fetch("/__fixture/authenticate", {
      method: "POST",
      body: JSON.stringify({ id: secret.split("_secret_")[0] }),
    })
    return response.status
  }, secret)
  expect(result).toBe(204)
}

test("OR-UI-01: exact cart, order ownership, account and customer policy", async ({
  page,
}) => {
  const facts = await page.evaluate(async () => {
    const c = window.billing,
      f = window.fixture
    const config = await c.getConfig(),
      customer = await c.getCustomerConfig(),
      account = await c.getAccount()
    const cart = await c.setCartLine(f.price, { quantity: 3 })
    await c.clearCart()
    const empty = await c.getCart()
    const result = await c.createOrder({
      lines: [{ price_id: f.price, quantity: 3 }],
    })
    const own = await c.getOrder(result.order.id)
    let foreign: unknown
    try {
      await window.makeBilling(f.otherToken).getOrder(result.order.id)
    } catch (error) {
      foreign = {
        status: (error as { status: number }).status,
        code: (error as { code: string }).code,
      }
    }
    const low = await c.setLowBalanceLine({
      currency: "usd",
      amount: "25000000",
      runwayHours: 4,
    })
    const changed = await c.getAccount()
    const usage = await c.getUsage({
      currency: "USD",
      groupBy: "event_type",
      from: "2026-01-01T00:00:00Z",
      to: "2030-01-01T00:00:00Z",
    })
    return {
      config,
      customer,
      account,
      cart,
      empty,
      order: own,
      foreign,
      low,
      changed,
      usage,
      f,
    }
  })
  expect(facts.config.currencies.find((x) => x.code === "USD")).toMatchObject({
    decimals: 6,
  })
  expect(facts.customer.merchant.display_name).toBe("Alpha")
  expect(facts.account.id).toBe(facts.f.alice)
  expect(facts.cart.total).toBe("29970000")
  expect(facts.cart.lines[0]).toMatchObject({
    price_id: facts.f.price,
    quantity: 3,
    amount: "29970000",
  })
  expect(facts.empty.lines).toEqual([])
  expect(facts.order).toMatchObject({
    customer_id: facts.f.alice,
    status: "open",
    total: "29970000",
  })
  expect(facts.foreign).toMatchObject({ status: 404 })
  expect(facts.low).toMatchObject({
    currency: "USD",
    amount: "25000000",
    runway_hours: 4,
    source: "customer",
  })
  expect(facts.changed.low_balance_lines).toContainEqual(facts.low)
  expect(facts.usage.customer_id).toBe(facts.f.alice)
  expect(facts.usage.rows).toMatchObject([
    { key: "sdk-compute", amount: "20", event_count: 1 },
  ])
  const entered = await card(page)
  const paid = await page.evaluate(
    async ({ id, token }) =>
      window.billing.payOrder(id, {
        payment: { token },
        expectedTotal: "29970000",
        idempotencyKey: "sdk-oneoff",
      }),
    { id: facts.order.id, token: entered.token }
  )
  expect(paid.error).toBeUndefined()
  expect(paid.order).toMatchObject({
    id: facts.order.id,
    status: "complete",
    total: "29970000",
  })
  await page.evaluate(
    (id) => window.billing.reportPayURLFailure(id),
    facts.order.id
  )
  const after = await page.evaluate(() =>
    window.billing.getEntitlements({ prefix: "library:" })
  )
  expect(after.data.map((row) => row.entitlement)).toEqual(["library:read"])
})

test("OR-UI-04: bounded read retry, inflight cancellation and write refusal", async ({
  page,
}) => {
  const retried = await page.evaluate(() =>
    window.makeBilling(window.fixture.token, "retry-once").getAccount()
  )
  expect(retried.id).toBe(await page.evaluate(() => window.fixture.alice))
  const failures = await page.evaluate(async () => {
    const out: number[] = []
    try {
      await window
        .makeBilling(window.fixture.token, "always-unavailable")
        .getAccount()
    } catch (error) {
      out.push((error as { status: number }).status)
    }
    try {
      await window
        .makeBilling(window.fixture.token, "write-unavailable")
        .setCartLine(window.fixture.price, { quantity: 3 })
    } catch (error) {
      out.push((error as { status: number }).status)
    }
    return out
  })
  expect(failures).toEqual([503, 503])
  const canceled = await page.evaluate(async () => {
    const controller = new AbortController()
    const pending = window
      .makeBilling(window.fixture.token, "hang")
      .getAccount(controller.signal)
      .then(
        () => "unexpected-success",
        (error: Error) => error.name
      )
    const deadline = Date.now() + 5000
    let arrived = false
    while (Date.now() < deadline) {
      const counts = (await (
        await fetch("/__fixture/counts")
      ).json()) as Record<string, number>
      if (counts.hang === 1) {
        arrived = true
        break
      }
      await new Promise((resolve) => setTimeout(resolve, 10))
    }
    controller.abort()
    if (!arrived)
      throw new Error(
        "the real inflight request never reached the network barrier"
      )
    return pending
  })
  expect(canceled).toBe("AbortError")
  const counts = await page.evaluate(
    async () =>
      (await (await fetch("/__fixture/counts")).json()) as Record<
        string,
        number
      >
  )
  expect(counts).toMatchObject({
    "retry-once": 2,
    "always-unavailable": 2,
    "write-unavailable": 1,
    hang: 1,
  })
  const cart = await page.evaluate(() => window.billing.getCart())
  expect(cart.lines).toEqual([])
})

test("OR-UI-05: a real expired-token refusal refreshes once before one accepted write", async ({
  page,
}) => {
  const accepted = await page.evaluate(async () => {
    const client = window.makeRefreshingBilling()
    const created = await client.createOrder({
      lines: [{ price_id: window.fixture.price, quantity: 2 }],
    })
    const read = await client.getOrder(created.order.id)
    return { created, read, refreshes: window.refreshes }
  })
  expect(accepted.refreshes).toBe(2)
  expect(accepted.created.order).toMatchObject({
    status: "open",
    total: "19980000",
  })
  expect(accepted.read.id).toBe(accepted.created.order.id)
  const counts = await page.evaluate(
    async () =>
      (await (await fetch("/__fixture/counts")).json()) as Record<
        string,
        number
      >
  )
  expect(counts["POST /v1/me/orders"]).toBe(2)
})

test("OR-UI-02: bank challenges, saved cards and subscription changes", async ({
  page,
}) => {
  const entered = await card(page, "auth")
  const pending = await page.evaluate(
    (token) =>
      window.billing.addPaymentMethod({ psp_id: window.fixture.psp, token }),
    entered.token
  )
  expect(pending.status).toBe("requires_action")
  expect(pending.next_action?.type).toBe("authenticate")
  await authenticate(page, pending.next_action!.payload!.client_secret)
  const saved = await page.evaluate(
    (id) => window.billing.confirmPaymentMethod(id),
    pending.id
  )
  expect(saved.id).toBe(pending.id)
  expect(saved.status).toBe("active")
  const methods = await page.evaluate(() => window.billing.listPaymentMethods())
  expect(methods.data.map((item) => item.id)).toContain(saved.id)
  await page.evaluate(
    (id) =>
      window.billing.setDefaultPaymentMethod({
        currency: "usd",
        paymentMethodId: id,
      }),
    saved.id
  )
  const account = await page.evaluate(() => window.billing.getAccount())
  expect(account.default_payment_methods).toContainEqual({
    currency: "USD",
    payment_method_id: saved.id,
  })
  const purchase = await page.evaluate(
    (id) =>
      window.billing.createOrder({
        lines: [{ price_id: window.fixture.subscriptionPrice }],
        expectedTotal: "10000000",
        payment: { payment_method_id: id },
        idempotencyKey: "sdk-enrollment",
      }),
    saved.id
  )
  expect(purchase.order.payment.status).toBe("requires_action")
  await authenticate(
    page,
    purchase.order.payment.next_action!.payload!.client_secret
  )
  const complete = await page.evaluate(
    (id) => window.billing.confirmOrder(id),
    purchase.order.id
  )
  expect(complete.order.status).toBe("complete")
  let subscriptionID = complete.order.lines[0].subscription_id!
  const original = await page.evaluate(
    (id) => window.billing.getSubscription(id),
    subscriptionID
  )
  expect(original.status).toBe("active")
  const assigned = await page.evaluate(
    ({ id, method }) => window.billing.setSubscriptionPaymentMethod(id, method),
    { id: subscriptionID, method: saved.id }
  )
  expect(assigned.payment_method_id).toBe(saved.id)
  const preview = await page.evaluate(
    (id) =>
      window.billing.previewSubscriptionChange(id, {
        priceId: window.fixture.upgradePrice,
      }),
    subscriptionID
  )
  expect(preview).toMatchObject({
    currency: "USD",
    effective: "now",
    amount_due_now: "10000000",
    next_charge_amount: "20000000",
    price_id: await page.evaluate(() => window.fixture.upgradePrice),
  })
  const change = await page.evaluate(
    (id) =>
      window.billing.changeSubscription(id, {
        priceId: window.fixture.upgradePrice,
        idempotencyKey: "sdk-upgrade",
      }),
    subscriptionID
  )
  expect(change.status).toBe("requires_action")
  await authenticate(page, change.next_action!.payload!.client_secret)
  const upgraded = await page.evaluate(
    (id) => window.billing.confirmSubscriptionChange(id),
    subscriptionID
  )
  expect(upgraded.status).toBe("succeeded")
  expect(upgraded.subscription_id).toBeTruthy()
  subscriptionID = upgraded.subscription_id!
  const live = await page.evaluate(
    (id) => window.billing.getSubscription(id),
    subscriptionID
  )
  expect(live.price_id).toBe(
    await page.evaluate(() => window.fixture.upgradePrice)
  )
  const canceled = await page.evaluate(
    (id) =>
      window.billing.cancelSubscription(id, {
        reason: "Customer pauses the next renewal",
      }),
    subscriptionID
  )
  expect(canceled.status).toBe("canceled")
  const resumed = await page.evaluate(
    (id) => window.billing.resumeSubscription(id),
    subscriptionID
  )
  expect(resumed.status).toBe("active")
  expect(resumed.current_period_ends_at).toBe(original.current_period_ends_at)
  const disposable = await card(page)
  const extra = await page.evaluate(
    (token) =>
      window.billing.addPaymentMethod({ psp_id: window.fixture.psp, token }),
    disposable.token
  )
  const unsupported = await page.evaluate(async (id) => {
    try {
      await window.billing.removePaymentMethod(id)
      return null
    } catch (error) {
      return {
        status: (error as { status: number }).status,
        code: (error as { code: string }).code,
      }
    }
  }, extra.id)
  expect(unsupported).toEqual({
    status: 400,
    code: "payment_method_delete_unsupported",
  })
  const kept = await page.evaluate(() => window.billing.listPaymentMethods())
  expect(kept.data.map((item) => item.id)).toContain(extra.id)
  expect(kept.data.map((item) => item.id)).toContain(saved.id)
})

test("OR-UI-03: invalid checkout reporting retains typed errors", async ({
  page,
}) => {
  const errors = await page.evaluate(async () => {
    const out: { method: string; status: number; code: string }[] = []
    for (const [method, call] of [
      [
        "reportPayURLFailure",
        () =>
          window.billing.reportPayURLFailure(
            "ord_00000000000040008000000000000001"
          ),
      ],
    ] as const) {
      try {
        await call()
        throw new Error(`${method} unexpectedly succeeded`)
      } catch (error) {
        out.push({
          method,
          status: (error as { status: number }).status,
          code: (error as { code: string }).code,
        })
      }
    }
    return out
  })
  expect(errors).toEqual([
    { method: "reportPayURLFailure", status: 404, code: "resource_not_found" },
  ])
  expect((await page.evaluate(() => window.billing.getAccount())).id).toBe(
    await page.evaluate(() => window.fixture.alice)
  )
})

test("OR-UI-07: supported card deletion removes actual NMI custody", async ({
  page,
}) => {
  const result = await page.evaluate(async () => {
    const f = window.fixture
    if (!f.nmiToken || !f.nmiPSP || !f.nmiCardToken)
      throw new Error("the actual NMI wire fixture is required")
    const client = window.makeBilling(f.nmiToken, "", "/__nmi/v1")
    const saved = await client.addPaymentMethod({
      psp_id: f.nmiPSP,
      token: f.nmiCardToken,
    })
    const before = await client.listPaymentMethods()
    await client.removePaymentMethod(saved.id)
    const after = await client.listPaymentMethods()
    return { saved, before, after }
  })
  expect(result.saved.card).toMatchObject({ brand: "visa", last4: "5454" })
  expect(result.before.data.map((method) => method.id)).toEqual([
    result.saved.id,
  ])
  expect(result.after.data).toEqual([])
})
