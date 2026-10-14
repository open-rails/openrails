// <ProductList products> against the real server: the host's server reads its
// products with the Go client and answers them, the browser never reads the
// catalog, a signed-out visitor is sent to sign in, and a signed-in customer
// creates the order on /v1/me and pays it on the page (the page is not a site
// the merchant registered to frame its purchase). The loopback NMI gateway
// charges; Collect.js is a stand-in.
import { expect, test, type Page } from "@playwright/test"

import { createUser } from "./api"

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
    setTimeout(() => this.config.callback({
      token: "e2e-" + crypto.randomUUID() + "-4242",
      card: { number: "4xxxxxxxxxxx4242", type: "visa", exp: "1235" },
    }), 0)
  },
}
`

// Every billing request the page sends, by path.
function billingReads(page: Page) {
  const paths: string[] = []
  page.on("request", (r) => {
    const url = new URL(r.url())
    if (url.pathname.startsWith("/billing/"))
      paths.push(`${r.method()} ${url.pathname}`)
  })
  return paths
}

const windowList = (page: Page, name: "paid" | "signIns") =>
  page.evaluate(
    (key) => (window as unknown as Record<string, string[]>)[key],
    name
  )

test("ProductList sells the host's products and sends a signed-out visitor to sign in", async ({
  page,
}) => {
  const reads = billingReads(page)
  await page.goto("/purchase.html#keys=e2e-card,e2e-lifetime")

  await expect(page.getByTestId("product")).toHaveCount(2)
  await expect(
    page.getByRole("heading", { name: "Card membership" })
  ).toBeVisible()
  await expect(
    page.getByRole("heading", { name: "Lifetime pass" })
  ).toBeVisible()
  await expect(
    page.getByRole("button", { name: "$4.99 every 30 days" })
  ).toBeVisible()
  await page.getByRole("button", { name: "$19.99" }).click()
  await expect.poll(() => windowList(page, "signIns")).toEqual(["sign-in"])
  expect(reads.filter((r) => !r.endsWith("/config"))).toEqual([])
})

test("a signed-in customer orders the host's price and pays it on the page", async ({
  page,
  request,
}) => {
  const user = await createUser(request)
  await page.route(
    "https://secure.networkmerchants.com/token/Collect.js",
    (route) =>
      route.fulfill({ contentType: "application/javascript", body: COLLECT_JS })
  )
  const reads = billingReads(page)
  await page.goto(`/purchase.html#keys=e2e-card&token=${user.access_token}`)
  await page.getByRole("button", { name: "Rent for 30 days, $2.99" }).click()

  const dialog = page.getByRole("dialog")
  await dialog.getByLabel("Full name").fill("Card Payer")
  await dialog.getByLabel("Country").selectOption("US")
  await dialog.getByLabel(/ZIP/).fill("10001")
  await dialog.getByRole("button", { name: "Pay", exact: true }).click()
  await expect.poll(() => windowList(page, "paid")).toEqual(["complete"])

  expect(reads).toContain("POST /billing/v1/me/orders")
  expect(reads).toContain("GET /billing/v1/me/config")
  expect(reads.filter((r) => r.includes("/purchase"))).toEqual([])
  expect(reads.filter((r) => r.includes("/products"))).toEqual([])
})

test("a reload answers the customer's open order, storing nothing", async ({
  page,
  request,
}) => {
  const user = await createUser(request)
  const placed = async () => {
    const [res] = await Promise.all([
      page.waitForResponse(
        (r) =>
          r.request().method() === "POST" &&
          new URL(r.url()).pathname === "/billing/v1/me/orders"
      ),
      page.getByRole("button", { name: "$4.99 every 30 days" }).click(),
    ])
    expect(res.request().headers()["idempotency-key"]).toBeUndefined()
    return ((await res.json()) as { id: string }).id
  }
  await page.goto(`/purchase.html#keys=e2e-card&token=${user.access_token}`)
  const first = await placed()
  await expect(page.getByRole("dialog")).toBeVisible()

  await page.reload()
  expect(await placed()).toBe(first)
  await expect(page.getByRole("dialog")).toBeVisible()
  expect(await page.evaluate(() => sessionStorage.length)).toBe(0)
})

test("no public route reads the catalog", async ({ request }) => {
  const seeded = (await (await request.get("/__test/health")).json()) as {
    merchant_id: string
  }
  const res = await request.get(
    `/billing/v1/products?merchant=${seeded.merchant_id}&keys=e2e-card`
  )
  expect(res.status()).toBe(404)
})
