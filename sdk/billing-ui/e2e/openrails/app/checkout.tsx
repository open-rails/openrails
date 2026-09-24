// Host checkout page for checkout.spec.ts: it serves OpenRails' advertised
// options to the packaged Checkout unchanged, with no rail logic of its own.
import { createRoot } from "react-dom/client"

import {
  BillingUiProvider,
  Checkout,
  checkoutRails,
  type CheckoutRailOffer,
  type CheckoutSession,
} from "../../../dist/index.js"

const params = new URLSearchParams(location.hash.slice(1))
const price = params.get("price") ?? ""
const customer = params.get("customer") ?? ""

type Offer = {
  plan: CheckoutSession["plan"]
  options: CheckoutRailOffer[]
}

async function main() {
  const offer: Offer = await fetch(
    `/__test/checkout/${encodeURIComponent(price)}`
  ).then((res) => res.json())

  const session: CheckoutSession = {
    id: `e2e-${price}`,
    status: "created",
    merchant: { display_name: "billing-ui e2e" },
    plan: offer.plan,
    rails: checkoutRails(offer.options),
  }

  const source = {
    getSession: async () => session,
    pay: async (request: { option_id: string; token_symbol?: string }) => {
      const option = offer.options.find((o) => o.psp_id === request.option_id)
      const res = await fetch("/__test/checkout/pay", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          customer_id: customer,
          price_id: price,
          selector: option?.selector,
          psp_id: option?.psp_id,
          token_symbol: request.token_symbol,
        }),
      })
      const body = await res.json()
      if (!res.ok) throw new Error(body.error ?? "pay failed")
      return body
    },
  }
  Object.assign(window, { checkoutOffer: offer })

  createRoot(document.getElementById("root")!).render(
    <BillingUiProvider appearance={{ theme: "light" }} locale="en-US">
      <main style={{ maxWidth: 960, margin: "0 auto", padding: "32px 16px" }}>
        <Checkout source={source} />
      </main>
    </BillingUiProvider>
  )
}

void main()
