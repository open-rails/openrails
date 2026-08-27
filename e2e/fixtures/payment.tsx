import { createElement } from "react"
import { createRoot } from "react-dom/client"

import { Checkout, createHttpSource, fixtureSession } from "../../src/index"

const redirectURL = "https://merchant.example/ccbill/complete"

const source = createHttpSource({
  baseUrl: "",
  sessionId: "ocs_browser_redirect",
  fetch: async (_input, init) => {
    if (init?.method === "POST") {
      return Response.json({
        status: "requires_action",
        redirect_url: redirectURL,
      })
    }
    return Response.json(
      fixtureSession({
        rails: [
          {
            id: "option_ccbill",
            rail: "ccbill",
            mode: "subscription",
            driver: "redirect",
          },
        ],
        saved_methods: [],
      })
    )
  },
})

createRoot(document.getElementById("root")!).render(
  createElement(Checkout, { source })
)
