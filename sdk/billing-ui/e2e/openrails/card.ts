// The card form's Collect.js stand-in and the payment page it fills.
import type { Locator, Page } from "@playwright/test"

import type { TestUser } from "./api"

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

// Opens the order for price as user and fills the new card's details.
export async function openPayment(
  page: Page,
  price: string,
  user: TestUser,
  options: { theme?: string } = {}
): Promise<Locator> {
  await page.route(
    "https://secure.networkmerchants.com/token/Collect.js",
    (route) =>
      route.fulfill({ contentType: "application/javascript", body: COLLECT_JS })
  )
  const theme = options.theme ? `&theme=${options.theme}` : ""
  await page.goto(
    `/payment.html#price=${price}&token=${user.access_token}${theme}`
  )
  const purchase = page.locator(":root")
  await purchase.getByLabel("Full name").fill("Card Payer")
  await purchase.getByLabel("Country or region").selectOption("US")
  await purchase.getByLabel(/ZIP/).fill("10001")
  return purchase
}
