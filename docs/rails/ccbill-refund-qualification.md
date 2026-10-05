# CCBill refunds

Automatic CCBill refunds are unavailable: full, partial and combined
cancel-and-refund requests are refused (`400 refund_unsupported`), with no
configuration override. Refund in CCBill's admin; OpenRails ingests the
resulting `Refund` webhook and applies the merchant's `provider_refund_access`
setting.

## Why

CCBill's documented refund calls cannot target one charge for an exact amount
and prove the outcome:

- `refundTransaction` takes a decimal amount (example `5.95`); omitting it
  selects the initial transaction's full amount. It also cancels and expires the
  subscription, including for partial refunds. `voidOrRefundTransaction` can
  void the entire transaction despite a smaller amount. Both document
  `subscriptionId`; neither documents a `transactionId` selector. Subaccount credentials use `clientSubacc`; account credentials omit
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

## What OpenRails does

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
when the result cannot be attributed.
