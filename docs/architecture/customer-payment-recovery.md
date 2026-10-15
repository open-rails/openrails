# Customer payment recovery

A customer can pay an unpaid renewal or usage order, or retry a past-due
subscription the PSP schedules, from the self-service billing surface. These commands accept no customer identifier:
the admitted customer owns the addressed resource. `Idempotency-Key`
is required, customer-scoped and bound to the accepted request. Same-key replay
returns the existing result after settlement; a different key cannot displace
unresolved work. The order or subscription read exposes the payment's state
and any unresolved operation, without a second operation API.

These are customer HTTP routes; the Go Client carries merchant routes only.
The mount's `Auth` admits the customer: the identity's subject is the customer,
and only a user acting in person (its invoker is itself, and its credential is
a session, a device key or an access token, not an API key or a signed token)
initiates a payment. An invoker acting on the customer's behalf, an
application subject, or a user automating its own account with an API key
keeps its read routes but is refused a customer-initiated charge with
`403 customer_action_required`. Nothing in a request header or body raises a
credential to in person, and a merchant credential never becomes
customer-present by naming a payment method. The default runtime Client is the
merchant-owner client and is not a customer credential.

Usage billed in arrears is an order too (`origin: usage`, see
[usage orders](../usage-orders.md)). For an NMI usage order, the customer's
payment freezes the card's lineage it cites. A storing charge's reference is
recorded on the card's mandates only from the qualified provider receipt,
atomically with the order's settlement and the operation's completion. Local
rollback retains custody and recovery commits it without charging again.
Off-session collection of a usage order requires the currency's active
unscheduled mandate. Rounded provider collection settles the exact amount the
order owes and credits the excess through the spendable credit ledger.

Each renewal of an engine-owned subscription is an order (`origin:
subscription`, with `renewal`: the subscription, its period and `due_at`), and
each charge of it, scheduled or the customer's, is an attempt on that order. A
declined renewal stays `open` with `payment.last_payment_error`; the
subscription's `recovery.order_id` names it, and it is overdue once past its
`due_at`. The customer pays it with `POST /v1/me/orders/{id}/pay` and a saved
card (`payment.payment_method_id`): another card becomes the subscription's,
and a card whose agreements wait for consent after a brand change is verified
first. A renewal the bank asks the customer to authenticate is
`requires_action`: the customer is told, completes `next_action` and calls
`POST /v1/me/orders/{id}/confirm`. The customer cannot cancel a renewal;
ending the subscription voids it (`order.voided`). The payment uses the
scheduled worker's admission and completion: the subscription's frozen terms
and paid period, never a period reset to the click time.

Subscriptions the PSP schedules (NMI with PSP-held cards) retry with
`POST /v1/me/subscriptions/{id}/retry-now`. Unsupported rails or custody paths
return `customer_payment_unsupported`; an engine-owned subscription's renewal
is paid as its order instead. Paying a usage order does not reactivate an
unrelated subscription.

HTTP commands:

- `POST /v1/me/orders/{id}/pay` with `payment.payment_method_id` and
  `expected_total`, for a renewal or usage order.
- `POST /v1/me/subscriptions/{id}/retry-now` with optional `payment_method_id`
  (when supplied, it must be the subscription's current method), for a
  subscription the PSP schedules.

An order answers as every order does (200, or a coded 402 on a decline).
`retry-now`'s completed result is 200 (`status: succeeded`); unresolved
execution is 202 (`status: processing`), read back with the same key. A
definitive card refusal uses the existing coded 402 error envelope (processor
failures retain the standard processor error mapping). Replaying the same key
returns that original refusal even if a later attempt has recovered the
account.
`payment_in_progress` and `payment_idempotency_conflict` are 409 refusals.
A caller should retain its key through network uncertainty and read the existing
resource before starting a different action.

The dunning dates describe the planned schedule. For retries OpenRails runs,
`dunning.paused` reports an operating-mode hold without changing those dates:
`pause_reason: automatic_collection_paused` means limited mode, while
`provider_writes_disabled` means readonly. Effective mode includes the
deployment, merchant and billing-book restrictions. Restoring effective full
mode clears this automatic hold on the next read; it does not guarantee that
the card, provider or pending payment permits a charge. An existing
`waiting_for_new_card` or failure reason remains meaningful while paused.
OpenRails' mode does not pause retries managed by an external provider.

Customer-requested payment is separate: limited mode permits otherwise
eligible customer payments, even while automatic retries wait. Readonly makes
an otherwise retryable recovery answer `retryable: false` with
`blocked_reason: provider_writes_disabled`. Existing `payment_in_progress`,
`authentication_required` and unsupported/nonretryable reasons keep their
meaning and priority. No raw operating-mode diagnostics are exposed in these
fields. Reads do not charge, reschedule or arm anything; commands still check
current eligibility under the appropriate lock.

A declined order carries `payment.last_payment_error`; a subscription's
`recovery.last_failure_reason` names why its latest collection failed. This is
the same normalized machine category as a payment error's `decline_reason`
metadata, never provider response text. A usage order's
`usage.next_collection_attempt_at` and the subscription's retry count and next
retry accompany it. A successful recovery, a newer pending
attempt, or a different subscription period does not inherit an old refusal.
