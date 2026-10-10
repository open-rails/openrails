# @openrails/billing-ui

Embeddable OpenRails billing UI: the checkout flow and the customer's account
billing (subscriptions, saved cards, payment history).

| Import                            | Contents                                                                           |
| --------------------------------- | ---------------------------------------------------------------------------------- |
| `@openrails/billing-ui/client`    | Framework-free typed client for `/billing/v1/me/*` and the catalog, `BillingError` |
| `@openrails/billing-ui/react`     | `BillingProvider` and headless hooks, without styles                               |
| `@openrails/billing-ui`           | `BillingProvider`, styled checkout and account components, i18n                    |
| `@openrails/billing-ui/locales/*` | `en de es ja ko zh` message bundles                                                |

The checkout owns the browser payment flow, behind `<BuyButton>` and
`<Offers>`. OpenRails holds the checkout session: the signed-in customer mints
one for a price, and its `ocs_` id alone reads and pays it. Card data is tokenized in NMI-hosted Collect.js iframes and
never enters the host application, unless the PSP takes cards on OpenRails
itself (`card_entry: server`, driver `card`).

See [Payment form contract](docs/payment-form-contract.md) for the exact-money
session document, billing fields, browser-autofill behavior, and the checkout
request schema.

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

`<BuyButton>` buys one price, `<Offers>` everything on sale that grants any of
some entitlements (a course, and the membership that also unlocks it): a
`BuyButton` per price ("$4.99", "Rent for 3 days, $1.99",
"$10.00 every 30 days"). The purchase is theirs from start to payment, so the
app never handles a checkout session: today a button opens one in a checkout
dialog; when orders take a new card it will create an order instead,
and apps won't change. Signing in is the host's: with `signedIn={false}`, or
when OpenRails answers 401, they call `onSignInRequired` instead. `onPaid`
follows a successful payment only; it is a hint, so confirm access from your
own API (your gate) before granting anything.

```tsx
<BuyButton
  product="course-101"
  price="rent" // labelled from the catalog unless you pass label
  signedIn={signedIn}
  onSignInRequired={() => openSignIn()}
  onPaid={() => navigate("/courses/css-101")} // your gate now admits them
/>

<Offers
  entitlements={["course:101", "channel:membership"]} // or products={...} from your server's Client.ListOffers
  signedIn={signedIn}
  onSignInRequired={() => openSignIn()}
  onPaid={() => navigate("/courses/css-101")}
/>
```

`successUrl` returns the buyer from a redirect step (Stripe's hosted page); it
must be on one of your app's return origins. `autoRenew={false}` buys a
recurring price's first term only.

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

<BuyButton product="api-credits" price="deposit" amount={valid ? amount : undefined} label="Add credits" {...handlers} />
```

OpenRails validates the bounds and whole currency minor units before accepting
payment. Fixed prices reject `amount`; their pack benefit can be greater than
the purchase price for a bulk discount. Customer-selected deposits currently
use NMI or Stripe.

A rail's own step is the pay result's `next_action`: `redirect_to_url` (an
https page, opened in the top window) or `solana_pay` (a `solana:` link shown
as a QR code). A card challenge is its `operation`, authenticated in the page.

### A shared payment page

Several sites selling for one merchant can share one payment page
(`Config.Checkout.PageURL`). A server mints the session with the Go
`Client.CreateCheckoutSession` and hands the browser its `url`, which the site
frames:

```tsx
<CheckoutFrame url={session.url} theme="dark" onComplete={() => refetchAccess()} />
```

Stripe Elements needs the buyer's own client, so the shared page offers token
rails only.

The payment host serves `<CheckoutPage>` from one HTML entry at `PageURL`,
behind its adapter's `CheckoutFramePolicy` (only the sites in
`Config.Checkout.EmbedOrigins` may frame it). No customer signs in there, so
its `BillingProvider` has no client:

```tsx
import { BillingProvider, CheckoutPage } from "@openrails/billing-ui"

createRoot(root).render(
  <BillingProvider>
    <CheckoutPage appearance={{ variables: brand }} />
  </BillingProvider>
)
```

The page reads the session id from its URL fragment and talks to its frame
with `postMessage`, origin-checked both ways: page to app `ready`,
`resize {height}`, `complete {status}` and `redirect {url}` (a nested frame
cannot navigate the top window, so the app does); app to page
`init {theme}`. The page messages only the app origin recorded on its session.

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

The panels read OpenRails' public configuration (`GET /config`) themselves,
once per `BillingProvider`: "Add card" offers each PSP of its `payment` that
saves a card in the page, and says so while the configuration fails or its
card PSPs are temporarily unavailable; amounts use its currency registry over
the pinned copy. `useConfig()` and `useCurrencyScales()` read the same copy
for host components.

## Catalog and plan changes

The client also reads the public catalog and changes a subscription's plan.
The catalog and Solana reads are public routes, served only when the host
mounts OpenRails' checkout routes; `GET /config` is always served.

| Call                                                                                                | Route                                                           |
| --------------------------------------------------------------------------------------------------- | --------------------------------------------------------------- |
| `listProducts({ entitlements?, keys? })`: the products on sale granting any of `entitlements` and named by `keys` (one is required), each with its prices; `useProducts` reads the same | `GET /catalog/products` |
| `getConfig()` (capabilities, `currencies`, `payment` with PSPs, `solana.network` and tokens; `client.currencies` is the pinned registry) | `GET /config` |
| `previewSubscriptionChange(id, { priceId?, quantity? })`                                            | `POST /me/subscriptions/{id}/change/preview`                    |
| `changeSubscription(id, { priceId?, quantity?, idempotencyKey, signature? })`                       | `POST /me/subscriptions/{id}/change`                            |
| `listSolanaTokens({ priceId, wallet })`                                                             | `GET /solana/tokens`                                            |

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
  `subscription_change_in_flight` refusal naming `metadata.operation_id`, a 5xx or a
  lost response), so OpenRails replays the stored result instead of charging
  twice. `requires_action` carries `operation_id` for `authenticatePayment`;
  replay the same key afterwards.
- A rail that needs the customer's own step answers `requires_action` with a
  `next_action`. Solana's is `solana_sign_transactions`:
  `signWalletAction(change.next_action, send)` signs and sends its
  transactions and returns the signature; repeat `changeSubscription` with the same
  key and `signature`. `cancelSubscription(id, { reason, signature? })` works
  the same way: its answer is the subscription, unchanged and carrying
  `next_action` until the signed cancel lands. The hooks run this when given
  `sendTransaction`.

## Provider-neutral flows

OpenRails's browser PSP configs (`GET /config`'s `payment.psps`) pick the
browser flow from each PSP's `flow` and public `config`
(Collect.js, native card inputs, Stripe Elements, redirect, wallet). Hosts never branch on a
provider:

- A session's `options` are exactly the armed PSPs whose rail can make this
  sale (Solana when it is configured, never CCBill),
  each with its `driver` and `public_config`; `saved_methods` are the buyer's
  cards on them, newest first (paying sends the chosen card's id explicitly).
  Card rails (`collect_js`, `card`, `stripe_elements`) render one panel: saved cards,
  an inline new card and one Pay/Subscribe button, which is the customer's
  confirmation of the displayed terms. No provider chooser with one rail.
- Inside a `BillingProvider` with a client, a new card is always saved to the
  account first and the session is paid with `payment_method_id` on the
  customer surface; `requires_action` results
  carrying an `operation` run 3-D Secure in the page. A `failed` result stays
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
- `defaultCountry` (CheckoutPage, SavePaymentMethod, AccountBilling)
  preselects the billing country, else the browser locale's region.

- Panels: `SubscriptionsPanel` (cancel, resume, change card),
  `PaymentMethodsPanel`, `PaymentHistory`, `BillingStatusBadge`,
  `CancelSubscriptionDialog`; `AccountBilling` stacks them.
- `renderSubscriptionFooter={(s) => ...}` adds host content under a
  subscription row; `useBillingRefresh()` refetches after host-side changes.
- Hooks: `useSubscriptions` (`cancel`, `resume`, `setPaymentMethod`,
  `changeSubscription`, per-row `pending`, `nextCursor`), `usePaymentMethods` (`add`,
  `remove`, `setDefault`), `usePayments` (cursor pages), `useProducts` (the
  catalog).
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
