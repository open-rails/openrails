// The payment page on its own origin, over a fixture client. ?app= names the
// origin the session records as its app.
import { createElement } from "react"
import { createRoot } from "react-dom/client"

import { CheckoutPage, fixtureSession } from "../../src/index"
import { createBillingClient } from "../../src/client"

const redirectURL = "https://merchant.example/ccbill/complete"
const app =
  new URLSearchParams(location.search).get("app") ?? "http://127.0.0.1:4173"

const client = createBillingClient({
  fetch: async (_input, init) => {
    if (init?.method === "POST") {
      return Response.json({
        status: "requires_action",
        redirect_url: redirectURL,
      })
    }
    return Response.json(
      fixtureSession({
        id: "ocs_browser_redirect",
        rails: [
          {
            id: "option_ccbill",
            rail: "ccbill",
            mode: "subscription",
            driver: "redirect",
          },
        ],
        saved_methods: [],
        embed_origin: app,
      })
    )
  },
})

createRoot(document.getElementById("root")!).render(
  createElement(CheckoutPage, { client })
)
