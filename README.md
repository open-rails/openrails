# @openrails/billing-ui

Embeddable React checkout components for OpenRails.

The package owns the browser checkout flow while the host supplies a
short-lived `CheckoutSource`. Card data is tokenized in NMI-hosted Collect.js
iframes and never enters the host application.

See [Payment form contract](docs/payment-form-contract.md) for the exact-money
session document, billing fields, browser-autofill behavior, and the checkout
request schema.

Until the `@openrails` npm scope is live, install the tarball attached to each
GitHub release:

```sh
pnpm add https://github.com/open-rails/billing-ui/releases/download/v0.4.2/openrails-billing-ui-0.4.2.tgz
```

```tsx
import { Checkout, createHttpSource } from "@openrails/billing-ui"
import "@openrails/billing-ui/styles.css"

const source = createHttpSource({
  baseUrl: "https://merchant.example",
  sessionId: "ocs_example",
})

export function PaymentPage() {
  return <Checkout source={source} />
}
```

## UI primitives

`src/components/ui/*` is shadcn (`base-vega`, zinc; see `components.json`),
managed with `pnpm dlx shadcn@4.21.0 add <name> --overwrite`, importing `cn`
from the [`cn`](https://github.com/shadcn-ui/cn) package.
