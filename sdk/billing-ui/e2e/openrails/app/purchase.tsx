// Host page for purchase.spec.ts: the host's server answers the products it
// sells (/api/products), and <ProductList products> from the packaged entry
// sells them. The browser never reads the catalog.
import { createRoot } from "react-dom/client"

import "../../../dist/styles.css"
import {
  BillingProvider,
  ProductList,
  createBillingClient,
} from "../../../dist/index.js"
import type { Product } from "../../../dist/client.js"

const params = new URLSearchParams(location.hash.slice(1))
const token = params.get("token")
const paid: string[] = []
const signIns: string[] = []
Object.assign(window, { paid, signIns })

const client = createBillingClient({ getToken: () => token })

async function main() {
  const res = await fetch(`/api/products?keys=${params.get("keys") ?? ""}`)
  const { products } = (await res.json()) as { products: Product[] }
  createRoot(document.getElementById("root")!).render(
    <BillingProvider client={client} locale="en-US">
      <main style={{ maxWidth: 720, margin: "0 auto", padding: "32px 16px" }}>
        <ProductList
          products={products}
          signedIn={token !== null}
          onSignInRequired={() => signIns.push("sign-in")}
          onPaid={(order) => paid.push(order.status)}
        />
      </main>
    </BillingProvider>
  )
}

void main()
