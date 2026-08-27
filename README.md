# openrails-checkout

Embeddable React checkout components for OpenRails.

The package owns the browser checkout flow while the host supplies a
short-lived `CheckoutSource`. Card data is tokenized in NMI-hosted Collect.js
iframes and never enters the host application.

See [Payment form contract](docs/payment-form-contract.md) for billing fields,
browser-autofill behavior, and the checkout request schema.

```tsx
import { Checkout, createHttpSource } from "openrails-checkout"
import "openrails-checkout/styles.css"

const source = createHttpSource({
  baseUrl: "https://merchant.example",
  sessionId: "ocs_example",
})

export function PaymentPage() {
  return <Checkout source={source} />
}
```
