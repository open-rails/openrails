import { createRoot } from "react-dom/client"
import { BillingProvider, OrderPayment } from "@openrails/billing-ui"
import { createBillingClient } from "@openrails/billing-ui/client"

// The customer's open order, paid on this page through the real router.
const fixture = (await (await fetch("/__fixture/config")).json()) as {
  orderId: string
  token: string
}
const client = createBillingClient({ baseUrl: "/v1", getToken: () => fixture.token })
createRoot(document.getElementById("app")!).render(
  <BillingProvider client={client}>
    <OrderPayment orderId={fixture.orderId} />
  </BillingProvider>
)
document.body.dataset.ready = "true"
