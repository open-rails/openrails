# @openrails/billing-ui

Embeddable OpenRails billing UI: the checkout flow and the customer's account
billing (subscriptions, saved cards, payment history).

| Import                            | Contents                                                                           |
| --------------------------------- | ---------------------------------------------------------------------------------- |
| `@openrails/billing-ui/client`    | Framework-free typed client for `/billing/v1/me/*` and the catalog, `BillingError` |
| `@openrails/billing-ui/react`     | `BillingProvider` and headless hooks                                               |
| `@openrails/billing-ui`           | Styled checkout and account components, `BillingUiProvider`, i18n                  |
| `@openrails/billing-ui/locales/*` | `en de es ja ko zh` message bundles                                                |

The checkout owns the browser payment flow. OpenRails holds the checkout
session: the signed-in customer mints one for a price, and its `ocs_` id alone
reads and pays it. Card data is tokenized in NMI-hosted Collect.js iframes and
never enters the host application, unless the PSP takes cards on OpenRails
itself (`card_entry: server`, driver `card`).

See [Payment form contract](docs/payment-form-contract.md) for the exact-money
session document, billing fields, browser-autofill behavior, and the checkout
request schema.

The package lives in the OpenRails repository and shares its version: each
OpenRails release `vX.Y.Z` attaches `openrails-billing-ui-X.Y.Z.tgz`.

```sh
pnpm add https://github.com/open-rails/openrails/releases/download/vX.Y.Z/openrails-billing-ui-X.Y.Z.tgz
```

## Checkout

```tsx
import { createBillingClient } from "@openrails/billing-ui/client"
import { Checkout, CheckoutFrame } from "@openrails/billing-ui"
import "@openrails/billing-ui/styles.css"

const billing = createBillingClient({ fetch: auth.authFetch })
const session = await billing.createCheckoutSession({ priceKey: "pro-monthly" })

// A shared payment page (Config.HTTP.Checkout.PageURL) answers with a url:
<CheckoutFrame url={session.url} theme="dark" onComplete={() => refetchAccess()} />
// Without one, the app renders the checkout itself:
<Checkout source={billing.checkoutSource(session.id)} onComplete={() => refetchAccess()} />
```

`onComplete` is a hint: confirm access from your own authenticated API before
granting anything. Pass `successUrl` to return the buyer from a redirect rail
(CCBill, Stripe hosted); it must be on one of your app's return origins.

The payment host serves `<CheckoutPage>` from one HTML entry at `PageURL`,
behind its adapter's `CheckoutFramePolicy` (only the sites in
`Config.HTTP.Checkout.EmbedOrigins` may frame it):

```tsx
import { BillingUiProvider, CheckoutPage } from "@openrails/billing-ui"

createRoot(root).render(
  <BillingUiProvider>
    <CheckoutPage appearance={{ variables: brand }} />
  </BillingUiProvider>
)
```

The page reads the session id from its URL fragment and talks to its frame
with `postMessage`, origin-checked both ways: page to app `ready`,
`resize {height}`, `complete {status}` and `redirect {url}` (a nested frame
cannot navigate the top window, so the app does); app to page
`init {theme}`. The page messages only the app origin recorded on its session.

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

## Catalog and plan changes

The client also reads the public catalog and changes a subscription's plan.
No UI ships for these; hosts render their own. The catalog, currency and
Solana reads are public routes, served only when the host mounts OpenRails'
checkout routes.

| Call                                                                                                | Route                                                           |
| --------------------------------------------------------------------------------------------------- | --------------------------------------------------------------- |
| `listProducts()`, `listPrices({ currency, product, type })`                                         | `GET /products`, `GET /prices`                                  |
| `listCurrencies()` (`client.currencies` is the pinned copy)                                         | `GET /currencies`                                               |
| `previewTierChange(id, priceId)`                                                                    | `POST /me/subscriptions/{id}/change-tier/preview`               |
| `changeTier(id, { priceId, idempotencyKey })`                                                       | `POST /me/subscriptions/{id}/change-tier`                       |
| `getSolanaConfig()`, `listSolanaTokens({ priceId, wallet })`                                        | `GET /solana/config`, `GET /solana/tokens`                      |
| `prepareSolanaTierChange(id, newPriceId)`, `confirmSolanaTierChange(id, { signature, newPriceId })` | `POST /me/subscriptions/{id}/solana-tier-change`, `.../confirm` |

```ts
const preview = await billing.previewTierChange(sub.id, price.id) // amount_due_now, effective
const change = await billing.changeTier(sub.id, {
  priceId: price.id,
  idempotencyKey,
})
```

- `changeTier` is a money write. Mint one `idempotencyKey` per attempt and
  reuse it until the change resolves (`status: "processing"`, a
  `tier_change_in_flight` refusal naming `metadata.operation_id`, a 5xx or a
  lost response), so OpenRails replays the stored result instead of charging
  twice. `requires_action` carries `operation_id` for `authenticatePayment`;
  replay the same key afterwards.
- Solana rail: the wallet signs and sends `prepareSolanaTierChange`'s
  `transaction`; pass its signature to `confirmSolanaTierChange`. Keep the
  signature until the confirm succeeds; confirming again replays the result.

## Provider-neutral flows

Hosts pass OpenRails's browser PSP configs (`GET /checkout-config`, `psps`)
through unchanged; the PSP's `flow` and public `config` pick the browser flow
(Collect.js, native card inputs, Stripe Elements, redirect, wallet). Hosts never branch on a
provider:

- `checkoutRails(offers)` and `savedMethodsFor(methods, rails)` build a
  `CheckoutSource`'s `rails` and `saved_methods` (the default card first, pre-selected; paying sends
  the chosen card's id explicitly).
  `offers` is OpenRails' `ListCheckoutRailOptions` result, passed through: it
  lists exactly the armed PSPs whose rail can make this sale (Solana when it is
  configured, never CCBill for a new subscription), each with its `driver` and
  `public_config`. An offer without a driver is not rendered.
  Card rails (`collect_js`, `card`, `stripe_elements`) render one panel: saved cards,
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
  `setPaymentMethod`, `changeTier`, per-row `pending`), `usePaymentMethods`
  (`add`, `remove`, `setDefault`), `usePayments` (offset pages), `useProducts`
  (the catalog). Actions resolve to `null` or a `BillingError`; they never
  throw. `changeTier` resolves to the `TierChange` instead of `null`. Cancel
  and resume are queued by OpenRails (202); hooks re-read the subscription
  until the change shows.
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
