# Payment form contract

`Checkout` renders one real `<form autocomplete="on">` and submits through its
native submit event. Every host-owned billing field has a stable `id`, `name`,
label, and standards-based autocomplete token.

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

For CCBill REST/tokenized-card hand-off, the host collects only email, the same
single `name_on_card`, country, and postal code. Street, city, and state are
optional to CCBill and are not collected by this card flow. The customer's IP
is required by CCBill but remains server-derived rather than browser-supplied.
The checkout package never exposes separate first- and last-name inputs.
OpenRails performs any provider-specific name projection at the CCBill
boundary.

Version 0.2.3 requires a checkout host whose pay endpoint accepts
`name_on_card`. Legacy `first_name` and `last_name` are no longer emitted by
this package.
