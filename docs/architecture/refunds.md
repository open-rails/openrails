# Refunds and disputes

This system reverses charges: a staff refund OpenRails sends to the provider, a refund made at the provider, a chargeback, and a won dispute that returns the money. Each reversal is a `payments` row linked to its charge, and posts to the [books](books.md): the general ledger and, for a payment that bought credit, its share to the units book. It also records external payments: money a merchant received outside OpenRails, booked against an invoice with no provider call.

## Concepts

- **Charge**: a positive `payments` row with `reversal_kind` NULL. A **reversal**
  has `refunded_payment_id` and a `reversal_kind`: `refund` or `chargeback`
  (negative), `dispute_reversal` (a won dispute, positive), held by
  `payments_reversal_check` and `payments_sign_check`. The refunded-payment
  foreign key includes customer and currency.
- **Reservation**: a pending negative refund row (`PaymentService.ReserveRefund`)
  holding its amount until the provider answers; `MarkFailed` releases it. The
  credit the payment bought leaves the balance at once and comes back on a
  release ([credits](credits.md#refunds-and-chargebacks)).
- **Refund intent**: a `provider_intents` row of type `nmi_refund` or
  `stripe_refund` carrying `intents.RefundPayload`, keyed by
  `intents.RefundIdempotencyKey`.
- **Refundable** (`PaymentService.ValidateRefund`): a succeeded charge with `money_movement = 'rail'` whose non-failed reversals plus this amount stay within it. A usage invoice's payment (`ledger_transfer_id`) is refundable and can be charged back: a refund leaves its owed settled, a chargeback owes it again (`owed.SyncUsageReversals`, [books](books.md)).
- **Credit line**: the invoice line that bought a credit lot; reversals return its money first ([credits](credits.md)).
- **External payment**: a `payments` row with `channel = 'manual'`.
- **Dispute**: a `disputes` row per provider case (`disputes.Service.Record`):
  `kind` `inquiry` (nothing withdrawn), `chargeback`, or `prevented` (an RDR
  refund or a Compelling Evidence 3.0 block), with its network, reason family
  (`disputerules.Classify`), code, dates and outcome. The money stays in the
  `payments` rows it names (`reversal_payment_id`, `recovery_payment_id`).
- **Fraud report**: a `fraud_reports` row per Stripe early fraud warning (Visa
  TC40, Mastercard SAFE).
- **Refund reason**: `payments.refund_reason`, set exactly on refund rows.
- **Network activity**: `network_activity_daily`, card sales, refunds,
  chargebacks and fraud reports per PSP, network, currency and UTC day, kept by
  triggers on `payments`, `disputes` and `fraud_reports`.
- **Ledger repair**: a `consistency.ledger.unbooked` finding for a provider
  reversal OpenRails could not book (`alerting.RecordLedgerRepair`), one per
  event: its idempotency key, else its operation, transaction and the
  resources it names (OR-J06-S71).

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
   `purchasedcredits.Service.ValidateRefund`, inserts the pending row and takes
   the credit it returns off the balance (`WithdrawRefund`), and the
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
   refunded, it also ends a membership OpenRails renews (`subscriptions.EndRefundedMembership`).

A retry with the same key reuses the reservation and intent
(`preparedRefundReplay`); other terms are `idempotency_key_reused`.

- **Stripe** (`StripeRefundHandler`): the intent key is the Stripe
  `Idempotency-Key`; `Verify` finds the refund by it
  (`FindRefundByIdempotencyKey`). 4xx is terminal, 429 retryable, 5xx ambiguous.
- **NMI** (`NMIRefundHandler`): no request idempotency, so a second attempt never
  sends again. A refusal NMI answered (a v5 4xx, a decline, or a response 3 the
  gateway did not process, `nmi.ErrSaleRefused`) releases the reservation and
  keeps NMI's reason. Any other outcome settles from a captured receipt, or stays
  `unknown_needs_verify` with a `life.refund.unresolved` finding until an operator
  names the refund and NMI confirms it is a refund of the original sale
  (`Resolve`, `ConfirmRefund`), or attests release when NMI's report shows no
  refund of the sale that OpenRails has not recorded (`ReadRefundTransactions`).
- **CCBill**: no API refund (`ccbill.ErrRefundUnsupported`); refunds made at
  CCBill arrive as notices.

System refunds use the same reservation and intents with `OriginSystem`
(`queueFullRefund`): a renewal charged after cancellation
(`queueChargeAfterCancelRefund`) and an invoice paid after it closed
(`RefundLateInvoicePayment`).

### Provider-side reversals

- Refunds made at the provider arrive by webhook (Stripe `refund.created`,
  `refund.updated`, `charge.refunded`; NMI; CCBill), refresh and import
  (`RecordObservedRefund`, `recordDeclaredRefunds`). `recordRefund` takes the
  customer, subscription and payment locks, replays a known provider transaction
  id with the same facts and refuses one with other facts, defers while an
  OpenRails refund of the payment is pending (`ErrRefundReservationPending`), and
  applies the merchant's `provider_refund_access` (`ApplyRefundAccessPolicy`).
  When access was revoked and the payment is fully refunded, the same
  `subscriptions.EndRefundedMembership` rule ends an engine-collected membership:
  now for its latest paid period, otherwise at period end. Provider-owned
  subscriptions and access kept for review keep their renewal. A partial refund
  under `revoke_on_any` revokes access but keeps the membership.
- Disputes come from Stripe's `charge.dispute.*` events (classified by status and card `case_type`, `disputerules.StripeKind`), the reconcile pull's `/v1/disputes` read (`reconcile.DisputeRecorder`), NMI chargeback batches (every entry, matched or not) and CCBill `Chargeback` notices. Each is recorded; a case is acted on by the kind its notice reports. A chargeback (`ReversalChargeback`) records the reversal on whichever notice says so first, cancels the subscription (`CancelTypeChargeback`) or ends a one-off purchase's access; its response blocks the card the charge was taken from (`payments.payment_method_id`, `disputes.Service.BlockCard`): the method is `disputed` and its mandates end (`disputed`). A charge names its card when OpenRails charged a saved one, when a Stripe Billing invoice's charge names a saved payment method, and when an NMI schedule's sale names the vault of the customer's one card in it; one that names none blocks none, and the customer review says so. An inquiry, a status Stripe had not reported before (`unknown`) and a CE 3.0 block book nothing; an RDR resolution books one refund (`dispute_prevention`), whichever of it and Stripe's refund arrives first: a Stripe refund of the whole charge made no earlier than five minutes before the case, which OpenRails did not ask for, is absorbed, its id recorded on the case (`absorbed_refund_ref`), each refund by one case. Stripe's order of the two is unconfirmed. A won, lost or closed case stays so against a later notice from before the decision, and a chargeback or a prevented case keeps its kind against a stale notice. A reversal it cannot book raises `consistency.ledger.unbooked`; a matched charge still loses access, while an unmatched NMI entry revokes and cancels nothing. NMI matches amount and card last four digits within ±7 days; `openrails disputes attach` lets an operator identify an unmatched entry's charge, book its reversal and end what it bought. An already fully refunded charge takes no second reversal: the finding advises contesting the chargeback with the processor and citing the refund; a lost chargeback is outside OpenRails' books. The customer review names the refund when one already succeeded; otherwise it tells staff that suspending the customer's account belongs in their application. A dispute or fraud report on a charge OpenRails has no record of raises `life.disputes.unknown_charge` and is answered.
- A won Stripe dispute (`handleStripeDisputeWon`) records a positive
  `dispute_reversal` whose `metadata.reverses_payment_id` names the chargeback,
  and reactivates a provider-managed subscription this dispute canceled. The
  dispute's `won` status is final against later notices. An engine-collected
  subscription stays canceled, and a one-off purchase's access is not restored;
  it is only logged for review.
- A Stripe early fraud warning (`handleEarlyFraudWarning`) is recorded once, from whichever event reports it, and stays actionable only while every event says so. An actionable one received while `fraud_warning_refund` is `refund` is `pending` and queues `openrails.dispute_response`, which refunds its charge in full (`intents.QueueFraudWarningRefund`, `fraud_warning`, sent to Stripe as `fraudulent`), ends what it paid for and blocks its card, unless an open dispute (or an unbooked chargeback) decides the charge, checked under the report's lock. A charge no refund can reach (nothing left, no money moved at the rail, a usage invoice's settlement, credits bought and spent, no provider target) is `nothing_to_refund` and still ended. A report received while the merchant recorded reports only stays recorded when the setting changes.
- `openrails.dispute_response` also answers each chargeback: it blocks the card. Both then cancel at period end the customer's other provider-billed subscriptions on the card. Each step runs in one transaction as an autonomous `dispute_response` command (`destructive.Admissions.AdmitAutonomous`; the block is `block_payment_method`), keyed on its case and step, under a `full` write posture and an allowance of its own (`dispute:<merchant>`, apart from the merchant's other autonomous work); a case a reconcile pull found waits for the merchant to be armed or a provider's notice of it. The refund, the end and the block are protective and do not wait for the merchant's destructive arming; the period-end cancels are destructive and do. A failed wake is retried with backoff, and an unreadable posture is retried as an error, not waited on. A held response raises `life.disputes.response_waiting` (high, alerting once) and ends; a change of the merchant's writes or destructive arming, the book identity or a cleared hold notifies `openrails_dispute_responses`, and `disputes.Service.Run` queues every waiting response again, as on every start.

### General ledger

A settled reversal posts to the general ledger once, in the transaction that settles it (`books.PostReversal`): at the end of `recordRefund`, after the provider access policy; in `CompleteRefundReservation`, after the access a staff refund revokes; and where a won dispute's recovery is stored or linked. The credit lot the payment bought goes first, by its own poster; the rest leaves what the payment left unapplied, so a late or duplicate payment's refund comes from `unapplied_cash`; then each schedule the payment funded, pro rata: a price reduction with catch-up while its service runs, its unrecognised revenue first once the service has ended, the rest to `refunds` or `chargebacks`. A won dispute brings back, pro rata, what its chargeback took. The legs are dated when the reversal settled: a staff refund the rail confirms later, at its confirmation. A refund of a payment from before the books is never refused: `unapplied_cash` goes below zero and raises `books.sign_breach`. `ledger-audit` names any receipt or settled reversal stored without its posting and raises `books.entry.missing`. The legs are in [books](books.md#reversals).

### Dispute monitoring

Recording a chargeback or a fraud report queues `openrails.dispute_monitor` for its PSP in the same transaction (`disputejob.QueueMonitor`, unique while queued), and so does a sale settling on a PSP with an open threshold finding (`disputejob.QueueMonitorIfFlagged`). The job reads at most 62 days of `network_activity_daily` and evaluates each program in `disputerules.Programs` (and `AcquirerProgram` for a PSP's `dispute_ratio_limit`) over the month so far, its ratio scaled to the whole month (the last 30 days before its 7th day); a level's minimum counts are met only by what was counted. Mastercard divides by the previous month's sales. A program at its warning level raises `life.disputes.threshold` (high), at its excessive level (critical), subject `<psp>:<program>:<YYYY-MM>`; both alert through `alerting.Service.NotifyFinding`, and an evaluation below the warning level resolves it. A finding of a month that has ended is settled on the whole month at the PSP's next evaluation. The job also alerts on `life.disputes.customer_review` findings each chargeback raises. `GET /v1/metrics/dispute-programs` evaluates the same programs for any month.

After any reversal of a charge that funded a lot, `syncPurchasedCreditRefund` runs
`purchasedcredits.Service.ApplyReversal` per reversal, negatives oldest first and
then recoveries, until none posts.

### External payments

`POST /v1/admin/payments` records money received outside OpenRails for one
engine-collected invoice: up to what a usage invoice owes (`RecordUsageRemittance`,
an `owed_payment` at `manual_invoice_payment`), or exactly another invoice's total
(`invoices.Service.RecordRemittance`). `transaction_id` is its identity
(`payments_channel_transaction_id_key`). Its `money_movement` is `none`, so it is
never refundable through OpenRails.

## Interfaces

- Go: `Client.RefundPayment` (`billing.RefundPaymentParams`), `Client.GetPayment`, `Client.ListPayments`, `Client.ListDisputes`, `Client.GetDispute`, `Client.ListDisputePrograms`, `Client.RecordExternalPayment` (`billing.RecordExternalPaymentParams`). `billing.PaymentKind` is `charge`, `refund`, `chargeback` or `dispute_reversal`; a charge with reversals reads `refunded` or `partially_refunded` by their net, so a won dispute gives back what its chargeback took. Sentinels: `billing.ErrRefundUnsupported`, `billing.ErrRefundRailUnavailable`.
- HTTP: `POST /v1/admin/payments/{id}/refunds` and `POST /v1/admin/payments`
  (`merchant:customer:manage`); `GET /v1/admin/payments` and
  `GET /v1/admin/payments/{id}`, `GET /v1/admin/disputes` and
  `GET /v1/admin/disputes/{id}` (`merchant:customer:read`);
  `GET /v1/metrics/dispute-programs`; `GET /v1/me/payments`.
- Error codes: `idempotency_key_required`, `idempotency_key_reused`,
  `invalid_param`, `payment_not_found`, `payment_not_refundable`,
  `refund_unsupported`, `refund_rail_unavailable`, `refund_failed`,
  `payment_exceeds_due`, `invoice_not_payable`, `invoice_has_recurring_line`.
- Merchant settings: `provider_refund_access` (`revoke_on_full`, `revoke_on_any`,
  `keep`), `fraud_warning_refund` (`refund`, `record`); PSP setting
  `dispute_ratio_limit`. Events: none of its own; `payment.settled` fires only for charges.
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
(`selfAccountCustomer`). Every payment read requires the bound merchant
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
`payments` is permanent, soft-deleted only by `openrails prune`. Refund intents are permanent (they moved or refused money); `operation_log` entries go after 25 months, `webhook_events` 90 days after completion. Nothing here is partitioned.

Checks:
- `internal/retention/tables.go`: `payments` `Permanent`, `operation_log` `Rows`, `provider_intents` `Rows` but kept when it moved or refused money.
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
- The dispute monitor runs per chargeback or fraud report, or per sale while a finding is open (`PSPHasOpenThresholdFinding`), and reads a PSP's `network_activity_daily` days, never its payments.

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
Every staff refund is a `destructive.Refund` command, admitted before anything is reserved and finished with its outcome; a replay spends nothing again. The intent records `OriginAdmin` and the staff subject, which `finalize` names on revoked access. System refunds are autonomous maintenance and spend no staff allowance; a dispute response spends its own allowance and waits behind a hold and a write posture below `full`, and its period-end cancels behind the merchant's destructive arming.

Checks:
- `executeAdminRefund` refuses when `DestructiveAdmissions` is nil;
  `startProtectedEffect` refuses an `OriginAdmin` intent with no admission.
- A failed transaction finishes the admission `OutcomeNoEffect` or
  `OutcomeUncertain` (`executeAdminRefund`).
- OR-J34-S01 to S04 test the shared ceilings and holds, through credit
  revocations rather than refunds.

## Tests

- J20: OR-J20-S01 partial then full refund (`TestRefunds`); OR-J20-S02 concurrent refunds, OR-J20-S03 lost response, OR-J20-S70 dashboard refund under a failing read, OR-J20-S07 NMI refusals released, OR-J20-S08 an unresolved NMI refund closed by the operator (`TestRefundRecovery`); OR-J20-S04 foreign and off-rail refunds (`TestRefundBoundaryContract`); OR-J20-S05 CCBill partial refunds (`TestCCBillRefunds`); OR-J20-S06 a won dispute nets against its chargeback (`TestRefunds`).
- J21: OR-J21-S01 to S03 external payments (`TestExternalPayments`); OR-J21-S04
  imported refund allowance (`TestImportedReceipt`).
- J15 (`TestProviderTime`): OR-J15-S73 keeps sales changed after the prune window; OR-J15-S75 keeps pending refund reservations and booked NMI chargebacks out of prune excess.
- J46 (`TestDisputes`): OR-J46-S01 a chargeback; S02 an inquiry, its escalation and the reconcile pull; S03 RDR and CE 3.0; S04 won and lost disputes stay decided; S06 CCBill; S07 early fraud warnings; S08 the monitor's findings, alerts and resolution; S09 Mastercard's previous-month basis; S10 the levels in force; S11 the blocked card; S05 an NMI batch; S12 the dispute metrics; S13 the rollup across a restore; S14 refund reasons; S15 only the charged card is blocked; S16 reports on unknown charges; S17 an operator attaches an unmatched NMI chargeback; S18 a chargeback of a refunded charge advises contesting it.
- Credit-line reversals (`TestMixedInvoiceCreditRefunds`): OR-J22-S07 (a pending
  refund holds the lot), S08 (refunds, chargeback, won dispute), S09 (dispute credit).
- OR-J20-S09: a full NMI dashboard refund ends an engine membership and prevents
  renewal; redelivery changes nothing, and a partial refund keeps access and renewal.
- OR-J27-S04: a staff refund's `revoke_access`, and a full revoking refund ends
  the membership; OR-J27-S07 repair after a chargeback; OR-J38-S37 (CCBill
  chargeback); OR-J38-S43 (a late CCBill sale held for refund).
- No Go package test covers this system; `sqlc vet` prepares its queries.

Gaps:
- No scenario sets a nondefault `provider_refund_access` policy.
