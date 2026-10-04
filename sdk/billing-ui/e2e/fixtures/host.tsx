// The app: frames the payment page on another origin.
import { createElement } from "react"
import { createRoot } from "react-dom/client"

import { CheckoutFrame } from "../../src/index"

const page = new URLSearchParams(location.search).get("page") ?? "payment.html"
createRoot(document.getElementById("root")!).render(
  createElement(CheckoutFrame, {
    url: `http://localhost:4173/e2e/fixtures/${page}#ocs_browser_redirect`,
    minHeight: 900,
  })
)
