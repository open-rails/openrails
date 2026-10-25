# Charge agreements

A charge agreement is the customer's permission for the merchant to charge a
saved card without the customer present. Each is a mandate (`mdt_`):

| Kind | Given when | Covers |
|---|---|---|
| `recurring` | the customer subscribes with a card | one subscription's renewals |
| `unscheduled` | the customer sets a default card for a currency | charges for usage in that currency |

A card kept for one-click buys (`card_on_file`) authorizes no merchant charge
and is not an agreement; deleting the card ends it.

## Revoking one

The customer revokes each agreement on its own, unilaterally, where it was
given: on the merchant's site (`/v1/me`) or through a host-authorized
`Engine.CustomerRoutes` mapping, with its recent-sign-in checks.

| Route | Does |
|---|---|
| `GET /v1/me/agreements` | the customer's live agreements, newest first: kind, currency, card and, for `recurring`, the subscription's amount, interval and next charge |
| `POST /v1/me/agreements/{id}/revoke` | revokes one; `404 resource_not_found` once it is no longer live |

The mandate becomes `revoked` (`end_reason: customer_revoked`) and the card is
never charged under it again.

- **`recurring`**: the subscription is canceled at the end of its paid period,
  through the customer's own cancel. A schedule the provider bills that
  cannot be canceled now (the merchant's destructive provider actions are
  disarmed) answers `409 provider_cancel_held` and nothing is revoked.
- **`unscheduled`**: usage owed until now becomes an open usage order,
  collection of every open usage order in that currency stops, and the
  customer pays them itself, present. From then on the customer spends only
  its balance in that currency, whatever its billing mode, until it sets a
  default card again, which gives a new agreement.

The answer carries the revoked agreement, the canceled `subscription` or the
open `usage_order`.

## What the merchant and the customer are told

- The merchant gets the host event `mandate.revoked` (`GET /v1/app/events`):
  `mandate_id`, `kind`, `customer_id`, `currency` and, for `recurring`,
  `subscription_id`.
- OpenRails records a customer notification event describing the cancellation
  and its effective time, or the end of usage charges and what is still owed.
- The consuming application owns required customer communications, including
  cancellation confirmation and any receipt links to manage charge agreements.
  Source acknowledgment records durable app processing, not email delivery or
  customer read state. See [notification events](customer-notifications.md).
