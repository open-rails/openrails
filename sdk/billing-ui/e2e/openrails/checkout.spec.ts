// The packaged Checkout renders exactly what OpenRails advertises (#1078):
// with a Solana PSP configured, Solana is offered for one-time and recurring
// prices, and selecting it opens a Solana Pay request from the real engine.
import { expect, test } from "@playwright/test"

import { createUser } from "./api"

type Catalog = {
  crypto_pass_price_id: string
  crypto_monthly_price_id: string
}

for (const [name, key, mode] of [
  ["recurring", "crypto_monthly_price_id", "subscription"],
  ["one-time", "crypto_pass_price_id", "one_off"],
] as const) {
  test(`Solana is offered for a ${name} price when configured`, async ({
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
    await page.goto(`/checkout.html#price=${catalog[key]}&customer=${user.id}`)

    const offer = await page.evaluate(
      () =>
        (window as unknown as { checkoutOffer: { options: unknown[] } })
          .checkoutOffer
    )
    expect(offer.options).toContainEqual(
      expect.objectContaining({
        rail: "solana",
        mode,
        driver: "solana_pay",
        public_config: {
          token_symbol: "DUSD",
          token_name: "Dev USD",
          network: "devnet",
        },
      })
    )

    // Solana is the only armed rail here, so its Solana Pay request opens at
    // once: a real engine session, rendered as a QR code in DUSD.
    await expect(page.getByText("Scan with a Solana wallet")).toBeVisible()
    await expect(page.getByText(/DUSD · Watching for payment…/)).toBeVisible()
    await expect(
      page.getByRole("img", { name: "Solana Pay QR code" })
    ).toBeVisible()
  })
}
