# Changelog

## 0.9.0

One embedded card panel for one-time purchases and subscriptions, on every
card PSP (openrails#1064).

- Stripe Elements checkout rail (`driver: "stripe_elements"`, from a PSP with
  `flow: "elements"`): saved Stripe cards or a new card in the page, 3-D
  Secure via `authenticatePayment`; no redirect to hosted Checkout.
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
  Stripe card setup needs OpenRails serving Stripe's `publishable_key`
  (openrails#1062).

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
