# Payment form contract

`Checkout` renders one real `<form autocomplete="on">` and submits through its
native submit event. Every host-owned billing field has a stable `id`, `name`,
label, and standards-based autocomplete token.

## Money

The session document (`GET /billing/v1/checkout-sessions/{id}`) carries every amount
as an exact signed int64 decimal string of `plan.currency`'s native unit, and
`plan.unit_decimals` is that currency's registered scale:

```json
{
  "plan": {
    "display_name": "Premium Membership",
    "unit_amount": "99000000",
    "currency": "USD",
    "unit_decimals": 6,
    "period_hours": 720,
    "automatically_renews": true
  },
  "line_items": [{ "label": "Premium Membership", "amount": "99000000" }],
  "tax": "0",
  "due_today": "99000000"
}
```

`unit_amount`, `line_items[].amount`, `tax` and `due_today` share the plan's
currency and scale. A JSON number, a decimal point, a value outside int64, or
a missing `unit_decimals` fails schema validation and the session is
unavailable; the package never assumes a scale. Hosts take the scale from
OpenRails' currency registry (`billing.LookupCurrency`, the same table as
`GET /v1/currencies`); OpenRails serves the session at
`GET /v1/checkout-sessions/{id}`; its canonical fixture
(`testdata/wire/checkout_session.json`) is decoded by this package's tests.

Rendering scales with `BigInt` and hands Intl the exact major-unit decimal;
`formatAmount`, `amountToDecimal` and `addAmounts` are exported for hosts that
show the same figures outside the component. An amount the engine cannot show
exactly is refused with a visible notice, never rounded. `due_today`, when
absent, is the exact sum of the line items and `tax`.

## NMI new cards

The host collects only:

- the cardholder's name: one visible “Name on card” input with `autocomplete="cc-name"`;
- the country: a native ISO-3166 country select with `autocomplete="billing country"`;
- the postal code: a country-aware input with `autocomplete="billing postal-code"`.

Postal code is optional for the 53 ISO-3166 alpha-2 countries and territories
in the Universal Postal Union's September 2025
[list of countries which do not require postal codes](https://www.upu.int/UPU/media/upu/documents/PostCode/General-Addressing-Issues.pdf).
The UI keeps a postal code when the customer supplies one. It does not collect
street, city, or state for NMI.

Card number, expiration, and CVC remain inside NMI Collect.js cross-origin
iframes. NMI owns those inner inputs, including their browser-autofill behavior;
host markup cannot assign `cc-number`, `cc-exp`, or `cc-csc` to them. The
Collect.js configuration supplies the supported field titles and placeholders,
and the outer host page provides stable labelled containers. Verify saved-card
autofill against the real gateway in each supported browser because a DOM unit
test cannot inspect or control the cross-origin fields.

A PSP that takes cards on OpenRails itself (`card_entry: server`, driver
`card`) gets plain inputs instead, with `autocomplete` `cc-number`, `cc-exp`
and `cc-csc`, the same layout and field errors, and no gateway script. Its
request carries `card: {number, exp_month, exp_year, cvc}` in place of
`payment_token`; the inputs are cleared once the card is sent.

A new-card payment request includes the one-time `payment_token` plus
`billing_details`: `name`, and `address` with the uppercase ISO `country` and
`postal_code`. Paying with an
existing saved method sends only `payment_method_id`; it does not overwrite the
stored billing identity with empty form values.

The package never shows separate first- and last-name inputs; OpenRails
projects the one name onto provider-specific fields at the rail boundary.

## Card subscriptions

`Subscribe` is one action: paying the session with a `payment_token` (or a
`payment_method_id`) saves a new card, accepts the displayed recurring terms
and charges it. A decline answers `failed` and keeps no card.

## Solana Pay

The package never assumes which token a Solana option settles in. OpenRails
binds the token on the advertised option's `public_config` (hosts copy it):

- `token_symbol` — required; the SPL mint symbol the price is bound to
  (`USDC`, `USD1`, …). It is sent back verbatim (uppercased) as the pay
  request's `token_symbol`. An option without it is not offered at all.
- `token_name` — optional buyer-facing name (`USD Coin`), shown next to the
  symbol in the method list.
- `network` — optional Solana cluster (`mainnet-beta`, `devnet`, `testnet`).
  Any network other than `mainnet-beta` is named in the method hint
  (“USD Coin (USDC) on Solana devnet”) so a test-network payment is never
  mistaken for a real one.

## Pending payment outcomes

`processing` means an accepted outcome is unresolved. Checkout polls the same
source's `getSession` without calling `pay` or Collect.js again. The optional
`expires_at` may be null for an accepted pending attempt; a local display timer
never releases or fails that attempt. The host must return verified terminal
state, not infer payment from navigation or a timeout. A thrown error after
`pay` starts is treated as unknown and remains pending, rather than enabling
a fresh card token and payment. Tokenization/field validation errors before
submission remain editable. Explicit `failed` results are definitive host
responses; they must not represent an unknown provider result.

`client.checkoutSource(id)` is the source of every checkout: it reads and pays
the OpenRails session. `getSession` is a read, never another payment.

## Separate NMI card setup

`TokenizedCardForm` reuses the same hosted fields and emits `payment_token`,
`name_on_card`, `country` and `zip` to its async `onTokenized` callback. The host
provides explicit save-card consent and uses the callback to invoke its native
authenticated save-card endpoint. Set `disabled` until consent is given. The
form never creates a zero-dollar checkout or announces a completed payment.
After token submission it locks the form; a failed/unknown save must be checked
against the host's saved-method state before mounting a new attempt.

Saving a card does not authorize a recurring charge. The host separately obtains
and displays the immutable membership quote, then asks the signed-in customer
to confirm that agreement.

Hosts must key `TokenizedCardForm` by authenticated customer and provider identity. A customer change must unmount the old form. The form also discards an in-flight token if its tokenization configuration changes or save consent is withdrawn before dispatch.
