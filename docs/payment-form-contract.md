# Payment form contract

`Checkout` renders one real `<form autocomplete="on">` and submits through its
native submit event. Every host-owned billing field has a stable `id`, `name`,
label, and standards-based autocomplete token.

## Money

The session document (`GET .../checkout/sessions/{id}`) carries every amount
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
OpenRails' currency registry (`openrails.LookupCurrency`, the same table as
`GET /v1/currencies`) by building the plan with
`openrails.NewHostedCheckoutPlan(product, price)`; the Go shape is
`openrails.HostedCheckoutSession` and its canonical fixture
(`testdata/wire/hosted_checkout_session.json`) is decoded by this package's
tests.

Rendering scales with `BigInt` and hands Intl the exact major-unit decimal;
`formatAmount`, `amountToDecimal` and `addAmounts` are exported for hosts that
show the same figures outside the component. An amount the engine cannot show
exactly is refused with a visible notice, never rounded. `due_today`, when
absent, is the exact sum of the line items and `tax`.

Version 0.3.0 replaces `unit_amount_micros`, `amount_micros`, `tax_micros` and
`due_today_micros` (JSON numbers, assumed six decimals) with this contract.

## NMI new cards

The host collects only:

- `name_on_card` — one visible “Name on card” input with `autocomplete="cc-name"`;
- `country` — a native ISO-3166 country select with `autocomplete="billing country"`;
- `zip` — a country-aware ZIP/postal input with `autocomplete="billing postal-code"`.

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

A new-card payment request includes the one-time `payment_token` plus canonical
`name_on_card`, uppercase ISO country in `country`, and `zip`. Paying with an
existing saved method sends only `payment_method_id`; it does not overwrite the
stored billing identity with empty form values.

## CCBill

For CCBill credit-card hand-off, the browser supplies only the same single
`name_on_card`, country, and postal code. OpenRails binds the authenticated
account's verified email and derives the customer's IP server-side; the browser
cannot override either value. CCBill's FlexForms Address Fields are
configurable for credit-card forms, so OpenRails does not collect street, city,
or state in this flow. The checkout package never exposes separate first- and
last-name inputs. OpenRails performs any provider-specific name projection at
the CCBill boundary.

Version 0.2.4 requires a checkout host whose pay endpoint accepts
`name_on_card`. Legacy `first_name` and `last_name` are no longer emitted by
this package.

## Solana Pay

The package never assumes which token a Solana option settles in. The host
binds the token on the option's `public_config`:

- `token_symbol` — required; the SPL mint symbol the price is bound to
  (`USDC`, `USD1`, …). It is sent back verbatim (uppercased) as the pay
  request's `token_symbol`. An option without it is not offered at all.
- `token_name` — optional buyer-facing name (`USD Coin`), shown next to the
  symbol in the method list.
- `network` — optional Solana cluster (`mainnet-beta`, `devnet`, `testnet`).
  Any network other than `mainnet-beta` is named in the method hint
  (“USD Coin (USDC) on Solana devnet”) so a test-network payment is never
  mistaken for a real one.

Version 0.2.5 removes the former `USDC` default: a host that bound no token
saw “USDC” before and sees no Solana option now.
