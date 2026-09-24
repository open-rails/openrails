// The styled account surface against real AuthKit + OpenRails: list, cancel,
// resume, card removal refusal and history. Card deletion itself calls NMI,
// which the harness has no credentials for; jsdom tests cover that success
// path (src/account/account.test.tsx).
import { expect, test, type Page } from "@playwright/test"

import { createUser, seedBilling } from "./api"

const shots = process.env.BILLING_UI_SCREENSHOTS

async function open(page: Page, token: string, theme: string) {
  await page.goto(`/account.html#token=${token}&theme=${theme}`)
  await expect(page.getByTestId("subscription-row")).toHaveCount(1)
  await expect(page.getByTestId("payment-row")).toHaveCount(1)
}

test("customer manages a subscription from the account page", async ({
  page,
  request,
}) => {
  const user = await createUser(request)
  await seedBilling(request, user.id)
  await open(page, user.access_token, "light")

  const sub = page.getByTestId("subscription-row")
  await expect(sub).toContainText("Membership")
  await expect(sub).toContainText("Active")
  await expect(sub).toContainText("$9.99")
  // The seeded price is 720h: 30 days, not a calendar month.
  await expect(sub).toContainText("every 30 days")
  await expect(sub).not.toContainText(/month/i)
  await expect(sub).toContainText("Visa •••• 4242")
  await expect(sub).toContainText(/Renews \w{3} \d{1,2}, \d{4}/)

  const card = page.getByTestId("payment-method-row")
  await expect(card).toContainText("Visa •••• 4242")

  const payment = page.getByTestId("payment-row")
  await expect(payment.getByTestId("payment-item")).toHaveText("Membership")
  await expect(payment.getByTestId("payment-period")).toHaveText(
    "every 30 days"
  )
  await expect(payment).toContainText("Paid")
  await expect(payment).toContainText("$9.99")

  if (shots)
    await page.screenshot({
      path: `${shots}/account-light.png`,
      fullPage: true,
    })

  // Cancel: feedback is required, then the queued change settles.
  await sub.getByRole("button", { name: "Cancel Membership" }).click()
  const dialog = page.getByRole("alertdialog")
  await expect(dialog).toContainText("Cancel Membership?")
  if (shots) await page.screenshot({ path: `${shots}/cancel-dialog-light.png` })
  await dialog.getByRole("button", { name: "Cancel subscription" }).click()
  await expect(dialog).toContainText("Enter at least 4 characters.")
  await dialog.getByLabel("Why are you cancelling?").fill("e2e: testing cancel")
  await dialog.getByRole("button", { name: "Cancel subscription" }).click()
  await expect(dialog).toBeHidden({ timeout: 20_000 })
  await expect(page.getByText("Cancellation requested.")).toBeVisible()
  await expect(sub).toContainText("Ending")
  await expect(sub).toContainText(/Access until \w{3} \d{1,2}, \d{4}/)
  await expect(sub.getByRole("button", { name: /^Cancel/ })).toHaveCount(0)
  if (shots) await sub.screenshot({ path: `${shots}/ending-light.png` })

  await sub.getByRole("button", { name: "Resume" }).click()
  await expect(sub).toContainText("Active", { timeout: 20_000 })
  await expect(sub.getByRole("button", { name: "Resume" })).toHaveCount(0)
  await expect(page.getByText("Your subscription will continue.")).toBeVisible()

  // One saved card: the change-card dialog says how to add another.
  await sub.getByRole("button", { name: "Change card for Membership" }).click()
  const change = page.getByRole("dialog")
  await expect(change.getByTestId("change-card-option")).toHaveCount(1)
  await expect(change).toContainText("Add another card under Payment methods")
  await expect(
    change.getByRole("button", { name: "Use this card" })
  ).toBeDisabled()
  await change.getByRole("button", { name: "Cancel" }).click()
  await expect(change).toBeHidden()

  // The card pays for the live subscription, so OpenRails refuses removal.
  await card.getByRole("button", { name: "Remove Visa •••• 4242" }).click()
  const remove = page.getByRole("alertdialog")
  await remove.getByRole("button", { name: "Remove card" }).click()
  await expect(remove.getByRole("alert")).toContainText(
    "pays for an active subscription"
  )
  await remove.getByRole("button", { name: "Cancel" }).click()
  await expect(card).toBeVisible()

  expect(
    await page.evaluate(
      () => (window as unknown as { billingChanges: string[] }).billingChanges
    )
  ).toEqual(["subscription.cancelled", "subscription.resumed"])
})

test("renders the dark theme and a narrow viewport", async ({
  page,
  request,
}) => {
  const user = await createUser(request)
  await seedBilling(request, user.id)
  await open(page, user.access_token, "dark")
  const bg = await page
    .getByTestId("subscriptions-panel")
    .evaluate(
      (el) =>
        getComputedStyle(el.querySelector("[data-slot=card]")!).backgroundColor
    )
  expect(bg).not.toBe("rgb(255, 255, 255)")
  if (shots)
    await page.screenshot({ path: `${shots}/account-dark.png`, fullPage: true })

  await page.setViewportSize({ width: 375, height: 800 })
  const overflow = await page.evaluate(
    () => document.documentElement.scrollWidth - window.innerWidth
  )
  expect(overflow).toBeLessThanOrEqual(0)
  if (shots)
    await page.screenshot({
      path: `${shots}/account-mobile-dark.png`,
      fullPage: true,
    })
})

test("inherits the host page's palette and dark class", async ({
  page,
  request,
}) => {
  const user = await createUser(request)
  await seedBilling(request, user.id)
  const cardBackground = () =>
    page
      .getByTestId("subscriptions-panel")
      .evaluate(
        (el) =>
          getComputedStyle(el.querySelector("[data-slot=card]")!)
            .backgroundColor
      )
  await open(page, user.access_token, "inherit")
  expect(await cardBackground()).toBe("rgb(250, 240, 230)")
  await page.evaluate(() => document.documentElement.classList.add("dark"))
  expect(await cardBackground()).toBe("rgb(30, 20, 10)")
})
