# Changelog

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
