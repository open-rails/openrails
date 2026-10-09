# Changelog

## One public configuration

- `getConfig()` reads `GET /config`: the mount's `capabilities`, the currency
  registry (`currencies`) and the merchant's `payment` setup (its PSPs and
  Solana network and tokens; null without a merchant). It replaces
  `getCheckoutConfig()` and `listCurrencies()`: `checkoutConfigSchema` and
  `CheckoutConfig` are `publicConfigSchema` and `PublicConfig` (its `payment`
  is a `PaymentConfig`), and `currencyRegistrySchema` is removed.
  `client.currencies` stays the pinned registry.
- `BillingProvider` fetches it once and shares it: `useConfig()` reads it,
  `useCurrencyScales()` layers the pinned registry and host overrides over its
  currencies, and the account panels use both.
- `AccountBilling` and `PaymentMethodsPanel` take their PSPs from it; their
  `psps` prop is removed. "Add card" waits for it, says so when it fails (with
  a retry) or every card PSP is temporarily unavailable, and asks again after
  the PSP's `retry_after`.
- `PspConfig` has `status` and `retry_after`; a temporarily unavailable PSP
  saves no card (`cardSetupDriver` is null).

## Prices come with their products

- `listPrices` is removed with `GET /prices`: `listProducts()` returns every
  product on sale with its current prices. Filter by currency, product or
  renewal in the browser.

## Paying on a customer surface

- `client.checkoutSource(id, { customerBase })` reads and pays a checkout
  session as its signed-in customer at a customer surface's
  `/checkout-sessions/{id}` (for example a host's
  `/api/v1/merchants/acme/billing/me`), so its saved cards pay. Without
  `customerBase` the session id alone pays at `/checkout-sessions/{id}` as
  before. `<CheckoutPage customerBase>` passes it through.

## 1.0.0

Ships with OpenRails v1.0.0; use the two at the same version. Everything a host
changes is in [Migrating to v1](../../docs/migrating-to-v1.md).

Checkout

- A checkout session is the only browser purchase. `createCheckoutSession()`
  mints at `/me/checkout-sessions` and returns a `CheckoutSessionLink` (`url`
  null without a payment page); `client.checkoutSource(id)` is the source of
  `<Checkout>`, `<CheckoutModal>` and `<CheckoutPage>`.
- The session document's `rails` is `options` (`PaymentOption`, with the
  option's `psp_id`). A rail's own step is one `next_action`
  (`redirect_to_url`, `solana_pay`); a card challenge is `operation`. Saved
  methods carry `card` (`brand`, `last4`, `exp_month`, `exp_year`) and no
  `default`. Fields OpenRails sends as null read as `undefined`.
- `PayRequest` carries one `billing_details` (`name`, `email`, `phone`,
  `address` with `postal_code` and `country`) in place of the flat
  `name_on_card`, `zip`, `country`, `last_four`, `card_type` and `expiry_date`.
- Stripe Elements runs on a session inside a `BillingProvider`: the card is
  saved through the customer's client and the session pays it by id.
- Removed: `createHttpSource`, `CheckoutSourceError`, `checkoutRails`,
  `CheckoutRailOffer`, `PaymentRailOption`.

Client

- Lists are cursor pages: `listSubscriptions`, `listPaymentMethods`,
  `listPayments` and `listInvoices` take `cursor` and answer `next_cursor`;
  there is no `total`. The hooks page with `nextCursor`.
- `cancelSubscription`, `resumeSubscription` and
  `setSubscriptionPaymentMethod` resolve to the `Subscription`. A Solana cancel
  or tier change answers `next_action` (`solana_sign_transactions`); sign it
  with `signWalletAction` and repeat the call with `signature`. The separate
  Solana cancel and tier-change methods are removed.
- `addPaymentMethod` takes `NewCard` with `psp_id`, a `payment_token` or
  `card`, and `billing_details`.
- There is no default card: `setDefaultPaymentMethod` is
  `setCollectionPaymentMethod`, the hook's `setDefault` is `setCollection`, the
  change event is `payment_method.collection_changed`, and the
  `AccountBilling` / `PaymentMethodsPanel` prop `defaultCurrency` is
  `collectionCurrency`. `PaymentMethod` lists `collection_currencies`.
- `getSolanaConfig()` is `getCheckoutConfig()` (the network is
  `solana.network`); `listSolanaTokens` takes `{ priceId, wallet }`.
- `getStatus()` and `BillingStatus` are removed: read `/me/entitlements` and
  `listSubscriptions()`.
- `listPrices` filters by `productId` and `autoRenew`. A `Price` has
  `product_id`, `archived`, `access_duration_hours` (null: for good) and
  `auto_renew`; `type`, `recurring.interval` and `active` are gone.
- `Payment` has `kind`, a typed `status` and the `price` it bought; a
  subscription's status is `canceled`.
- A `TierChange` has no `mode`, `url` or `payment`: a redirect is
  `next_action.url`. Generated list fields are `T[]`, never `null`.
- A subscription's `access` windows are `starts_at` and `ends_at`.
- A `Product` has no `catalog_id`. A refusal's code fixes its status:
  `changeTier` answers a registered code for each condition, and a declined
  tier-change charge is `card_declined` with `decline_reason`.

## Hosted checkout sessions

- OpenRails serves the checkout session; hosts no longer write
  `/api/v1/checkout/sessions` routes. `client.createCheckoutSession()` mints
  one for the signed-in customer (`priceKey` or `priceId`, `successUrl`) and
  `client.checkoutSource(id)` reads and pays it with the id alone (no bearer).
  A refusal the buyer can act on resolves as a `failed`, `blocked` or
  `expired` result. A `card` rail pays through the shared page too.
- `<CheckoutFrame url onComplete theme>` frames the shared payment page and
  `<CheckoutPage>` is that page; they speak an origin-checked frame protocol
  (`ready`, `resize`, `complete`, `redirect`; `init {theme}`).
- `<Checkout onRedirect>` replaces the top-window navigation of redirect rails.
- The session document gains `embed_origin`.

## Server card entry

- Driver `card` (a PSP declared `card_entry: server`, flow `card`): `<Checkout>`
  and `SavePaymentMethod` render plain card inputs with the Collect.js form's
  layout and field errors, load no gateway script, and send
  `card: {number, exp_month, exp_year, cvc}`. Inside a `BillingProvider` the
  card is saved first (`addPaymentMethod({ provider, card })`) and charged by
  id; a page without one sends `card` in `PayRequest`. The inputs are cleared
  once the card is sent.
- `cardSetupDriver` may return `"card"`; `NewCard` takes `payment_token` or
  `card`, never both.

## Catalog and plan-change client

- `@openrails/billing-ui/client` gains the public catalog (`listProducts`,
  `listPrices`, `listCurrencies`), plan change (`previewTierChange`,
  `changeTier`) and Solana (`getSolanaConfig`, `listSolanaTokens`,
  `prepareSolanaTierChange`, `confirmSolanaTierChange`) calls, with the types
  `Product`, `Price`, `Currency`, `TierChangePreview`, `TierChange`,
  `SolanaConfig`, `SolanaToken`, `SolanaTierChangeTx` and `SolanaTierChange`.
  `changeTier` requires an `idempotencyKey`; reuse it until the change
  resolves.
- React: `useProducts`, and `useSubscriptions().changeTier` (per-row `pending`
  `"change_tier"`, `onChange` `subscription.tier_changed`), which resolves to
  the `TierChange` or a `BillingError`.

## Explicit payment method, fail-fast errors

- The checkout pre-selects the customer's default card (`PaymentMethod.default`,
  `SavedPaymentMethod.default`) and always sends the chosen card's id.
  OpenRails refuses a charge that names no method with
  `payment_method_required` (400).
- A server error (5xx) from `pay` is shown at once and the panel returns to
  ready; hosts keep their idempotency key so paying again replays. A host
  source signals it by throwing an error with a numeric `status`
  (`CheckoutSourceError`, or any error carrying `status`).
- The client never retries a write. A GET retries once, only on a network
  error or 502/503/504 (429/503 with `Retry-After`), within `RETRY_BUDGET_MS`
  (2 s). New `isServerError`; a 5xx without an error envelope has code
  `server_error`.

## One-click card subscriptions

- A checkout host relays a card subscription's pay request with OpenRails'
  `CreateCheckoutSessionRequest.Confirm`; a new Collect.js token subscribes in
  one call. No package API change.

## Advertised checkout rails

- Breaking: `checkoutRails(offers)` takes OpenRails' checkout options alone.
  Each option carries the `driver` and `public_config` OpenRails derived from
  the armed PSP (Solana: `token_symbol`, `token_name`, `network`); the package
  no longer derives drivers from `psps`. `CheckoutRailOffer` gains `selector`,
  `driver` and `public_config`.

## Moved into OpenRails

The package now lives in `open-rails/openrails` under `sdk/billing-ui` and
takes the OpenRails version: each OpenRails release `vX.Y.Z` attaches
`openrails-billing-ui-X.Y.Z.tgz`. The e2e server and generated contract build
the OpenRails at the same commit. `OPENRAILS_VERSION` is gone from the
generated contract.

## 0.9.0

One embedded card panel for one-time purchases and subscriptions, on every
card PSP. Requires OpenRails v0.164.0.

- Stripe Elements checkout rail (`driver: "stripe_elements"`, from a PSP with
  `flow: "elements"`): saved Stripe cards or a new card in the page, 3-D
  Secure via `authenticatePayment`; no redirect to hosted Checkout. The
  Payment Element is cards only (no Link bank tab or wallets).
- One screen: saved cards (brand •••• last4 · MM/YY, most recent first),
  inline "Use a new card", one "Pay $X" / "Subscribe for $X every <period>"
  button with the agreement line. No provider chooser for a single rail.
- New cards are always saved to the account (no consent checkbox) before
  paying by id; Collect.js display metadata (`last_four`, `card_type`,
  `expiry_date`) is sent with every token.
- Declines stay on the panel: `PayResult.failure` / `CheckoutSession.failure`
  (`{ reason, message, field }`) render next to the card field; retry with
  another card. `requires_action` + `operation_id` authenticates in the page.
- Collect.js `validationCallback`: inline number/expiry/CVC errors; the
  button waits for three valid fields. Stripe Elements and Collect.js fields
  are themed from the page tokens.
- `defaultCountry` on Checkout / SavePaymentMethod / AccountBilling /
  TokenizedCardForm; fallback: the browser locale's region
  (`initialCountry`, `browserCountry`).
- `PspConfig.checkout`; `checkoutPsps(psps)` — only checkout PSPs take new
  cards. `isCardRail`, `useOptionalBillingContext`.
- `CheckoutModal`: always closable; `gate` renders host content (sign-in)
  in place of the checkout.
- Account: card labels read "Visa •••• 4242 · 12/30"; failed payments show
  `payment.failure.message`.
- Breaking: `paymentMethods.consent`/`enterCard` messages are replaced by
  `paymentMethods.saveNotice`; `cardLabel` is "{brand} •••• {last4}".

## 0.8.0

Provider-neutral hosts: a host passes OpenRails's browser PSP configs through
and never branches on Stripe, NMI or any other provider.

- `checkoutRails(offers, psps)` / `savedMethodsFor(methods, rails)` derive the
  checkout rails and chargeable saved cards from each PSP's `flow` and public
  config.
- `SavePaymentMethod`: consented in-page card saving with any PSP — Collect.js
  tokenization or Stripe Elements card setup (Stripe.js is loaded on first
  use). `canSavePaymentMethod`, `cardSetupDriver`.
- `authenticatePayment(client, operationId, psp)`: 3-D Secure for a pending
  payment operation. `canAuthenticatePayment`.
- Client: `createCardSetup`, `getCardSetup`, `confirmCardSetup`,
  `getPaymentAuthentication`, `confirmPaymentAuthentication`.
- Breaking: `AccountBilling`/`PaymentMethodsPanel` take `psps` (and optional
  `cardSetupReturnURL`) instead of `cardSetup`; `CardSetupConfig` is removed.
  Stripe card setup needs OpenRails serving Stripe's `publishable_key`.

## 0.7.0

- Periods are exact. OpenRails windows are hours, so a 720h price reads
  "every 30 days" (was "every month") and 8760h "every 365 days"; only exact
  weeks, days or hours are named, in the account panels and the checkout
  summary, in all six locales.
- Plural messages: `interval.every|per|access.<hour|day|week>` are CLDR plural
  nodes (`{ one, other }`, exact `"1"` for 毎日-style forms) chosen by the
  provider's `locale`; `Translator.plural(key, count)`. Breaking for hosts
  overriding the removed `interval.day|week|month|year|days|hours` keys.
- Payment history names what was bought: the product's `display_name` from
  OpenRails (`payment.product`, OpenRails after v0.160.0) and the renewal
  period; older servers fall back to "Subscription"/"Purchase".
- Checkout summary copy ("Renews {period}") is a message: `checkout.renews`.
- The e2e harness and route contract pin OpenRails master `78039ea` (first
  commit serving `payment.product`; untagged, after v0.160.0).

## 0.6.0

- "Add card" works against real OpenRails, which requires the PSP that
  tokenized the card: `CardSetupConfig.provider` (the PSP key, e.g. `"nmi"`)
  is required and sent as `NewCard.provider`. Before, every add was refused
  with 400 `provider is required`.

## 0.5.2

- `renderSubscriptionFooter` (on `SubscriptionsPanel` and `AccountBilling`)
  renders host content under a subscription row, e.g. a plan-change control.
- `useBillingRefresh()` (from `./react`) refetches every hook after a
  host-side change such as a plan switch.
- The cancel and change-card dialogs use the panel's `appearance`; they read
  only the provider's, so `AccountBilling appearance` missed them.

## 0.5.1

- `appearance.theme: "inherit"`: no bundled palette; components use the host
  page's shadcn tokens and follow its `.dark` class.
- `SubscriptionsPanel`: "Change card" switches the saved card an active NMI
  subscription renews on (`useSubscriptions().setPaymentMethod`), which now
  notifies `subscription.payment_method_changed`.

## 0.5.0

Account billing, alongside checkout. No change to checkout's API.

- `@openrails/billing-ui/client`: `createBillingClient` for the OpenRails
  customer surface (`/billing/v1/me/*`): subscriptions (list, get, cancel with
  feedback, resume, payment method, Solana on-chain cancel), payment methods
  (list, add tokenized card, remove, per-currency default), payments, invoices,
  status. zod-validated responses, `BillingError` from the error envelope, the
  pinned currency registry for exact money.
- `@openrails/billing-ui/react`: `BillingProvider` (`onChange` for host cache
  invalidation), `useSubscriptions`, `usePaymentMethods`, `usePayments`.
- Styled: `AccountBilling`, `SubscriptionsPanel` with cancel/resume dialog and
  provider-portal links, `PaymentMethodsPanel` (adding a card uses
  `TokenizedCardForm`), `PaymentHistory`, `BillingStatusBadge`,
  `BillingUiProvider`, `BillingUiRoot`.
- Messages: typed bundles with `en de es ja ko zh` under `./locales/*`.
- `formatAmount` takes an optional locale.
- The route contract and e2e suite run against real OpenRails v0.159.0.

## 0.4.2

No API or visual change.

- Components import `cn` from the [`cn`](https://github.com/shadcn-ui/cn)
  package (pinned `0.4.0`), replacing `clsx` and `tailwind-merge`.

## 0.4.1

Visual only; no API change.

- UI primitives regenerated as shadcn `base-vega` on a zinc palette.
- Theme tokens now map to Tailwind colors, so token utilities (`bg-primary`,
  `text-muted-foreground`, `border-input`, …) render; in 0.4.0 they emitted no
  CSS, leaving the pay button and selected radios unstyled.
- `dark:` variants follow `appearance.theme` instead of the OS alone.
- `CheckoutModal` now paints its popover panel; buttons inside `.orck` drop the
  UA button face.

## 0.4.0

Breaking: the package is renamed `openrails-checkout` → `@openrails/billing-ui`.

- Import from `@openrails/billing-ui` and `@openrails/billing-ui/styles.css`.
- The injected stylesheet is `<style id="billing-ui-styles" data-billing-ui>`.
- Releases attach the packed tarball to the GitHub release instead of
  publishing to npm:
  `https://github.com/open-rails/billing-ui/releases/download/v0.4.0/openrails-billing-ui-0.4.0.tgz`.

## 0.3.0

Breaking: the session document carries exact money.

- Accepted `processing` outcomes poll the same source without repeating payment
  or tokenization. Ambiguous pay errors retain that pending attempt; local expiry
  cannot turn it into a failure. Pending sessions may omit/null `expires_at`.
- Export `TokenizedCardForm` for separately consented NMI card setup, reusing the
  existing hosted fields without presenting setup as a payment.
- Source changes fence late payment callbacks from the previous checkout.

- `plan.unit_amount`, `line_items[].amount`, `tax` and `due_today` are int64
  decimal strings of `plan.currency`'s native unit; `plan.unit_decimals` (the
  currency's registered scale) is required. Numeric amounts are refused as an
  unavailable session. Replaces `unit_amount_micros`, `amount_micros`,
  `tax_micros` and `due_today_micros`, which were JSON numbers at an assumed
  six decimals.
- Rendering never passes money through a JS number (`BigInt` scaling, exact
  Intl decimal formatting, refusal instead of rounding). `formatAmount`,
  `amountToDecimal`, `addAmounts`, `isAmount`, `amountSchema` and
  `unitDecimalsSchema` are exported.
- Hosts build the plan with `openrails.NewHostedCheckoutPlan` (OpenRails
  `HostedCheckoutSession` types); the package decodes OpenRails' canonical
  `hosted_checkout_session.json` fixture.

## 0.2.5

- A Solana option is named after the host-bound token and network; the former
  `USDC` default is removed.
- Releases publish to npm through the GitHub release workflow.

## 0.2.4

- Hosts must accept `name_on_card` on the pay endpoint; `first_name` and
  `last_name` are no longer emitted.
