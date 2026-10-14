// A new card entered in OrderPayment subscribes in one click: the customer's
// order is paid with the token, and OpenRails charges and saves the card. The
// loopback NMI gateway (nmimock) declines cards ending 0002; Collect.js is a
// stand-in.
import { expect, test, type Page } from "@playwright/test"

import { createUser } from "./api"
import { openPayment } from "./card"

type Catalog = {
  card_monthly_price_id: string
  card_once_price_id: string
}

type Billing = {
  subscriptions: { status: string; collection_policy: string }[]
  payment_methods: unknown[]
}

// The card Collect.js tokenizes next.
async function nextCard(page: Page, last4: string) {
  await page.evaluate((value) => {
    ;(window as unknown as { e2eCardLast4: string }).e2eCardLast4 = value
  }, last4)
}

const completed = (page: Page) =>
  page.evaluate(
    () => (window as unknown as { paymentCompleted: string[] }).paymentCompleted
  )

async function billing(
  request: import("@playwright/test").APIRequestContext,
  customer: string
): Promise<Billing> {
  return (await (
    await request.get(`/__test/customers/${customer}/billing`)
  ).json()) as Billing
}

test("a new card subscribes in one click", async ({ page, request }) => {
  const catalog = (await (
    await request.get("/__test/health")
  ).json()) as Catalog
  const user = await createUser(request)
  const purchase = await openPayment(page, catalog.card_monthly_price_id, user)
  await purchase.getByRole("button", { name: "Subscribe", exact: true }).click()
  await expect(purchase.getByText("Payment complete").first()).toBeAttached()
  await expect.poll(() => completed(page)).toEqual(["complete"])

  const held = await billing(request, user.id)
  expect(held.subscriptions).toHaveLength(1)
  expect(held.subscriptions[0]).toMatchObject({
    status: "active",
    collection_policy: "engine",
  })
  expect(held.payment_methods).toHaveLength(1)
})

test("a declined new card leaves nothing behind and can be retried", async ({
  page,
  request,
}) => {
  const catalog = (await (
    await request.get("/__test/health")
  ).json()) as Catalog
  const user = await createUser(request)
  const purchase = await openPayment(page, catalog.card_monthly_price_id, user)
  await nextCard(page, "0002")
  await purchase.getByRole("button", { name: "Subscribe", exact: true }).click()
  // OpenRails classifies the decline; the page shows its message.
  await expect(purchase.getByRole("alert")).toContainText(/insufficient funds/i)

  const declined = await billing(request, user.id)
  expect(declined.subscriptions).toHaveLength(0)
  expect(declined.payment_methods).toHaveLength(0)
  expect(await completed(page)).toEqual([])

  await nextCard(page, "4242")
  await purchase.getByRole("button", { name: "Subscribe", exact: true }).click()
  await expect(purchase.getByText("Payment complete").first()).toBeAttached()
  const held = await billing(request, user.id)
  expect(held.subscriptions).toHaveLength(1)
  expect(held.payment_methods).toHaveLength(1)
})

test("a new card pays a one-time price", async ({ page, request }) => {
  const catalog = (await (
    await request.get("/__test/health")
  ).json()) as Catalog
  const user = await createUser(request)
  const purchase = await openPayment(page, catalog.card_once_price_id, user)
  await purchase.getByRole("button", { name: "Pay", exact: true }).click()
  await expect(purchase.getByText("Payment complete").first()).toBeAttached()
  await expect.poll(() => completed(page)).toEqual(["complete"])
  const held = await billing(request, user.id)
  expect(held.subscriptions).toHaveLength(0)
})

// A server error is shown at once; nothing retries behind the buyer, and
// paying again completes.
test("a server error is shown at once and paying again succeeds", async ({
  page,
  request,
}) => {
  const catalog = (await (
    await request.get("/__test/health")
  ).json()) as Catalog
  const user = await createUser(request)
  const purchase = await openPayment(page, catalog.card_once_price_id, user)
  let failures = 0
  await page.route("**/billing/v1/me/orders/*/pay", (route) => {
    if (failures++ > 0) return route.continue()
    return route.fulfill({
      status: 500,
      contentType: "application/json",
      body: JSON.stringify({
        error: {
          type: "api_error",
          code: "internal_error",
          message: "internal error",
        },
      }),
    })
  })
  const started = Date.now()
  await purchase.getByRole("button", { name: "Pay", exact: true }).click()
  await expect(purchase.getByRole("alert")).toBeVisible({ timeout: 2_000 })
  expect(Date.now() - started).toBeLessThan(2_000)
  expect(failures).toBe(1)
  await purchase.getByRole("button", { name: "Pay", exact: true }).click()
  await expect(purchase.getByText("Payment complete").first()).toBeAttached()
})

test("OrderPayment takes the app's theme", async ({ page, request }) => {
  const catalog = (await (
    await request.get("/__test/health")
  ).json()) as Catalog
  const user = await createUser(request)
  const purchase = await openPayment(page, catalog.card_once_price_id, user, {
    theme: "dark",
  })
  await expect(
    purchase.locator('[data-orck-theme="dark"]').first()
  ).toBeAttached()
})
