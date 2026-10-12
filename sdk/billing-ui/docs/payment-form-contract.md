# Payment form contract

`OrderPayment` renders one real `<form autocomplete="on">` and submits through
its native submit event. Every host-owned billing field has a stable `id`,
`name`, label, and standards-based autocomplete token.

## Money

An order (`GET /billing/v1/me/orders/{id}`) carries every amount as an exact
int64 decimal string of its `currency`'s native unit: `total`, each line's
`unit_amount` and `amount`, and a usage order's `amount_paid` and
`amount_due`. A JSON number, a decimal point or a value outside int64 fails
schema validation; the package never assumes a scale. It takes the scale from
OpenRails' currency registry (`billing.LookupCurrency`, the `currencies` of
`GET /v1/config`).

Rendering scales with `BigInt` and hands Intl the exact major-unit decimal;
`formatAmount`, `amountToDecimal` and `addAmounts` are exported for hosts that
show the same figures outside the component. An amount the engine cannot show
exactly is refused with a visible notice, never rounded.

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

A PSP that takes cards on OpenRails itself (`card_entry: server`) takes no
card on an order: `OrderPayment` offers it for saved cards only, and the card
is saved first (`SavePaymentMethod`).

A new-card payment sends the one-time `token` and its `psp_id` plus
`billing_details`: `name`, and `address` with the uppercase ISO `country` and
`postal_code`. Paying with an existing saved card sends only
`payment_method_id`; it does not overwrite the stored billing identity with
empty form values.

The package never shows separate first- and last-name inputs; OpenRails
projects the one name onto provider-specific fields at the rail boundary.

## Card subscriptions

Subscribing is one action: paying the order with a `token` (or a
`payment_method_id`) charges and saves a new card and starts the subscription.
A decline answers `402` with the order open again and keeps no card.

## Solana Pay

A Solana payment option takes a token from `GET /v1/me/config`'s
`payment.solana.tokens`; the pay sends its `token_symbol`. The order answers a
`solana_pay` next action whose `url` the package shows as a QR code and a
wallet link, and it confirms the order until the transfer lands.

## Pending payment outcomes

`processing` means an accepted outcome is unresolved; `onSettled` reports it,
and the host waits for the verified terminal state (the order read again, or
its host event) rather than inferring payment from navigation or a timeout.
Each pay sends one Idempotency-Key, minted when the button renders: a retry of
the same request answers what the first did. A refusal or an error shows its
message, and the next pay is a new attempt under a new key.

## Separate NMI card setup

`TokenizedCardForm` reuses the same hosted fields and emits `payment_token`,
`name_on_card`, `country` and `zip` to its async `onTokenized` callback. The host
provides explicit save-card consent and uses the callback to invoke its native
authenticated save-card endpoint. Set `disabled` until consent is given. The
form never creates a zero-dollar purchase or announces a completed payment.
After token submission it locks the form; a failed/unknown save must be checked
against the host's saved-method state before mounting a new attempt.

Saving a card does not authorize a recurring charge. The host separately obtains
and displays the immutable membership quote, then asks the signed-in customer
to confirm that agreement.

Hosts must key `TokenizedCardForm` by authenticated customer and provider identity. A customer change must unmount the old form. The form also discards an in-flight token if its tokenization configuration changes or save consent is withdrawn before dispatch.
