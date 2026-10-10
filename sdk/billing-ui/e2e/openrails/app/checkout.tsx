// The checkout behind BuyButton, driven directly: the signed-in customer
// mints a session and frames the shared payment page, or renders Checkout
// itself (#mode=inline, the single-site case). Checkout and the checkout
// calls are internal, so this test page imports them from src.
import { createRoot } from "react-dom/client"

import "../../../dist/styles.css"
import { Checkout } from "../../../src/checkout"
import { CheckoutFrame } from "../../../src/checkout-frame"
import { checkoutOf } from "../../../src/client/checkout"
import { createBillingClient } from "../../../src/client/client"
import type { CheckoutFrameTheme } from "../../../src/frame"
import { BillingProvider } from "../../../src/react/provider"

const params = new URLSearchParams(location.hash.slice(1))
const token = params.get("token")
const inline = params.get("mode") === "inline"
const theme = (params.get("theme") ?? "light") as CheckoutFrameTheme
const completed: string[] = []
Object.assign(window, { checkoutCompleted: completed })

const client = createBillingClient({ getToken: () => token })

async function main() {
  const session = await checkoutOf(client).createCheckoutSession({
    priceId: params.get("price") ?? "",
  })
  Object.assign(window, { checkoutSession: session })
  createRoot(document.getElementById("root")!).render(
    <BillingProvider appearance={{ theme: "light" }} locale="en-US">
      <main style={{ maxWidth: 960, margin: "0 auto", padding: "32px 16px" }}>
        {inline || !session.url ? (
          <Checkout
            source={checkoutOf(client).checkoutSource(session.id)}
            onComplete={(result) => completed.push(result.status)}
          />
        ) : (
          <CheckoutFrame
            url={session.url}
            theme={theme}
            minHeight={120}
            onComplete={(status) => completed.push(status)}
          />
        )}
      </main>
    </BillingProvider>
  )
}

void main()
