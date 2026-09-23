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

## E2E against real OpenRails

`e2e/server` is a Go module pinning OpenRails and AuthKit. It serves the
embedded `/billing/v1` API and `/auth/v1` on a throwaway `postgres:18-alpine`
container (Docker and Go required), with test-only `POST /__test/users` (user +
access token) and `POST /__test/users/{id}/billing` (imported subscription,
sale and saved card on a credential-less NMI PSP).

```sh
pnpm test:e2e        # Playwright against the real server
pnpm contract        # regenerate src/client/generated from the pinned OpenRails
pnpm contract:check  # fail if the generated contract is stale
```
