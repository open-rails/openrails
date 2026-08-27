import { expect, test } from "@playwright/test"

test("CCBill navigates the merchant host from the payment origin iframe", async ({
  page,
}) => {
  const browserErrors: Error[] = []
  page.on("pageerror", (error) => browserErrors.push(error))
  await page.route("https://merchant.example/**", (route) =>
    route.fulfill({
      contentType: "text/html",
      body: "<!doctype html><title>CCBill handoff</title><h1>CCBill handoff</h1>",
    })
  )

  await page.goto("/e2e/fixtures/host.html")
  const checkout = page.frameLocator('iframe[title="Secure checkout"]')
  await checkout.getByLabel("Name on card").fill("Jane Tester")
  await checkout.getByLabel("Country").selectOption("US")
  await checkout.getByLabel("ZIP code").fill("62704")
  await expect(checkout.getByLabel("Email")).toHaveCount(0)
  await expect(checkout.getByLabel("Address")).toHaveCount(0)
  await expect(checkout.getByLabel("City")).toHaveCount(0)
  await expect(checkout.getByLabel("State / region (optional)")).toHaveCount(0)

  await Promise.all([
    page.waitForURL("https://merchant.example/ccbill/complete"),
    checkout.getByRole("button", { name: "Continue to CCBill" }).click(),
  ])

  await expect(
    page.getByRole("heading", { name: "CCBill handoff" })
  ).toBeVisible()
  expect(browserErrors).toEqual([])
})
