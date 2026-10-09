# Customer payment recovery

A customer can pay an unpaid invoice or retry a past-due subscription from the
existing self-service billing surface. These commands accept no customer identifier:
the admitted customer owns the addressed resource. `Idempotency-Key`
is required, customer-scoped and bound to the accepted request. Same-key replay
returns the existing result after settlement; a different key cannot displace
unresolved work. The existing invoice/subscription read exposes `recovery` and
any unresolved operation, without a second operation API.

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

For NMI invoice pay, a customer action freezes initial or subsequent unscheduled
stored-credential posture. An initial approved reference is captured only from
the qualified provider receipt, atomically with invoice settlement and operation
completion. Local rollback retains custody and recovery commits it without
charging again. Future off-session invoice collection requires that approved
reference. Rounded provider collection settles the exact invoice liability and
credits the excess through the existing spendable credit ledger.

Subscription retry uses the same immutable admission and completion as the
scheduled worker. It freezes customer-initiated recurring reuse of the existing
approved agreement, the subscription's current method, price/benefits and paid
period. The NMI request remains `recurring=rebill_subscription`; preparation
checks the supported fixed-day calendar against the actual provider record.
The client command does not invent new calendar semantics or reset a lapsed
period to the click time. Unsupported cadence or preparation stays refused or
unresolved before a money write.

Current customer-pay support is NMI with PSP-held cards. Unsupported rails or
custody paths return `customer_payment_unsupported`; Stripe's administrative
and scheduled off-session collection is unaffected. Invoice payment does not
reactivate an unrelated subscription.

HTTP commands:

- `POST /v1/me/invoices/{id}/pay-now` with `payment_method_id`.
- `POST /v1/me/subscriptions/{id}/retry-now` with optional `payment_method_id`
  (when supplied, it must be the subscription's current method).

A completed result is 200; unresolved execution is 202 with the operation's
identity and state. A definitive card refusal uses the existing coded 402 error
envelope (processor failures retain the standard processor error mapping).
Error metadata includes the operation ID. Replaying the same key returns that
original refusal even if a later attempt has recovered the account.
`payment_in_progress` and `payment_idempotency_conflict` are 409 refusals.
A caller should retain its key through network uncertainty and read the existing
resource before starting a different action.

Fresh customer invoice/subscription reads include `recovery.last_failure_reason`
when the latest applicable collection failed. This is the same normalized machine
category as a payment error's `decline_reason` metadata, never provider response
text. The existing invoice collection failure count/next attempt and subscription
retry count/next retry fields accompany it. A successful recovery, a newer pending
attempt, or a different subscription period does not inherit an old refusal.
