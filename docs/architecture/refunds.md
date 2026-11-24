# Refunds and disputes

This system reverses charges: a staff refund OpenRails sends to the provider, a
refund made at the provider, a chargeback, and a won dispute that returns the
money. Each reversal is a `payments` row linked to its charge; reversing a payment
that bought credit also posts its share to the [ledger](ledger.md). It also
records external payments: money a merchant received outside OpenRails, booked
against an order with no provider call.

## Concepts

- **Charge**: a positive `payments` row with `reversal_kind` NULL. A **reversal**
  has `refunded_payment_id` and a `reversal_kind`: `refund` or `chargeback`
  (negative), `dispute_reversal` (a won dispute, positive), held by
  `payments_reversal_check` and `payments_sign_check`. The refunded-payment
  foreign key includes customer and currency.
- **Reservation**: a pending negative refund row (`PaymentService.ReserveRefund`)
  holding its amount until the provider answers; `MarkFailed` releases it.
- **Refund intent**: a `provider_intents` row of type `nmi_refund` or
  `stripe_refund` carrying `intents.RefundPayload`, keyed by
  `intents.RefundIdempotencyKey`.
- **Refundable** (`PaymentService.ValidateRefund`): a succeeded charge with
  `money_movement = 'rail'`, not a usage order's payment (`ledger_transfer_id`),
  whose non-failed reversals plus this amount stay within it.
- **Credit line**: the order line that bought a credit lot; reversals return its
  money first ([prepaid credits](prepaid-credits.md)).
- **External payment**: a `payments` row with `channel = 'manual'`.
- **Ledger repair**: a `consistency.ledger.unbooked` finding for a provider
  reversal OpenRails could not book (`alerting.RecordLedgerRepair`).

## How it works

### Staff refunds

1. `POST /v1/admin/payments/{id}/refunds` takes an `Idempotency-Key` and exactly
   one of `amount` or `full`.
2. `adminRefundCommand` checks the rail (`checkAdminRefundRail`: a configured
   Stripe rail or an armed NMI account; CCBill and other rails are
   `refund_unsupported`), refundability and an exact minor amount;
   `destructive.AdmitCommand` admits a `destructive.Refund`.
3. One transaction under `LockAdminRefund` (an advisory lock per payment):
   `ReserveRefund` takes the customer and payment locks, runs
   `purchasedcredits.Service.ValidateRefund` and inserts the pending row, and the
   intent is enqueued with `AmountCents` from `NativeToRailMinorExact`. Both commit
   before any request leaves.
4. `issuePreparedAdminRefund` runs the intent inline: `201` once the reservation
   completed, `202` while the intent is parked, ambiguous or retryable. A terminal
   refusal releases the reservation and answers `refund_failed`.
5. On success `finalize` stores the provider receipt first
   (`RecordRefundProviderReceipt`). Then, in one transaction, it revokes the
   payment's access when asked (`RevokeRefundedPaymentAccess`) and completes the
   reservation (`CompleteRefundReservation`), or releases it if a provider notice
   already booked the refund. When access was revoked and the charge is fully
   refunded, it also ends a membership OpenRails renews (`endRefundedMembership`).

A retry with the same key reuses the reservation and intent
(`preparedRefundReplay`); other terms are `idempotency_key_reused`.

- **Stripe** (`StripeRefundHandler`): the intent key is the Stripe
  `Idempotency-Key`; `Verify` finds the refund by it
  (`FindRefundByIdempotencyKey`). 4xx is terminal, 429 retryable, 5xx ambiguous.
- **NMI** (`NMIRefundHandler`): no request idempotency, so a second attempt never
  sends again. It settles from a captured receipt, or stays `unknown_needs_verify`
  until an operator names the refund and the provider confirms it (`Resolve`,
  `ConfirmRefund`); a "not executed" resolution is refused.
- **CCBill**: no API refund (`ccbill.ErrRefundUnsupported`); refunds made at
  CCBill arrive as notices.

System refunds use the same reservation and intents with `OriginSystem`
(`queueFullRefund`): a renewal charged after cancellation
(`queueChargeAfterCancelRefund`) and an order paid after it closed
(`RefundLateOrderPayment`).

### Provider-side reversals

- Refunds made at the provider arrive by webhook (Stripe `refund.created`,
  `refund.updated`, `charge.refunded`; NMI; CCBill), refresh and import
  (`RecordObservedRefund`, `recordDeclaredRefunds`). `recordRefund` takes the
  customer, subscription and payment locks, replays a known provider transaction
  id with the same facts and refuses one with other facts, defers while an
  OpenRails refund of the payment is pending (`ErrRefundReservationPending`), and
  applies the merchant's `provider_refund_access` (`ApplyRefundAccessPolicy`).
- Chargebacks (`ReversalChargeback`) come from Stripe `charge.dispute.created`
  (or `charge.dispute.closed` as lost), NMI chargeback reports and CCBill
  `Chargeback` notices. Each records the reversal, then cancels the subscription
  (`CancelTypeChargeback`) or ends a one-off purchase's access. A reversal it
  cannot book (no matching charge, more than remains) raises
  `consistency.ledger.unbooked`; access still ends.
- A won Stripe dispute (`handleStripeDisputeWon`) records a positive
  `dispute_reversal` whose `metadata.reverses_payment_id` names the chargeback,
  and reactivates a provider-managed subscription this dispute canceled. An
  engine-collected one stays canceled, and a one-off purchase's access is not
  restored; it is only logged for review.

After any reversal of a charge that funded a lot, `syncPurchasedCreditRefund` runs
`purchasedcredits.Service.ApplyReversal` per reversal, negatives oldest first and
then recoveries, until none posts.

### External payments

`POST /v1/admin/payments` records money received outside OpenRails for one
engine-collected order: up to what a usage order owes (`RecordUsageRemittance`,
an `owed_payment` at `manual_invoice_payment`), or exactly another order's total
(`orders.Service.RecordRemittance`). `transaction_id` is its identity
(`payments_channel_transaction_id_key`). Its `money_movement` is `none`, so it is
never refundable through OpenRails.

## Interfaces

- Go: `Client.RefundPayment` (`billing.RefundPaymentParams`), `Client.GetPayment`, `Client.ListPayments`, `Client.RecordExternalPayment` (`billing.RecordExternalPaymentParams`). `billing.PaymentKind` is `charge`, `refund`, `chargeback` or `dispute_reversal`; a charge with reversals reads `refunded` or `partially_refunded` by their net, so a won dispute gives back what its chargeback took. Sentinels: `billing.ErrRefundUnsupported`, `billing.ErrRefundRailUnavailable`.
- HTTP: `POST /v1/admin/payments/{id}/refunds` and `POST /v1/admin/payments`
  (`merchant:customer:manage`); `GET /v1/admin/payments` and
  `GET /v1/admin/payments/{id}` (`merchant:customer:read`); `GET /v1/me/payments`.
- Error codes: `idempotency_key_required`, `idempotency_key_reused`,
  `invalid_param`, `payment_not_found`, `payment_not_refundable`,
  `refund_unsupported`, `refund_rail_unavailable`, `refund_failed`,
  `payment_exceeds_due`, `order_not_payable`, `order_has_recurring_line`.
- Merchant setting: `provider_refund_access` (`revoke_on_full`, `revoke_on_any`,
  `keep`). Events: none of its own; `payment.settled` fires only for charges.
- Docs: [merchant operations](../client.md#payments-and-refunds),
  [provider uncertainty](../standalone.md#provider-intents),
  [destructive actions](../standalone.md#destructive-holds).

## Guarantees

### 1. Exactly-once provider calls
The reservation and the intent commit before the request is sent; the intent is
unique per merchant on its key (`provider_intents_idempotency_key_key`). Just
before the call, `startProtectedEffect` starts the staff admission and fences the
send to this attempt (`FenceDestructiveProviderSend`). Stripe dedupes on the
intent key; NMI never resends and needs a captured receipt or a provider-confirmed
resolution. An ambiguous outcome keeps the reservation.

Checks:
- `prepareAdminRefund` enqueues the intent in the transaction that calls
  `ReserveRefund`; `issuePreparedAdminRefund` runs only after it commits.
- `NMIRefundHandler.Execute` calls `recoverReceipt`, never `client.Refund`, when
  `intent.Attempts > 1`.
- `StripeRefundHandler.Execute` passes `intent.IdempotencyKey` as the Stripe key.
- `finalize` writes the receipt under `LedgerWriteContext` before its transaction.
- OR-J20-S03: a lost Stripe refund response is found by its key and booked once.

### 2. Agreement with providers
Every provider fact is keyed by its transaction id
(`payments_psp_id_transaction_id_key`), so a webhook, refresh and import of one
refund book it once. A provider refund seen while an OpenRails refund is pending
waits instead of counting twice. A fact that cannot be booked becomes a finding,
not a guess. Refresh reads reversals on their own, so a dashboard refund lands
even when the full read fails (OR-J20-S70).

Checks:
- `refundLocked` returns the existing row for a known transaction id and errors
  when its facts differ, and, for a refund fact, returns
  `ErrRefundReservationPending` while a pending refund reservation exists.
- Chargeback handlers call `alerting.RecordLedgerRepair` when `Refund` fails.
- OR-J20-S05: CCBill partial refunds book once each; an excess books nothing and
  raises one finding.

### 3. Exact money
Refunds are native units in the charge's currency; the reversal inherits it and
the foreign key enforces it. Outbound, `refundAmountCents` converts exactly
(`NativeToRailMinorExact`); NMI sends `nmi.WireAmount`, Stripe the minor integer.
Inbound, Stripe and CCBill amounts widen with `moneyutil.CentsToMicros`; NMI
notices go through `nmiRefundAmount` (another currency is refused) and
`RailMinorToNative`, refresh through `RemoteTransaction.nativeIn`.

Checks:
- `DecodeRefundPayload` refuses a payload without a canonical fiat currency.
- OR-J20-S01 pins the provider effects: 3000 charged, 1000 and 2000 refunded.
- `GetRefundTotalByPaymentID` nets reversals and `ValidateRefund` keeps the total
  within the charge, also under concurrency (OR-J20-S02).
- The Stripe refund and dispute paths convert with `CentsToMicros` without
  reading the object's currency: confirm it must equal the charge's.

### 4. Access control and tenant isolation
Refund and external-payment routes need `merchant:customer:manage`, reads
`merchant:customer:read`; a customer lists only its own payments
(`selfAccountPayer`). Every payment read requires the bound merchant
(`merchant.Require`) and every query names it.

Checks:
- OR-J20-S04: another merchant's refund of the payment is `ErrNotFound` and moves
  nothing.
- `RefundPayment` is `Level: LevelUpdate` in `internal/http/routes/payments.go`.
- OR-J21-S03: a receipt cannot be relinked to another customer or merchant.

### 5. Contracts match the code
The methods, `billing.Payment`, `billing.RefundPaymentParams` and the routes are
in `api/go.txt` and `api/openapi.json`; `payments` and `provider_intents` are in
`api/schema.txt`. The in-process client calls the same routes as the remote one.

Checks:
- `go run ./scripts/contracts` shows no removed line here.
- A new reversal kind adds to the `reversal_kind` CHECK and `billing.PaymentKind`.

### 6. Data lifecycle
`payments` is permanent, soft-deleted only by `openrails prune`. Refund intents
are permanent (they moved or refused money); `provider_mutation_logs` rows go after
25 months, `webhook_events` 90 days after completion. Nothing here is partitioned.

Checks:
- `internal/retention/tables.go`: `payments` `Permanent`, `provider_mutation_logs`
  `Rows`, `provider_intents` `Rows` but kept when it moved or refused money.
- Reads filter `deleted_at IS NULL` (`ListRefundsForPayment`,
  `ListRefundRowsForTotal`).

### 7. Work scales with activity
Staff refunds run inline; an unfinished intent has its own
`openrails.provider_operation` job and `provider_intents_next_attempt_at_idx`
finds it. Provider facts arrive by webhook or watermarked refresh.

Checks:
- Refund paths read reversals by `refunded_payment_id` or a provider transaction
  id (`ListRefundsForPayment`, `ListRefundRowsForTotal`, `GetByPSPTransactionID`).
- `syncPurchasedCreditRefund` loops over one charge's reversals only.

### 8. Abuse protection
Refunds are staff-only. Provider webhooks use the `webhook` bucket and per-rail
body caps ([limits](../standalone.md#abuse-limits)). Staff refunds are bounded by destructive
admission, external payments by `middleware.AdminOperationOffChannel` (10 a
minute).

Checks:
- No customer route reaches `PaymentService.ReserveRefund` or
  `service.Service.RecordPayment`.
- `RecordExternalPayment` refuses a `transaction_id` containing card data
  (`cardguard.ContainsPAN`).

### 9. Destructive-action safeguards
Every staff refund is a `destructive.Refund` command, admitted before anything is
reserved and finished with its outcome; a replay spends nothing again. The intent
records `OriginAdmin` and the staff subject, which `finalize` names on revoked
access. System refunds are autonomous maintenance and spend no staff allowance.

Checks:
- `executeAdminRefund` refuses when `DestructiveAdmissions` is nil;
  `startProtectedEffect` refuses an `OriginAdmin` intent with no admission.
- A failed transaction finishes the admission `OutcomeNoEffect` or
  `OutcomeUncertain` (`executeAdminRefund`).
- OR-J34-S01 to S04 test the shared ceilings and holds, through credit
  revocations rather than refunds.

## Tests

- J20: OR-J20-S01 partial then full refund (`TestRefunds`); OR-J20-S02 concurrent refunds, OR-J20-S03 lost response, OR-J20-S70 dashboard refund under a failing read (`TestRefundRecovery`); OR-J20-S04 foreign and off-rail refunds (`TestRefundBoundaryContract`); OR-J20-S05 CCBill partial refunds (`TestCCBillRefunds`); OR-J20-S06 a won dispute nets against its chargeback (`TestRefunds`).
- J21: OR-J21-S01 to S03 external payments (`TestExternalPayments`); OR-J21-S04
  imported refund allowance (`TestImportedReceipt`).
- Credit-line reversals (`TestMixedOrderCreditRefunds`): OR-J22-S07 (a pending
  refund holds the lot), S08 (refunds, chargeback, won dispute), S09 (dispute credit).
- OR-J27-S04: a staff refund's `revoke_access`, and a full revoking refund ends
  the membership; OR-J27-S07 repair after a chargeback; OR-J38-S37 (CCBill
  chargeback); OR-J38-S43 (a late CCBill sale held for refund).
- No Go package test covers this system; `sqlc vet` prepares its queries.

Gaps:
- No scenario sends an NMI refund through `RefundPayment`: `NMIRefundHandler`'s
  execute, verify and resolve paths are untested end to end.
- No scenario ingests an NMI chargeback.
- No scenario sets `provider_refund_access`: `ApplyRefundAccessPolicy` is untested.
