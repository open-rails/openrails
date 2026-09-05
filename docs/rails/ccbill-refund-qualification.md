# CCBill refund qualification (#696)

Automatic CCBill refunds are unavailable. This applies to full, partial, and
combined cancel-and-refund requests. There is no configuration override. This
document records the defensive boundary and the evidence needed to reconsider
it; it does not certify provider execution.

## Documented contract

Reviewed 2026-09-05 against CCBill's primary documentation:

- `refundTransaction` takes a decimal amount (example `5.95`); omitting it
  selects the initial transaction's full amount. It also cancels and expires the
  subscription, including for partial refunds. `voidOrRefundTransaction` can
  void the entire transaction despite a smaller amount. Both document
  `subscriptionId`; neither documents the former adapter's `transactionId`
  selector. Subaccount credentials use `clientSubacc`; account credentials omit
  it and can select `usingSubacc`. Success code `1` alone does not establish that
  the requested charge, amount, and lifecycle outcome were honored.
  [CCBill API guide](https://ccbill.com/doc/ccbill-api-guide)
- Code `-7` has mixed causes, including temporary system errors. It cannot
  generally establish that no mutation occurred.
  [CCBill's -7 explanation](https://ccbill.com/doc/ccbill-api-7-error-explained)
- DataLink extract `testMode=1` returns synthetic export data. It is not a
  subscription-management refund simulation switch.
  [Data Link Extract guide](https://ccbill.com/doc/data-link-extract-system-user-guide)
- CCBill documents test users matched to email/IP/card and account/subaccount
  setup. Those checkout settings do not by themselves establish a supported
  DataLink refund sandbox.
  [Transaction test settings](https://ccbill.com/doc/transaction-test-settings)
- Refund webhook payloads carry transaction and subscription references,
  amounts, currencies, and timestamps. Their relationship to an exact requested
  operation still needs evidence. This is an inbound notification schema, not
  an outbound refund endpoint.
  [Refund webhook reference](https://ccbill.com/doc/refund)

## Runtime and existing operations

New requests fail before refund reservation or intent creation. A combined
request fails before its cancellation leg. The admin payment UI does not offer
automatic CCBill refunds. Cancel-only requests remain available, using status
verification after ambiguous responses, including `-7`.

Previously queued or unknown CCBill refund operations remain
`unknown_needs_verify`, with an operator-visible reason. The handler has no
outbound client. It preserves the reserved balance and stored payload/receipt;
it neither resends nor finalizes a synthetic `ccbill_refund:...` reference.
Historical completed rows are not rewritten. Provider-confirmed inbound refund
ingestion and the existing manual accounting service remain available.

For an unresolved operation, retain its merchant/payment/reservation/intent IDs
and original request evidence. Verify the provider's actual transaction,
amount, currency, and subscription state before choosing a manual accounting
resolution. Subscription counters alone cannot link an outcome to that
operation. Do not release a reservation or resend merely because the response
was `-7`, a timeout, an internal/database error, or a missing receipt. Escalate
when the result cannot be attributed; do not manufacture a golden fixture.

## Remaining provider qualification

No provider requests were made for this review. Local metadata showed a
configured host DataLink credential pair, but no authorized test-user record,
test transaction, or provider-supported SMS refund sandbox was established.
Credential presence and OpenRails' sandbox FlexForm setting are insufficient.

Before a provider exercise, supply all of the following through an approved
secret channel where applicable:

1. The authorized merchant account/subaccount, DataLink credential level and
   permissions, and approved outbound IP. CCBill requires DataLink user setup;
   avoid invalid-auth probes and lockout.
   [DataLink user configuration](https://ccbill.com/doc/datalink-user)
2. Provider confirmation of a nonfinancial SMS test environment, or explicit
   approval of a real operation. Name the exact test subscription, original
   charge, amount, currency, and whether cancellation, expiry, or a full void
   is acceptable. Checkout test mode is not that approval.
3. A provider-supported exact-targeting contract and an operation-specific
   durable receipt/readback. Without these, this payment-refund capability
   remains unsupported even if a subscription-level mutation succeeds.
4. Captured, redacted request/response and independent readback for the approved
   operation, including before/after subscription state and exact refunded
   charge/amount. Establish partial/full behavior and refusal semantics only
   within the approved test scope. Preserve a lost/uncertain response without
   resending; reconciliation must link the outcome to the same operation.

Golden fixtures must come from that authorized capture, label the environment
and provenance, remove credentials and personal data, and preserve reference
relationships. The existing manual status-read golden and synthetic test
responses are not refund qualification.

## Local regression evidence

These tests use isolated PostgreSQL and local fake HTTP endpoints only:

- `TestAdminCCBillRefundRejectedBeforeReservationAndAccessChanges`: partial and
  full requests leave no reservation/intent and preserve paid access.
- `TestFindingsCCBillRefundRefusesBeforeCancellation`: combined requests leave
  the subscription active and enqueue neither operation.
- `TestCCBillRefundReservationsAndReceiptsRemainUnresolved`: old queued/unknown
  requests, including stored synthetic receipts and old `-7` reasons, retain
  their reserved balance and payload through execution and verification.
- `TestCCBillCancelMinusSevenRequiresVerification`: cancellation uncertainty
  resolves only through a provider-state read, with no mutation in verification.
- `TestFindingsQueueApproveCCBillCancelOnly`: supported admin cancellation still
  queues and executes through the intent ledger.
