// Host checkout page for the checkout specs: it serves OpenRails' advertised
// options to the packaged Checkout unchanged, with no rail logic of its own.
import { createRoot } from "react-dom/client"

import {
  BillingUiProvider,
  Checkout,
  checkoutRails,
  type CheckoutRailOffer,
  type CheckoutSession,
  type PayRequest,
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

  // One host attempt key per payment; a declined attempt starts a new one.
  let attempt = crypto.randomUUID()
  const source = {
    getSession: async () => session,
    pay: async (request: PayRequest) => {
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
          payment_token: request.payment_token,
          name_on_card: request.name_on_card,
          zip: request.zip,
          country: request.country,
          idempotency_key: attempt,
        }),
      })
      const body = await res.json()
      if (!res.ok) throw new Error(body.error ?? "pay failed")
      if (body.status === "failed") attempt = crypto.randomUUID()
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
