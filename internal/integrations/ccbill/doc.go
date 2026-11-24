// Package ccbill builds CCBill FlexForm URLs and speaks DataLink. CCBill is a
// reserved gateway (rail and PSP key both ccbill) that owns the card vault and
// the rebill schedule; OpenRails never sees card data. A PSP's account is
// client_accnum-client_subacc ("999999-0000"); its secrets are the FlexForm
// salt and an optional DataLink username/password pair, which enables
// merchant-initiated cancels and the provider pull. DataLink refuses an export
// range over 24 hours, so every export is read a day at a time, and answers a
// live account once every 60 minutes: a live client takes the account's slot
// (Pacing, kept in the abuse state) before each request or fails with
// PacedError, which callers treat as "not yet". Test mode is not paced, so a
// credential probe reads in test mode and never takes the slot; the
// subscription-management actions are not DataLink exports.
//
// CCBill documents the shape of its times but not their zone. DataLink rows,
// webhook timestamps and subscription-management dates are read as UTC
// (ParseTime); a date-only field (a REBILL or EXPIRE row's timestamp, a
// rebill, renewal or expiry date) is a day, and a paid-through date runs to
// its end (DayEnd). DataLink's startTime and endTime are sent in MST.
//
// Sales: a price links a FlexForm (flex_id, recurring_billing_option_id)
// whose pricing must equal the catalog's; renewals are checked within 2%. A
// FlexForm sells subscriptions only. Paying an order on CCBill redirects to a
// signed FlexForm carrying the attempt as reservationId and the customer's
// verified email and username (else 400 customer_email_required). Only the
// NewSaleSuccess webhook naming the attempt completes the order; a sale the
// order no longer takes is booked for refund (life.order.late_payment,
// consistency.ledger.unbooked) or refused (ccbill_sale_unhonored); a sale no
// attempt issued is refused (ccbill_new_subscription_unsupported). Upgrades
// redirect with originalSubscriptionId and apply on UpgradeSuccess; there are
// no downgrades and no resume after a cancel.
//
// Webhooks have no HMAC: a delivery is admitted from CCBill's published
// ranges (internal/shared/iputil), or from the deployment's sandbox allowlist
// for a sandbox account only, and its clientAccnum/clientSubacc must match the
// PSP. eventType comes from the query and must agree with the body. A merchant
// with no armed CCBill PSP answers 5xx so CCBill redelivers. Webhooks mirror
// the subscription through the lifecycle machine: BillingDateChange never
// moves the paid period, UserReactivation resumes only inside one
// (life.ccbill.reactivation_unapplied), and a roster absence is not evidence.
// A pull reads CCBill the same way: a CANCELLATION row, or a status read
// answering 1, only stops rebilling and keeps the paid period; an EXPIRE row
// ends it.
// Subscription ids match with leading zeros ignored; the first CCBill read
// rewrites the stored id to CCBill's spelling.
//
// Refunds are not automated (ErrRefundUnsupported, 400 refund_unsupported):
// refundTransaction also cancels and takes no transaction id, and neither its
// success code nor -7 proves the amount. Refund in CCBill's admin; the Refund
// webhook, keyed by transaction id, timestamp and amount, is booked once and
// follows provider_refund_access. Cancels are durable intents running
// DataLink cancelSubscription after a status read. An imported charge with no
// amount is recorded by the pull when it reads that rebill, else raises
// life.import.transaction_unverified.
package ccbill
