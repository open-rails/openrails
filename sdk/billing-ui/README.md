# @openrails/billing-ui

Embeddable OpenRails billing UI: the checkout flow and the customer's account
billing (subscriptions, saved cards, payment history).

| Import                            | Contents                                                                           |
| --------------------------------- | ---------------------------------------------------------------------------------- |
| `@openrails/billing-ui/client`    | Framework-free typed client for `/billing/v1/me/*` and the catalog, `BillingError` |
| `@openrails/billing-ui/react`     | `BillingProvider` and headless hooks, without styles                               |
| `@openrails/billing-ui`           | `BillingProvider`, styled checkout and account components, i18n                    |
| `@openrails/billing-ui/locales/*` | `en de es ja ko zh` message bundles                                                |

The checkout owns the browser payment flow, behind `<BuyButton>`, `<Offers>`
and `<OrderCheckout>`: the signed-in customer's order, read and paid with
their own credential. Card data is tokenized in the PSP's own fields
(Collect.js, Stripe Elements) and never enters the host application.

See [Payment form contract](docs/payment-form-contract.md) for exact money,
billing fields, browser-autofill behavior and the payment request.

The package lives in the OpenRails repository and shares its version: each
OpenRails release `vX.Y.Z` publishes `@openrails/billing-ui@X.Y.Z` to npm, with
provenance.

```sh
pnpm add @openrails/billing-ui@X.Y.Z
```

## Buying

A React app imports from one place:

```tsx
import { BillingProvider, BuyButton, Offers, createBillingClient, formatAmount } from "@openrails/billing-ui"

// auth-ui's authFetch attaches the bearer and retries once after a refresh;
// `getToken: () => token` works for any other auth.
const billing = createBillingClient({ baseUrl: "/billing/v1", fetch: auth.authFetch })

<BillingProvider client={billing} appearance={{ theme: "auto" }}>
  <App />
</BillingProvider>
```

Where your backend mints the customer's token, `tokenSource` keeps it: in
memory only, replaced `skew` seconds (60) before it expires with one request
however many calls wait, and once more when a call answers 401
`credential_expired`.

```ts
import { createBillingClient, redeemAssertion, tokenSource } from "@openrails/billing-ui"

const billing = createBillingClient({
  baseUrl: "https://billing.example/v1",
  getToken: tokenSource({
    // { access_token, expires_in } from your backend, or its signed assertion redeemed here.
    fetchToken: async () => {
      const { assertion } = await (await fetch("/api/billing/assertion", { method: "POST" })).json()
      return redeemAssertion({ tokenEndpoint: "https://billing.example/oauth2/token", assertion })
    },
  }),
})
```

`<BuyButton>` buys one price, `<Offers>` the products your server names by
key (a course, its bundle and the membership that also unlocks it; the Go
`Client.ListOffers` finds the products granting an entitlement): a
`BuyButton` per price ("$4.99", "Rent for 3 days, $1.99",
"$10.00 every 30 days"). Both look prices up in the public catalog of the
merchant whose id you pass as `merchant`, signed in or out. The purchase is
theirs from start to payment: a
button creates the customer's order and pays it in a dialog
(`<OrderCheckout>`); a second click before it is paid opens the same order.
Signing in is the host's: with `signedIn={false}`, or when OpenRails answers
401, they call `onSignInRequired` instead. `onPaid` receives the completed
order; it is a hint, so confirm access from your own API (your gate) before
granting anything.

```tsx
<BuyButton
  merchant={merchantId} // your merchant's id, from your server
  product="course-101"
  price="rent" // labelled from the catalog unless you pass label
  signedIn={signedIn}
  onSignInRequired={() => openSignIn()}
  onPaid={() => navigate("/courses/css-101")} // your gate now admits them
/>

<Offers
  merchant={merchantId}
  keys={["course-101", "course-bundle", "channel-membership"]} // from your server, or products={...} from its Client.ListOffers
  signedIn={signedIn}
  onSignInRequired={() => openSignIn()}
  onPaid={() => navigate("/courses/css-101")}
/>
```

`returnUrl` returns the buyer from a redirect step (Stripe's hosted page); it
must be on one of your app's return origins.

For prepaid credits, a fixed pack is another product/price. A
`customer_amount` price takes the buyer's `amount`: collect it on your product
page, and the checkout charges exactly that:

```tsx
import { decimalToAmount } from "@openrails/billing-ui/client"

// `depositPrice` is in a product from listProducts; `currency` from getConfig().currencies.
// `enteredAmount` is the text from an <input inputMode="decimal">, e.g. "100".
const amount = decimalToAmount(enteredAmount, currency.decimals)
const bounds = depositPrice.customer_amount
const valid = amount && bounds && BigInt(amount) >= BigInt(bounds.min_amount) && BigInt(amount) <= BigInt(bounds.max_amount)

<BuyButton merchant={merchantId} product="api-credits" price="deposit" amount={valid ? amount : undefined} label="Add credits" {...handlers} />
```

OpenRails validates the bounds and whole currency minor units before accepting
payment. Fixed prices reject `amount`; their pack benefit can be greater than
the purchase price for a bulk discount. Customer-selected deposits currently
use NMI or Stripe.

A rail's own step is the order's `payment.next_action`: `redirect_to_url` (an
https page, opened in the top window), `solana_pay` (a `solana:` link shown
as a QR code) or `authenticate` (the bank's challenge, run in the page).

### An order your server creates

A server that decides what its customer buys creates the order with the Go
`Client.CreateOrder` (`POST /v1/app/orders`) and hands its id to the
customer's browser, which pays it signed in:

```tsx
<OrderCheckout orderId={orderId} returnUrl={returnUrl} onSettled={(order) => refetchAccess(order)} />
```

`onSettled` follows a complete order, or one processing on an asynchronous
rail. A subscription line names its subscription once paid.

## Account billing

One `BillingProvider`, usually at the app's root, gives every component the
client, the appearance and the words. `@openrails/billing-ui/react` exports the
same provider for an app that uses only the hooks; it loads no stylesheet.

```tsx
import { AccountBilling, BillingProvider, createBillingClient } from "@openrails/billing-ui"
import { de } from "@openrails/billing-ui/locales/de"

const billing = createBillingClient({ baseUrl: "/billing/v1", fetch: auth.authFetch })

<BillingProvider
  client={billing}
  appearance={{ theme: "auto" }}
  messages={de}
  locale="de"
  navigate={navigate}
  onChange={() => queryClient.invalidateQueries({ queryKey: ["billing"] })}
>
  <AccountBilling
    plansHref="/plans"
    defaultCurrency="USD" // offers "Make default"
    sendSolanaTransaction={(tx) => wallet.signAndSend(tx)} // signs a Solana cancel
  />
</BillingProvider>
```

The panels read OpenRails' configuration themselves, once per
`BillingProvider`: the customer's merchant's (`GET /me/config`), where "Add
card" offers each PSP of its `payment` that saves a card in the page, and says
so while the read fails or its card PSPs are temporarily unavailable; and the
deployment's (`GET /config`), whose currency registry amounts use over the
pinned copy. `useCustomerConfig()`, `useConfig()` and `useCurrencyScales()`
read the same copies for host components.

## Catalog and plan changes

The client also reads the public catalog and changes a subscription's plan.
The catalog read and `GET /config` are public routes: the client sends them
no bearer.

| Call                                                                                                | Route                                                           |
| --------------------------------------------------------------------------------------------------- | --------------------------------------------------------------- |
| `listProducts({ merchant, keys })`: the products `merchant` (its id) has on sale named by `keys` (1 to 100 product keys), each with its prices, in one page; `useProducts` reads the same | `GET /products` |
| `getConfig()` (capabilities, `currencies`, `captcha`; `client.currencies` is the pinned registry) | `GET /config` |
| `getCustomerConfig()` (the customer's merchant: `payment` with PSPs, `solana.network` and tokens, and `merchant`, its name) | `GET /me/config` |
| `previewSubscriptionChange(id, { priceId?, quantity? })`                                            | `POST /me/subscriptions/{id}/change/preview`                    |
| `changeSubscription(id, { priceId?, quantity?, idempotencyKey, signature? })`                       | `POST /me/subscriptions/{id}/change`                            |

```ts
const preview = await billing.previewSubscriptionChange(sub.id, { quantity: 5 }) // amount_due_now, effective
const change = await billing.changeSubscription(sub.id, {
  quantity: 5,
  idempotencyKey,
})
```

- A change moves to another price of the tier group (`priceId`), to other
  seats of a per-seat price (`quantity`), or both. More seats and an upgrade
  charge now; fewer seats and a downgrade apply at the next renewal.
- `changeSubscription` is a money write. Mint one `idempotencyKey` per attempt and
  reuse it until the change resolves (`status: "processing"`, a
  `subscription_change_in_flight` refusal, a 5xx or a lost response), so
  OpenRails replays the stored result instead of charging twice. A change the
  card issuer challenges answers `requires_action` with an `authenticate`
  `next_action`: `authenticateChange(client, id, change, psp)` runs it in the
  page and confirms the change (`confirmSubscriptionChange`).
- A rail that needs the customer's own step answers `requires_action` with a
  `next_action`. Solana's is `solana_sign_transactions`:
  `signWalletAction(change.next_action, send)` signs and sends its
  transactions and returns the signature; repeat `changeSubscription` with the same
  key and `signature`. `cancelSubscription(id, { reason, signature? })` works
  the same way: its answer is the subscription, unchanged and carrying
  `next_action` until the signed cancel lands. The hooks run this when given
  `sendTransaction`.

## Provider-neutral flows

OpenRails's browser PSP configs (`GET /me/config`'s `payment.psps`) pick the
browser flow from each PSP's `flow` and public `config`
(Collect.js, native card inputs, Stripe Elements, redirect, wallet). Hosts never branch on a
provider:

- An order's `payment_options` are exactly the armed PSPs whose rail can make
  this sale (Solana for one-time lines when it is configured, never CCBill),
  each with what it `accepts`. `OrderCheckout` renders one panel: the
  customer's saved cards on them, newest first, an inline new card and one Pay
  button. No provider chooser with one rail.
- A new card is charged and saved in one pay; an `authenticate` next action
  runs 3-D Secure in the page, then `confirm`. A decline stays on the panel
  with its message, so the buyer can pick another card under a new
  idempotency key.
- Only PSPs with `checkout !== false` (`checkoutPsps`) take new cards; others
  stay listed so existing cards and subscriptions keep working.
- `<SavePaymentMethod psp={psp} onSaved={(id) => ...} />` saves a card in the
  page (needs `BillingProvider`); `canSavePaymentMethod(psp)` filters PSPs
  that support it. After an off-page verification the provider returns to
  `returnURL(setupId)`; confirm with `client.confirmCardSetup(id)`.
- `defaultCountry` (SavePaymentMethod, AccountBilling)
  preselects the billing country, else the browser locale's region.

- Panels: `SubscriptionsPanel` (cancel, resume, change card),
  `PaymentMethodsPanel`, `PaymentHistory`, `BillingStatusBadge`,
  `CancelSubscriptionDialog`; `AccountBilling` stacks them.
- `renderSubscriptionFooter={(s) => ...}` adds host content under a
  subscription row; `useBillingRefresh()` refetches after host-side changes.
- Hooks: `useSubscriptions` (`cancel`, `resume`, `setPaymentMethod`,
  `changeSubscription`, per-row `pending`, `nextCursor`), `usePaymentMethods` (`add`,
  `remove`, `setDefault`), `usePayments` (cursor pages), `useProducts` (a
  catalog lookup by product key).
  Actions resolve to `null` or a `BillingError`; they never throw.
  `changeSubscription` resolves to the `SubscriptionChange` instead of `null`. Cancel, resume
  and the card change answer the subscription, which replaces the row.
- Money is exact: `/me` amounts are int64 native-unit strings, scaled by the
  OpenRails currency registry (`currencies` option to extend it).
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

`e2e/server` is a Go module that builds the OpenRails at this commit (`replace`
to the repository root) with AuthKit. It serves the
embedded `/billing/v1` API and `/auth/v1` on a throwaway `postgres:18-alpine`
container (Docker and Go required), with test-only `POST /__test/users` (user +
access token) and `POST /__test/users/{id}/billing` (imported subscription,
sale and saved card on a credential-less NMI PSP).

```sh
pnpm test:e2e        # Playwright against the real server
pnpm contract        # regenerate OpenRails' contract files, src/client/generated among them
pnpm contract:check  # fail if a generated contract file is stale
```

`e2e/openrails/account.spec.ts` drives the packaged `AccountBilling` (built from
`dist/`) through list, cancel, resume, the in-use card refusal and history. Set
`BILLING_UI_SCREENSHOTS=<dir>` to capture light, dark and mobile screenshots.
Card deletion calls NMI, which the harness has no credentials for; jsdom tests
cover it.

## License

MIT; see [LICENSE](LICENSE). Only this package is MIT: the rest of the OpenRails repository is under the Functional Source License (FSL-1.1-ALv2).
