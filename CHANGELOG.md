# Changelog

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
