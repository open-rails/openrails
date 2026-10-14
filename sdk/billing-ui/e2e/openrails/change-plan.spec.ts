// Plan change and renewal on the styled account page against real AuthKit +
// OpenRails: a card member upgrades within the tier group from the
// subscription row, and a customer whose subscriptions ended is invited to
// renew. NMI runs on the local mock.
import { expect, test, type APIRequestContext } from "@playwright/test"

import { createUser } from "./api"
import { openPayment } from "./card"

type Catalog = { card_monthly_price_id: string }

type Billing = {
  subscriptions: { status: string; price_id: string }[]
  sales: { amount: string }[]
}

async function heldBy(request: APIRequestContext, customer: string) {
  return (await (
    await request.get(`/__test/customers/${customer}/billing`)
  ).json()) as Billing
}

test("a card member upgrades from the account page, then renews once it ends", async ({
  page,
  request,
}) => {
  const catalog = (await (
    await request.get("/__test/health")
  ).json()) as Catalog
  const user = await createUser(request)
  const purchase = await openPayment(page, catalog.card_monthly_price_id, user)
  await purchase.getByRole("button", { name: "Subscribe", exact: true }).click()
  await expect(purchase.getByText("Payment complete").first()).toBeAttached()

  await page.goto(`/account.html#token=${user.access_token}&theme=light`)
  const row = page.getByTestId("subscription-row")
  await expect(row).toHaveCount(1)
  await expect(page.getByTestId("renewal-prompt")).toHaveCount(0)

  await row
    .getByRole("button", { name: "Change plan for Card membership" })
    .click()
  const dialog = page.getByRole("dialog")
  await expect(dialog).toContainText("Change Card membership")
  const option = dialog.getByTestId("change-plan-option")
  await expect(option).toHaveCount(1)
  await expect(option).toContainText("Card plus · $9.99 every 30 days")
  await expect(option).toContainText(
    /\$\d+\.\d{2} now, then \$9\.99 from \w{3} \d{1,2}, \d{4}/
  )
  const sales = (await heldBy(request, user.id)).sales.length

  await option.click()
  await dialog.getByRole("button", { name: "Change plan", exact: true }).click()
  await expect(dialog).toBeHidden({ timeout: 20_000 })
  await expect(page.getByText("You're now on Card plus.")).toBeVisible()
  await expect(
    page.getByTestId("subscription-row").filter({ hasText: "Card plus" })
  ).toContainText("Active")

  const held = await heldBy(request, user.id)
  expect(held.sales).toHaveLength(sales + 1)
  expect(held.subscriptions.map((s) => s.status).sort()).toEqual([
    "active",
    "canceled",
  ])

  const ended = await request.post(
    `/__test/customers/${user.id}/end-subscriptions`
  )
  expect(ended.status(), await ended.text()).toBe(204)
  await page.reload()
  const prompt = page.getByTestId("renewal-prompt")
  await expect(prompt).toContainText("Renew your subscription")
  await expect(prompt.getByRole("link", { name: "Renew now" })).toHaveAttribute(
    "href",
    "/plans"
  )
  await expect(page.getByRole("button", { name: /^Change plan/ })).toHaveCount(
    0
  )
})
