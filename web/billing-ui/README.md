# @openrails/billing-ui

OpenRails billing in the browser: purchase, the customer's account billing,
and staff administration, inside your app. Card data is tokenized in the PSP's
own fields (Collect.js, Stripe Elements) and never enters your app. How it fits
an integration: the [client guide](https://github.com/open-rails/openrails/blob/master/docs/client.md#invoices-and-checkout).

| Import | Contents |
|---|---|
| `@openrails/billing-ui/client` | the typed client for `/v1/me` and `BillingError`, framework-free |
| `@openrails/billing-ui/react` | `BillingProvider` and headless hooks, no styles |
| `@openrails/billing-ui` | the provider, styled purchase and account components, i18n |
| `@openrails/billing-ui/locales/*` | `en de es ja ko zh` |
| `@openrails/billing-ui/admin`, `/admin.css` | `AdminConsole`, `BillingAdmin`, `BillingCustomerSummary` and their helpers |

Each OpenRails release `vX.Y.Z` publishes `@openrails/billing-ui@X.Y.Z` with
provenance; pin the exact version: `pnpm add @openrails/billing-ui@X.Y.Z`.

## The client

```tsx
import { BillingProvider, createBillingClient } from "@openrails/billing-ui"

// Embedded: your app's own mount, with your auth's fetch (auth-ui's authFetch
// attaches the bearer and retries once after a refresh).
const billing = createBillingClient({ baseUrl: "/billing/v1", fetch: auth.authFetch })

<BillingProvider client={billing} appearance={{ theme: "auto" }}>
  <App />
</BillingProvider>
```

Standalone or hosted, the browser calls the billing API with a short
`openrails:self` token your backend mints. `tokenSource` keeps it in memory
only, fetches the next one `skew` seconds (60) before expiry with one request
however many calls wait, and resends a call answered 401 `credential_expired` once:

```ts
const billing = createBillingClient({
  baseUrl: "https://billing.example/v1",
  getToken: tokenSource({
    fetchToken: async () => {
      const { assertion } = await (await fetch("/api/billing/assertion", { method: "POST" })).json()
      return redeemAssertion({ tokenEndpoint: "https://billing.example/oauth2/token", assertion })
    },
  }),
})
```

A `fetch` that adds a DPoP proof (auth-ui's `dpopFetch`) binds the token to the
browser's key. Requests omit credentials; any origin may call.

## Buying

The browser never reads the catalog: your server reads the products it sells
(Go `Client.ListProducts`) and puts them, priced as a buyer sees them
(`billing.BuyerPrices`), in its own page.

```tsx
<BuyButton price={course.prices[0]} signedIn={signedIn}
  onSignInRequired={openSignIn} onPaid={() => navigate("/courses/css-101")} />
<ProductList products={products} signedIn={signedIn}
  onSignInRequired={openSignIn} onPaid={() => navigate("/courses/css-101")} />
```

- A button creates the signed-in customer's invoice and pays it in a dialog. On
  a site the merchant registered to frame its pay URLs, it frames the invoice's
  `pay_url` instead (`<EmbeddedPayment>`). OpenRails answers the customer's open
  invoice of the same lines, so a second click, a reopened dialog or a reload pays
  one invoice.
- `onPaid` is a hint. Grant access from your server's own check, or from the
  `invoice.completed` event, never from the browser.
- `<InvoicePayment invoiceId>` pays an invoice your server created (`CreateInvoice`):
  saved cards, a new card, a hosted page, a Solana wallet, Apple and Google Pay
  through Stripe. `onSettled` follows completion. `returnUrl` must be on one of
  your app's return origins.
- A rail's own step is the invoice's `payment.next_action`: `redirect_to_url`
  (top window), `solana_pay` (a QR code or wallet link) or `authenticate` (the
  bank's challenge, in the page). In the EEA and UK a new card is kept only with
  `reusable: true`, the customer's own opt-in.
- `fallbacks` keeps a PSP back until the PSPs it stands in for fail
  (`{ ccbill: { for: ["nmi-main"], afterFailures: 2 } }`), then offers it under
  "Card payment isn't working?". `BuyButton`, `ProductList`, `Cart`,
  `InvoicePayment` and `EmbeddedPayment` take it.
- A `customer_amount` price takes the buyer's `amount`: parse it with
  `decimalToAmount(text, currency.decimals)` and check the price's bounds with
  `BigInt`. `<Cart>` sells the customer's cart.

The form's fields, autofill and exact money rules: [payment form
contract](docs/payment-form-contract.md).

## Account billing

```tsx
<AccountBilling plansHref="/plans" defaultCurrency="USD"
  sendSolanaTransaction={(tx) => wallet.signAndSend(tx)} />
```

`AccountBilling` stacks `SubscriptionsPanel` (cancel, resume, change card,
change plan), `PaymentMethodsPanel` and `PaymentHistory`; `RenewalPrompt` invites
a customer whose subscriptions all ended back to `plansHref`. The hooks
(`useSubscriptions`, `useChangeOptions`, `usePaymentMethods`, `usePayments`)
resolve to `null` or a `BillingError`, never throwing.

- **Configuration.** The panels read `GET /v1/config` and `GET /v1/me/config`
  once per provider (`useConfig()`, `useCustomerConfig()`). A page your server
  renders can pass both as `config` and `customerConfig` (Go:
  `Engine.PublicConfig`, `Engine.CustomerConfig`), so nothing is fetched to load.
- **Plan changes.** `changeSubscription(id, { priceId?, quantity?, idempotencyKey })`
  is a money write: mint one key per attempt and reuse it until the change
  resolves. More seats and upgrades charge now; fewer and downgrades apply at
  the next renewal. `authenticateChange` runs a card challenge in the page;
  Solana changes sign in the wallet (`signWalletAction`).
- **Money and periods** are exact: amounts are int64 native-unit strings scaled
  by OpenRails' currency registry (`formatAmount`), and 720 hours reads "every
  30 days", never "monthly".
- **Words and styles.** English is complete and the fallback; `messages` layers
  bundles and `t` lets your i18n win. Styles are scoped under `.orck`;
  `appearance.theme` is `light`, `dark`, `auto` or `inherit` (your shadcn tokens).
- **Notifications.** AuthKit UI owns the inbox; `BillingNotification` and
  `isBillingNotification` render billing items through its `renderItem`.

## Staff administration

Needs `@tanstack/react-query`. `AdminConsole` is the whole staff console, AuthKit's
account administration and billing together (needs `@openrails/auth-ui`):

```tsx
<AdminConsole authBaseUrl="/api/v1" billingBaseUrl="/billing/v1" merchant={merchant} basePath="/admin" />
```

`BillingAdmin` is the billing pages alone, inside your own admin app:

```tsx
import { BillingAdmin } from "@openrails/billing-ui/admin"
import "@openrails/billing-ui/admin.css"

<BillingAdmin baseUrl="/billing/v1" auth={auth} basePath="/admin/billing"
  navigation={{ pathname, search, navigate }} appearance={{ theme: "inherit" }} />
```

- `auth` supplies `authFetch`; the component stores no tokens. Mount it for the
  signed-in person and remount when that changes. Each tab shows only to whoever
  holds its permission (`GET /v1/admin/access`); the API checks every request.
- `merchant` fixes the merchant (`merchants` and `onMerchantChange` add a
  switcher); `settingsTabs`, `customerPanels` and `userHref` add your content.
- `useBillingAdminAccess` reads the staff member's access for your navigation;
  `BillingCustomerSummary` previews a customer's billing on your page;
  `billingConfirmWarnings` words a ban or deletion warning (neither cancels
  subscriptions).
- The customer imports load no admin code or styles.

## Developing

`pnpm critical:build` builds the package and checks the browser driver types;
`pnpm critical:static` runs type, lint and packed-artifact checks;
`pnpm contract:check` fails when the generated wire types under
`src/client/generated` are stale (`pnpm contract` regenerates them with
OpenRails' contracts). Browser behaviour runs in OpenRails' critical suite.
`src/components/ui` is shadcn (`base-vega`, zinc; see `components.json`).

## License

MIT; see [LICENSE](LICENSE). Only this package is MIT: the rest of the OpenRails repository is under the Elastic License 2.0 (ELv2).
