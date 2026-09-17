# Changelog

## 0.3.0

Breaking: the session document carries exact money.

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
