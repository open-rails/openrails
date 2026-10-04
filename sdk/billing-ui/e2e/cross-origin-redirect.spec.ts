// The payment page cannot navigate the app's top window from a nested frame:
// it asks the app it recorded as its session's origin, and only that app.
import { expect, test, type Page } from "@playwright/test"

async function continueToCCBill(page: Page, host: string) {
  await page.route("https://merchant.example/**", (route) =>
    route.fulfill({
      contentType: "text/html",
      body: "<!doctype html><title>CCBill handoff</title><h1>CCBill handoff</h1>",
    })
  )
  await page.goto(host)
  const checkout = page.frameLocator('iframe[title="Secure checkout"]')
  await checkout.getByLabel("Name on card").fill("Jane Tester")
  await checkout.getByLabel("Country").selectOption("US")
  await checkout.getByLabel("ZIP code").fill("62704")
  await expect(checkout.getByLabel("Email")).toHaveCount(0)
  await expect(checkout.getByLabel("Address")).toHaveCount(0)
  return checkout
}

test("CCBill navigates the app from the payment origin frame", async ({
  page,
}) => {
  const browserErrors: Error[] = []
  page.on("pageerror", (error) => browserErrors.push(error))
  const checkout = await continueToCCBill(page, "/e2e/fixtures/host.html")
  await Promise.all([
    page.waitForURL("https://merchant.example/ccbill/complete"),
    checkout.getByRole("button", { name: "Continue to CCBill" }).click(),
  ])
  await expect(
    page.getByRole("heading", { name: "CCBill handoff" })
  ).toBeVisible()
  expect(browserErrors).toEqual([])
})

test("the page messages only the app its session names", async ({ page }) => {
  const page_ = encodeURIComponent(
    "payment.html?app=" + encodeURIComponent("https://elsewhere.example")
  )
  const checkout = await continueToCCBill(
    page,
    `/e2e/fixtures/host.html?page=${page_}`
  )
  await checkout.getByRole("button", { name: "Continue to CCBill" }).click()
  await page.waitForTimeout(1_000)
  expect(new URL(page.url()).pathname).toBe("/e2e/fixtures/host.html")
})
