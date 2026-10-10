// The shared payment page (Config.Checkout.PageURL), on its own origin.
import { createRoot } from "react-dom/client"

import { BillingProvider, CheckoutPage } from "../../../dist/index.js"

document.body.style.margin = "0"
createRoot(document.getElementById("root")!).render(
  <BillingProvider locale="en-US">
    <CheckoutPage appearance={{ theme: "light" }} layout="wide" />
  </BillingProvider>
)
