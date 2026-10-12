// OrderPayment pays what OpenRails offers: with a Solana PSP configured, an
// order for a one-time price takes a Solana wallet, and choosing it opens a
// Solana Pay request from the real engine.
import { expect, test } from "@playwright/test"

import { createUser } from "./api"

type Catalog = { crypto_pass_price_id: string }

test("Solana pays a one-time order when configured", async ({
  page,
  request,
}) => {
  const catalog = (await (
    await request.get("/__test/health")
  ).json()) as Catalog
  const user = await createUser(request)
  // A server error is printed with its body, so a failure names its cause.
  page.on("response", async (response) => {
    if (response.status() >= 500) {
      console.error(
        `${response.status()} ${response.url()}: ${await response.text()}`
      )
    }
  })
  await page.goto(
    `/purchase.html#price=${catalog.crypto_pass_price_id}&token=${user.access_token}`
  )
  const order = (await (
    await page.waitForFunction(
      () => (window as unknown as { purchaseOrder?: unknown }).purchaseOrder
    )
  ).jsonValue()) as { payment_options: { rail: string; accepts: string[] }[] }
  expect(order.payment_options).toContainEqual(
    expect.objectContaining({ rail: "solana", accepts: ["solana_pay"] })
  )

  await page
    .getByRole("button", { name: /^Pay with .* on Solana$/ })
    .first()
    .click()
  await expect(
    page.getByRole("img", { name: "Solana Pay QR code" })
  ).toBeVisible()
})
