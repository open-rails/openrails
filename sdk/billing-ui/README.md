# @openrails/billing-ui

Embeddable OpenRails billing UI: the checkout flow and the customer's account
billing (subscriptions, saved cards, payment history).

| Import                            | Contents                                                           |
| --------------------------------- | ------------------------------------------------------------------ |
| `@openrails/billing-ui/client`    | Framework-free typed client for `/billing/v1/me/*`, `BillingError` |
| `@openrails/billing-ui/react`     | `BillingProvider` and headless hooks                               |
| `@openrails/billing-ui`           | Styled checkout and account components, `BillingUiProvider`, i18n  |
| `@openrails/billing-ui/locales/*` | `en de es ja ko zh` message bundles                                |

The checkout owns the browser payment flow while the host supplies a
short-lived `CheckoutSource`. Card data is tokenized in NMI-hosted Collect.js
iframes and never enters the host application.

See [Payment form contract](docs/payment-form-contract.md) for the exact-money
session document, billing fields, browser-autofill behavior, and the checkout
request schema.

Until the `@openrails` npm scope is live, install the tarball attached to each
GitHub release:

```sh
pnpm add https://github.com/open-rails/billing-ui/releases/download/v0.5.2/openrails-billing-ui-0.5.2.tgz
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

## Account billing

```tsx
import { createBillingClient } from "@openrails/billing-ui/client"
import { BillingProvider } from "@openrails/billing-ui/react"
import { AccountBilling, BillingUiProvider } from "@openrails/billing-ui"
import { de } from "@openrails/billing-ui/locales/de"

// auth-ui's authFetch attaches the bearer and retries once after a refresh;
// `getToken: () => token` works for any other auth.
const billing = createBillingClient({ baseUrl: "/billing/v1", fetch: auth.authFetch })

<BillingUiProvider appearance={{ theme: "auto" }} messages={de} locale="de" navigate={navigate}>
  <BillingProvider client={billing} onChange={() => queryClient.invalidateQueries({ queryKey: ["billing"] })}>
    <AccountBilling
      plansHref="/plans"
      psps={checkoutConfig.psps} // OpenRails checkout config; enables "Add card"
      defaultCurrency="USD" // enables "Make default"
      sendSolanaTransaction={(tx) => wallet.signAndSend(tx)} // Solana-rail cancel
    />
  </BillingProvider>
</BillingUiProvider>
```

## Provider-neutral flows

Hosts pass OpenRails's browser PSP configs (`GET /checkout-config`, `psps`)
through unchanged; the PSP's `flow` and public `config` pick the browser flow
(Collect.js, Stripe Elements, redirect, wallet). Hosts never branch on a
provider:

- `checkoutRails(offers, psps)` and `savedMethodsFor(methods, rails)` build a
  `CheckoutSource`'s `rails` and `saved_methods` (most recent card first).
  Card rails (`collect_js`, `stripe_elements`) render one panel: saved cards,
  an inline new card and one Pay/Subscribe button, which is the payer's
  confirmation of the displayed terms. No provider chooser with one rail.
- Inside a `BillingProvider`, a new card is always saved to the account first
  and the source is paid with `payment_method_id`; `requires_action` results
  carrying `operation_id` run 3-D Secure in the page. A `failed` result stays
  on the panel with `failure.message` next to its `field`, so the buyer can
  pick another card; the host gives each new attempt a new idempotency key.
- Only PSPs with `checkout !== false` (`checkoutPsps`) take new cards; others
  stay listed so existing cards and subscriptions keep working.
- `<SavePaymentMethod psp={psp} onSaved={(id) => ...} />` saves a card in the
  page (needs `BillingProvider`); `canSavePaymentMethod(psp)` filters PSPs
  that support it. After an off-page verification the provider returns to
  `returnURL(setupId)`; confirm with `client.confirmCardSetup(id)`.
- `authenticatePayment(client, operationId, psp)` completes a pending
  payment's 3-D Secure challenge when `canAuthenticatePayment(psp)`.
- `defaultCountry` (Checkout, SavePaymentMethod, AccountBilling) preselects
  the billing country, else the browser locale's region.
- `CheckoutModal gate={<SignIn />}` renders host content (e.g. sign-in) in
  the modal instead of the checkout until cleared.

- Panels: `SubscriptionsPanel` (cancel, resume, change card),
  `PaymentMethodsPanel`, `PaymentHistory`, `BillingStatusBadge`,
  `CancelSubscriptionDialog`; `AccountBilling` stacks them.
- `renderSubscriptionFooter={(s) => ...}` adds host content under a
  subscription row; `useBillingRefresh()` refetches after host-side changes.
- Hooks: `useSubscriptions` (`cancel`, `cancelOnChain`, `resume`,
  `setPaymentMethod`, per-row `pending`), `usePaymentMethods` (`add`, `remove`,
  `setDefault`), `usePayments` (offset pages). Actions resolve to `null` or a
  `BillingError`; they never throw. Cancel and resume are queued by OpenRails
  (202); hooks re-read the subscription until the change shows.
- Money is exact: `/me` amounts are int64 native-unit strings, scaled by the
  pinned OpenRails currency registry (`currencies` option to extend it).
- Messages: English is complete and the fallback; `messages` layers bundles,
  `t` lets the host's i18n win. `useMessages().error(err)` maps error codes.
  Count-dependent messages are CLDR plural nodes (`{ one, other }`, plus an
  exact `"1"` like ICU `=1`), selected with `locale`.
- Periods are exact: OpenRails windows are hours, so 720h reads "every 30
  days", never "monthly"; only exact weeks, days or hours are named.
- Styles are scoped under `.orck`; `appearance.theme` is `light`, `dark`,
  `auto` or `inherit`. `inherit` bundles no palette: the host page's shadcn
  tokens (`--background`, `--primary`, ...) and its `.dark` class apply.

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

`e2e/openrails/account.spec.ts` drives the packaged `AccountBilling` (built from
`dist/`) through list, cancel, resume, the in-use card refusal and history. Set
`BILLING_UI_SCREENSHOTS=<dir>` to capture light, dark and mobile screenshots.
Card deletion calls NMI, which the harness has no credentials for; jsdom tests
cover it.
