import { expect, test, type Page } from "@playwright/test"

const api = () => process.env.E2E_API!

interface Created {
  order_id: string
  customer_id: string
  url: string
}

async function newOrder(body: { saved_payment_methods?: boolean; cancel_url?: string } = {}): Promise<Created> {
  const res = await fetch(`${api()}/__test/orders`, { method: "POST", body: JSON.stringify(body) })
  expect(res.status).toBe(201)
  return (await res.json()) as Created
}

// Every Content-Security-Policy violation the page reports.
async function watchCSP(page: Page): Promise<() => Promise<string[]>> {
  await page.addInitScript(() => {
    const seen: string[] = []
    ;(window as unknown as { __csp: string[] }).__csp = seen
    document.addEventListener("securitypolicyviolation", (e) => seen.push(`${e.violatedDirective} ${e.blockedURI} ${e.sourceFile}:${e.lineNumber}`))
  })
  return () => page.evaluate(() => (window as unknown as { __csp: string[] }).__csp)
}

test("shows the merchant and the order, and keeps the secret out of the address bar", async ({ page }) => {
  const violations = await watchCSP(page)
  const order = await newOrder()
  await page.goto(order.url)
  await expect(page.getByRole("heading", { name: "Checkout Shop" })).toBeVisible()
  await expect(page.getByLabel("Order summary")).toContainText("Course")
  await expect(page.getByLabel("Order summary")).toContainText("$12.50")
  expect(new URL(page.url()).hash).toBe("")
  expect(await violations()).toEqual([])

  // A reload in the same tab still opens the order.
  await page.reload()
  await expect(page.getByLabel("Order summary")).toContainText("$12.50")
})

test("a wrong secret opens nothing", async ({ page }) => {
  const order = await newOrder()
  const [base] = order.url.split("#")
  await page.goto(`${base}#cks_${"A".repeat(43)}`)
  await expect(page.getByRole("status")).toHaveText("This checkout link is not valid.")
  await expect(page.getByLabel("Order summary")).toHaveCount(0)
})

test("an expired checkout says so", async ({ page }) => {
  const order = await newOrder()
  const res = await fetch(`${api()}/__test/clock`, { method: "POST", body: JSON.stringify({ seconds: 3601 }) })
  expect(res.status).toBe(200)
  await page.goto(order.url)
  await expect(page.getByRole("status")).toHaveText(/This checkout has expired/)
})

test("the page refuses to be framed", async ({ page }) => {
  const order = await newOrder()
  await page.goto(`${api()}/__test/frame?src=${encodeURIComponent(order.url)}`)
  const frame = page.frameLocator("iframe")
  await expect(frame.getByRole("heading", { name: "Checkout Shop" })).toHaveCount(0)
  await page.waitForTimeout(500)
  await expect(frame.getByRole("heading", { name: "Checkout Shop" })).toHaveCount(0)
})
