// The purchase behind BuyButton, driven directly from the packaged entry:
// the signed-in customer creates an order for one price and pays it in
// OrderPayment.
import { createRoot } from "react-dom/client"

import "../../../dist/styles.css"
import {
  BillingProvider,
  OrderPayment,
  createBillingClient,
} from "../../../dist/index.js"

const params = new URLSearchParams(location.hash.slice(1))
const token = params.get("token")
const theme = params.get("theme") === "dark" ? "dark" : "light"
const completed: string[] = []
Object.assign(window, { paymentCompleted: completed })

const client = createBillingClient({ getToken: () => token })

async function main() {
  const { order } = await client.createOrder({
    lines: [{ price_id: params.get("price") ?? "" }],
    idempotencyKey: crypto.randomUUID(),
  })
  Object.assign(window, { purchaseOrder: order })
  createRoot(document.getElementById("root")!).render(
    <BillingProvider client={client} appearance={{ theme }} locale="en-US">
      <main style={{ maxWidth: 520, margin: "0 auto", padding: "32px 16px" }}>
        <OrderPayment
          orderId={order.id}
          appearance={{ theme }}
          onSettled={(paid) => completed.push(paid.status)}
        />
      </main>
    </BillingProvider>
  )
}

void main()
