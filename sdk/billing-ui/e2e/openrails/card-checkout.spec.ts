// A new card entered in the packaged Checkout subscribes in one click (#1085):
// the host relays the Collect.js token and OpenRails saves the card, accepts
// the displayed terms and charges it. The loopback NMI gateway (nmifake)
// declines cards ending 0002; Collect.js is a stand-in served in its place.
import { expect, test, type Page } from "@playwright/test"

import { createUser } from "./api"

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

async function openCheckout(page: Page, price: string, customer: string) {
  await page.route(
    "https://secure.networkmerchants.com/token/Collect.js",
    (route) =>
      route.fulfill({ contentType: "application/javascript", body: COLLECT_JS })
  )
  await page.goto(`/checkout.html#price=${price}&customer=${customer}`)
  const card = page.getByRole("radio", { name: /Card/ })
  if (await card.count()) await card.first().check()
  await page.getByLabel("Name on card").fill("Hosted Payer")
  await page.getByLabel("Country").selectOption("US")
  await page.getByLabel(/ZIP/).fill("10001")
}

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
  await openCheckout(page, catalog.card_monthly_price_id, user.id)
  await expect(
    page.getByText(/You agree to pay .* until you cancel/)
  ).toBeVisible()
  await page.getByRole("button", { name: /^Subscribe for/ }).click()
  await expect(page.getByText("Payment complete").first()).toBeAttached()

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
  await openCheckout(page, catalog.card_monthly_price_id, user.id)
  await page.evaluate(() => {
    ;(window as unknown as { e2eCardLast4: string }).e2eCardLast4 = "0002"
  })
  await page.getByRole("button", { name: /^Subscribe for/ }).click()
  await expect(page.getByRole("alert")).toContainText(/declined/i)

  const declined = await billing(request, user.id)
  expect(declined.subscriptions).toHaveLength(0)
  expect(declined.payment_methods).toHaveLength(0)

  await page.evaluate(() => {
    ;(window as unknown as { e2eCardLast4: string }).e2eCardLast4 = "4242"
  })
  await page.getByRole("button", { name: /^Subscribe for/ }).click()
  await expect(page.getByText("Payment complete").first()).toBeAttached()
  const held = await billing(request, user.id)
  expect(held.subscriptions).toHaveLength(1)
  expect(held.payment_methods).toHaveLength(1)
})

test("a new card pays a one-time price", async ({ page, request }) => {
  const catalog = (await (
    await request.get("/__test/health")
  ).json()) as Catalog
  const user = await createUser(request)
  await openCheckout(page, catalog.card_once_price_id, user.id)
  await page.getByRole("button", { name: /^Pay / }).click()
  await expect(page.getByText("Payment complete").first()).toBeAttached()
  const held = await billing(request, user.id)
  expect(held.subscriptions).toHaveLength(0)
})
