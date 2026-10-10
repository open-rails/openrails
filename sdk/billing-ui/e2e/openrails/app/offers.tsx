// Host page for offers.spec.ts: <Offers keys> from the packaged entry, as an
// app imports it, over the real public catalog. The visitor is signed out.
import { createRoot } from "react-dom/client"

import {
  BillingProvider,
  Offers,
  createBillingClient,
} from "../../../dist/index.js"

const keys = new URLSearchParams(location.hash.slice(1)).get("keys")
const signIns: string[] = []
Object.assign(window, { signIns })

createRoot(document.getElementById("root")!).render(
  <BillingProvider
    locale="en-US"
    client={createBillingClient({ getToken: () => null })}
  >
    <main style={{ maxWidth: 720, margin: "0 auto", padding: "32px 16px" }}>
      <Offers
        keys={keys ? keys.split(",") : []}
        signedIn={false}
        onSignInRequired={() => signIns.push("sign-in")}
        onPaid={() => {}}
      />
    </main>
  </BillingProvider>
)
