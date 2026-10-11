// A new card entered in OrderCheckout subscribes in one click: the customer's
// order is paid with the token, and OpenRails charges and saves the card. The
// loopback NMI gateway (nmimock) declines cards ending 0002; Collect.js is a
// stand-in.
import { expect, test, type Locator, type Page } from "@playwright/test"

import { createUser, type TestUser } from "./api"

type Catalog = {
  card_monthly_price_id: string
  card_once_price_id: string
}

type Billing = {
  subscriptions: { status: string; collection_policy: string }[]
  payment_methods: unknown[]
}

const COLLECT_JS = `
window.CollectJS = {
  configure(config) {
    this.config = config
    for (const field of Object.values(config.fields)) {
      const host = document.querySelector(field.selector)
      if (host && !host.querySelector("input")) {
        const input = document.createElement("input")
        input.setAttribute("aria-label", field.title)
        host.appendChild(input)
      }
    }
    setTimeout(() => {
      config.fieldsAvailableCallback()
      for (const name of ["ccnumber", "ccexp", "cvv"]) config.validationCallback(name, true, "")
    }, 0)
  },
  startPaymentRequest() {
    const last4 = window.e2eCardLast4 || "4242"
    setTimeout(() => this.config.callback({
      token: "e2e-" + crypto.randomUUID() + "-" + last4,
      card: { number: "4xxxxxxxxxxx" + last4, type: "visa", exp: "1235" },
    }), 0)
  },
}
`

// Opens the order for price as user and fills the new card's details.
async function openCheckout(
  page: Page,
  price: string,
  user: TestUser,
  options: { theme?: string } = {}
): Promise<Locator> {
  await page.route(
    "https://secure.networkmerchants.com/token/Collect.js",
    (route) =>
      route.fulfill({ contentType: "application/javascript", body: COLLECT_JS })
  )
  const theme = options.theme ? `&theme=${options.theme}` : ""
  await page.goto(
    `/checkout.html#price=${price}&token=${user.access_token}${theme}`
  )
  const checkout = page.locator(":root")
  await checkout.getByLabel("Name on card").fill("Card Payer")
  await checkout.getByLabel("Country").selectOption("US")
  await checkout.getByLabel(/ZIP/).fill("10001")
  return checkout
}

// The card Collect.js tokenizes next.
async function nextCard(page: Page, last4: string) {
  await page.evaluate((value) => {
    ;(window as unknown as { e2eCardLast4: string }).e2eCardLast4 = value
  }, last4)
}

const completed = (page: Page) =>
  page.evaluate(
    () =>
      (window as unknown as { checkoutCompleted: string[] }).checkoutCompleted
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
  const checkout = await openCheckout(page, catalog.card_monthly_price_id, user)
  await checkout.getByRole("button", { name: /^Pay / }).click()
  await expect(checkout.getByText("Payment complete").first()).toBeAttached()
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
  const checkout = await openCheckout(page, catalog.card_monthly_price_id, user)
  await nextCard(page, "0002")
  await checkout.getByRole("button", { name: /^Pay / }).click()
  // OpenRails classifies the decline; the page shows its message.
  await expect(checkout.getByRole("alert")).toContainText(/insufficient funds/i)

  const declined = await billing(request, user.id)
  expect(declined.subscriptions).toHaveLength(0)
  expect(declined.payment_methods).toHaveLength(0)
  expect(await completed(page)).toEqual([])

  await nextCard(page, "4242")
  await checkout.getByRole("button", { name: /^Pay / }).click()
  await expect(checkout.getByText("Payment complete").first()).toBeAttached()
  const held = await billing(request, user.id)
  expect(held.subscriptions).toHaveLength(1)
  expect(held.payment_methods).toHaveLength(1)
})

test("a new card pays a one-time price", async ({ page, request }) => {
  const catalog = (await (
    await request.get("/__test/health")
  ).json()) as Catalog
  const user = await createUser(request)
  const checkout = await openCheckout(page, catalog.card_once_price_id, user)
  await checkout.getByRole("button", { name: /^Pay / }).click()
  await expect(checkout.getByText("Payment complete").first()).toBeAttached()
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
  const checkout = await openCheckout(page, catalog.card_once_price_id, user)
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
  await checkout.getByRole("button", { name: /^Pay / }).click()
  await expect(checkout.getByRole("alert")).toBeVisible({ timeout: 2_000 })
  expect(Date.now() - started).toBeLessThan(2_000)
  expect(failures).toBe(1)
  await checkout.getByRole("button", { name: /^Pay / }).click()
  await expect(checkout.getByText("Payment complete").first()).toBeAttached()
})

test("OrderCheckout takes the app's theme", async ({ page, request }) => {
  const catalog = (await (
    await request.get("/__test/health")
  ).json()) as Catalog
  const user = await createUser(request)
  const checkout = await openCheckout(page, catalog.card_once_price_id, user, {
    theme: "dark",
  })
  await expect(
    checkout.locator('[data-orck-theme="dark"]').first()
  ).toBeAttached()
})
