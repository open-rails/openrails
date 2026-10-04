// The shared payment page (Config.HTTP.Checkout.PageURL), on its own origin.
import { createRoot } from "react-dom/client"

import { BillingUiProvider, CheckoutPage } from "../../../dist/index.js"

document.body.style.margin = "0"
createRoot(document.getElementById("root")!).render(
  <BillingUiProvider locale="en-US">
    <CheckoutPage appearance={{ theme: "light" }} layout="wide" />
  </BillingUiProvider>
)
