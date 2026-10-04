// The app selling through hosted checkout: the signed-in customer mints a
// session and frames the shared payment page, or renders Checkout itself
// (#mode=inline, the single-site case).
import { createRoot } from "react-dom/client"

import {
  BillingUiProvider,
  Checkout,
  CheckoutFrame,
  type CheckoutFrameTheme,
} from "../../../dist/index.js"
import { createBillingClient } from "../../../dist/client.js"

const params = new URLSearchParams(location.hash.slice(1))
const token = params.get("token")
const inline = params.get("mode") === "inline"
const theme = (params.get("theme") ?? "light") as CheckoutFrameTheme
const completed: string[] = []
Object.assign(window, { checkoutCompleted: completed })

const client = createBillingClient({ getToken: () => token })

async function main() {
  const session = await client.createCheckoutSession({
    priceId: params.get("price") ?? "",
  })
  Object.assign(window, { checkoutSession: session })
  createRoot(document.getElementById("root")!).render(
    <BillingUiProvider appearance={{ theme: "light" }} locale="en-US">
      <main style={{ maxWidth: 960, margin: "0 auto", padding: "32px 16px" }}>
        {inline || !session.url ? (
          <Checkout
            source={client.checkoutSource(session.id)}
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
    </BillingUiProvider>
  )
}

void main()
