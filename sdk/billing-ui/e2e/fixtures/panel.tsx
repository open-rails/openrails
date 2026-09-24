// Design preview of the one-screen card panel in the checkout modal:
// ?renews=0 for a one-time purchase, ?decline=1 to decline the first payment.
import { createElement } from "react"
import { createRoot } from "react-dom/client"

import { CheckoutModal, fixtureSession } from "../../src/index"
import type { CheckoutSource } from "../../src/index"

const params = new URLSearchParams(location.search)
const renews = params.get("renews") !== "0"
let declines = params.get("decline") === "1" ? 1 : 0

const session = fixtureSession({
  merchant: { display_name: "Night Owl Studio" },
  plan: {
    display_name: renews ? "Channel membership" : "Behind the scenes, part 2",
    unit_amount: "7990000",
    currency: "USD",
    unit_decimals: 6,
    period_hours: renews ? 1 : null,
    automatically_renews: renews,
  },
  expires_at: null,
})
const rail = session.rails[0]
const source: CheckoutSource = {
  getSession: async () => ({
    ...session,
    rails: [rail],
    saved_methods: session.saved_methods?.map((method) => ({
      ...method,
      option_id: rail.id,
    })),
  }),
  pay: async () => {
    await new Promise((resolve) => setTimeout(resolve, 400))
    if (declines-- > 0)
      return {
        status: "failed",
        failure: {
          reason: "insufficient_funds",
          message: "Your card has insufficient funds. Try another card.",
          field: "",
        },
      }
    return { status: "succeeded" }
  },
}

createRoot(document.getElementById("root")!).render(
  createElement(CheckoutModal, {
    open: true,
    onOpenChange: () => {},
    source,
    layout: "compact",
    appearance: { theme: "light" },
  })
)
