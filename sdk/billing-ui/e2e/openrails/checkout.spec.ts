// The packaged Checkout renders exactly what OpenRails advertises (#1078):
// with a Solana PSP configured, Solana is offered for one-time and recurring
// prices, and selecting it opens a Solana Pay request from the real engine.
// The app frames the payment page on another origin (#1124).
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
    await page.goto(
      `/checkout.html#price=${catalog[key]}&token=${user.access_token}`
    )
    const session = (await (
      await page.waitForFunction(
        () =>
          (window as unknown as { checkoutSession?: unknown }).checkoutSession
      )
    ).jsonValue()) as { id: string; url: string }
    expect(session.url).toMatch(
      /^http:\/\/127\.0\.0\.1:\d+\/pay\.html#ocs_[0-9a-f]{64}$/
    )

    // The session id alone reads the session.
    const doc = await (
      await request.get(`/billing/v1/checkout-sessions/${session.id}`)
    ).json()
    expect(doc.rails).toContainEqual(
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
    const checkout = page.frameLocator('iframe[title="Secure checkout"]')
    await expect(checkout.getByText("Scan with a Solana wallet")).toBeVisible()
    await expect(
      checkout.getByText(/DUSD · Watching for payment…/)
    ).toBeVisible()
    await expect(
      checkout.getByRole("img", { name: "Solana Pay QR code" })
    ).toBeVisible()
  })
}
