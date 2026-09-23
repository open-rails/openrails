// Host page for account.spec.ts, built against the packaged dist/ entries.
import { createRoot } from "react-dom/client"

import { AccountBilling, BillingUiProvider } from "../../../dist/index.js"
import { createBillingClient } from "../../../dist/client.js"
import { BillingProvider } from "../../../dist/react.js"

const params = new URLSearchParams(location.hash.slice(1))
const token = params.get("token")
const theme = params.get("theme") === "dark" ? "dark" : "light"
const changes: string[] = []
Object.assign(window, { billingChanges: changes })

document.body.style.background = theme === "dark" ? "#09090b" : "#fafafa"
document.body.style.margin = "0"

const client = createBillingClient({ getToken: () => token })

createRoot(document.getElementById("root")!).render(
  <BillingUiProvider appearance={{ theme }} locale="en-US">
    <BillingProvider client={client} onChange={(c) => changes.push(c.type)}>
      <main style={{ maxWidth: 720, margin: "0 auto", padding: "32px 16px" }}>
        <AccountBilling plansHref="/plans" defaultCurrency="USD" />
      </main>
    </BillingProvider>
  </BillingUiProvider>
)
