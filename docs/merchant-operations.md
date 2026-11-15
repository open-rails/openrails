# Merchant operations

What staff (and the customer, where noted) do to existing subscriptions, payments
and credit. Each `/v1/admin` route has one `*Client` method; the permission pairs
are in [routes](api/routes.md).

## Quick reference

| Task | Route |
|---|---|
| look up a customer (settings, balances, arrears, default cards) | `GET /v1/admin/customers/{customer_id}`, `?search=` by email, username or name |
| overdue usage orders | `GET /v1/admin/orders?origin=usage&overdue=true` |
| failed renewals | `GET /v1/admin/subscriptions?dunning=true` ([dunning](dunning.md)) |
| grant a product, revoke a window | `POST /v1/admin/product-access` (batch; `hours` at most 2562047, none for no end), `…/{id}/revoke` |
| record money received outside OpenRails | `POST /v1/admin/payments` |
| subscriptions | `GET /v1/admin/subscriptions`, `sort` `created_at`, `current_period_ends_at` or `canceled_at` |
| findings | `GET /v1/merchant/findings` (merchant configuration, not customer support) |

## Cancel and resume

Cancel, resume and the payment-method switch answer the `Subscription`. Cancel
ends access at period end; the merchant route's `revoke_access` must be explicit
to end it now. Resume undoes a scheduled cancel where the rail can (Solana yes,
CCBill no).

- **Solana:** cancelling needs no signature: OpenRails stops pulling, keeps the
  paid period and allows undo until it ends. `revoke_on_chain: true` also asks for
  a wallet-signed on-chain cancel through `next_action`; repeat with the landed
  `signature` to confirm.
- **NMI or CCBill schedules:** while the merchant's destructive switch is off, a
  cancel is `409 provider_cancel_held` (OpenRails will not cancel locally while
  the provider would keep charging). `account_deletion: true` cancels locally and
  holds the provider stop.
- **A renewal in flight:** an unsettled renewal charge (or an unresolved Solana
  pull) refuses a cancel with `409 payment_in_progress`; retry once it settles.
- **Buying again:** until the provider confirms its schedule stopped, buying the
  product or tier group again is `409 resource_conflict`. While a canceled
  subscription is still paid it is `409 subscription_resumable` (resume it) or
  `409 subscription_paid_through`. A charge the provider takes after the cancel is
  refunded in full and raises `life.charge_after_cancel`.

## Subscription changes

`POST /v1/me/subscriptions/{id}/change` (and `/v1/admin/…/change`) moves a
subscription to another price of its tier group, other seats, or both:
`{price_id?, quantity?}`. It answers a `SubscriptionChange`: `status`
(`succeeded`, `processing`, `requires_action`, `blocked`), `effective` (`now`,
`period_end`), `price_id`, `quantity`, `amount_due_now`, `next_charge_amount`,
`next_charge_date` and any `next_action`. `…/change/preview` computes the same and
changes nothing.

- **`Idempotency-Key` is required** (`400 subscription_change_idempotency_key_required`).
  The same key replays (`200`, or `202 processing` while unresolved); a key for a
  different change is `409 subscription_change_idempotency_conflict`; a second key
  while one is unresolved is `409 subscription_change_in_flight`.
- **Seats** exist only on a price sold per seat (catalog `quantity: {min, max}`;
  its unit amount is one seat). Elsewhere `quantity` is
  `422 quantity_not_allowed`. Seats are billed only by OpenRails on NMI or Stripe
  (`400 subscription_change_unsupported_on_rail`).
- **More seats and upgrades are effective now.** Added seats pay the unit price
  for the rest of the period. An upgrade pays the new price × seats less a credit
  of old price × time left / current period length (rounded once, up to a whole
  minor unit, never above what was paid), for a fresh period of the new cycle;
  cadences may differ. On an NMI schedule the schedule changes in place and keeps
  its billing date. A
  decline changes nothing (`402`); a card challenge answers `requires_action`
  (`next_action.type: "authenticate"`, `payload.client_secret` for Stripe.js),
  then `POST /v1/me/subscriptions/{id}/confirm`.
- **Fewer seats and downgrades take effect at period end** as the subscription's
  `scheduled_change`, with nothing charged or refunded. Another tier change, or a
  pending price migration, is `409 subscription_change_already_scheduled`. A
  change back to the current price and seats cancels the pending change.
- **Staff changes never charge.** `POST /v1/admin/subscriptions/{id}/change` needs
  a `reason`; on a subscription OpenRails bills, every change (upgrades included)
  is scheduled for the next renewal, the reason and staff member are kept, and the
  customer gets `subscription_changed`. Staff cannot upgrade a provider-billed
  subscription or change a CCBill or Solana one (`403 customer_action_required`).
- **Refusals:** `409 subscription_change_renewal_due` (period ended or renewal
  unresolved), `422 subscription_change_cycle_unknown`,
  `422 subscription_change_period_unknown`, `409 subscription_change_credit_exceeds_price`.
- **NMI schedules** keep their billing date, so only a price of the same cycle
  qualifies (`409 subscription_change_cadence_unsupported`), and a schedule on a
  named NMI plan needs a linked plan on the target
  (`409 subscription_change_requires_linked_plan`). **CCBill** upgrades answer a
  `redirect_to_url`; downgrades are `blocked`.

Price migrations move many subscribers to new terms; see [catalog](catalog.md#price-migrations).

## Refunds

`POST /v1/admin/payments/{id}/refunds` (`Client.RefundPayment`): `{amount}` or
`{full: true}`, an optional `reason` and an explicit `revoke_access`;
`Idempotency-Key` required, and the amount, reason and choice are fixed for it.
`201` settled, `202` pending. A rail with no automatic refund (CCBill, Solana,
off-rail payments) is `400 refund_unsupported` (`ErrRefundUnsupported`); one that
cannot take it now is `409 refund_rail_unavailable` (`ErrRefundRailUnavailable`,
nothing reserved). A write gate (readonly, NMI test-mode qualification) parks the
refund `pending` until it allows it.

- Refunding money and revoking access are separate choices. Revocation commits
  with the local refund and removes only the refunded payment's access and its
  renewal grace; newer paid periods stay. A queued or uncertain refund keeps access
  until it completes.
- A revoking refund of the whole payment cancels a membership OpenRails renews:
  now if the payment bought its latest period, else at period end. A partial or
  non-revoking refund leaves renewal scheduled; a provider-renewed membership is
  canceled separately. Access combined from several payments raises
  `consistency.refund_access` instead of revoking.
- An NMI or CCBill refund whose response was lost keeps its reservation for
  operator verification; check the provider before resolving it and never retry
  with a new key. A captured success finalizes locally without a second refund.
- A refund made in the provider's dashboard, or found by an import or refresh,
  follows the merchant's `provider_refund_access` (`revoke_on_full` by default,
  `revoke_on_any`, `keep`).

Refunds and other destructive staff actions count against
[destructive-action limits](destructive-actions.md).

## Payment methods

Staff read a customer's cards and may delete one; they never create or change one.
`Client.SetSubscriptionPaymentMethod` sets a subscription's own card (or `null` to
follow the customer's default for its currency). A staff selection keeps the
dunning state and next retry: it neither advances a retry nor resumes a case
waiting for a new card. The card must be able to pay the subscription's PSP
(`409 payment_method_psp_mismatch`). Customer card flows: [frontend](frontend.md#payment-methods).

## Credit grants

`POST /v1/admin/credit-grants` (1–100 across customers, all or none) is idempotent
on each customer's `source_id`: an identical retry answers the grant with
`replayed: true`, other terms are `422 idempotency_key_reused`. Revoking takes the
unspent remainder (`409 credit_grant_held` while holds need it). Grants are listed
at `/v1/admin/credit-grants?customer_id=`; balances, limits and the ledger live
beneath `/v1/admin/customers/{customer_id}`.

## Subscription history

`GET /v1/admin/subscriptions/{id}/history?limit=50&cursor=…`
(`Client.GetSubscriptionHistory`) returns `events` (newest first by `at`) and
`coverage`. Each event has a stable `id`, `at`, `recorded_at` and one detail:

- `subscription_transition`: a committed status or paid-period change.
- `payment_attempt`: a provider answer and its classification; `action: "retry"`
  is a classification, not a promise.
- `provider_operation`: an execution phase (`unknown`, `parked` with a structured
  `reason` such as `write_posture`, `succeeded`, which alone does not prove money
  moved).
- `subscription_billing_change`: `card_selection`, `default_card`,
  `card_version` or `retry_schedule`, before and after.

Changes carry `attribution` where known: `origin`, `actor`, `executor` and
`operation_id`. A payment may be recorded after its `at`, so refresh the first
page to see late observations; paging is not a snapshot. `coverage` names the
sources and gaps: older events are not backfilled, and imported creation time is
not original signup time. Raw provider messages, payloads, credentials and card
data are never returned.
