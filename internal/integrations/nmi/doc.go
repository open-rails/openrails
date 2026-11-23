// Package nmi is the only path to NMI's gateway: the v5 JSON API
// (DefaultV5BaseURL), Classic Direct Post (DefaultDirectPostURL) and the
// Query API (DefaultQueryAPIURL), or their sandbox.nmi.com twins. The rail is
// nmi; each NMI merchant account is one PSP, named by its dashboard Gateway
// ID (`gateway_id`), which is the merchant account id. Nothing reads it from
// the security key, so the key identifies the account: a second live PSP
// holding another's key stays disarmed with consistency.duplicate_gateway_account.
//
// Wire quirks the client handles:
//   - v5 auth is the bare security key as the whole Authorization value.
//   - Enrollment (recurring=add_subscription), stored-card sales, manual
//     rebills and subscription updates stay on Classic: v5 lacks them, and
//     PATCH /v5/subscriptions/{id} answers E_ROUTE_NOT_FOUND live. Search
//     stays on query.php.
//   - Every stored-card authorization carries initiated_by and
//     stored_credential_indicator derived from the flow, and each later
//     CIT/MIT its mandate's initial transaction id, through the PSP that ran
//     it. billing_method=recurring is sent on recurring charges; confirm with
//     the acquirer before changing it.
//   - A canceled subscription is deleted: GET /v5/subscriptions/{id} answers
//     404, a terminal state. An E_NOT_FOUND or deleted record ends a held
//     subscription; any other 404 is retried.
//   - A duplicate refusal (response 3, code 300) charged nothing
//     (ErrDuplicateTransaction, 409 payment_duplicate_refused), except for a
//     renewal or recovery, where it may be this period's charge: it stays
//     unknown (life.submission.unresolved) and is never re-sent.
//   - Sales send partial_payments=payment_in_full; an answer whose
//     amount_authorized differs, or with a partial_payment_balance, is not a
//     payment and stays unknown for the operator.
//
// Exactly once: every attempt to pay one period shares one order id, read
// before any send. An engine renewal with no receipt is re-sent only after a
// five-minute settle when both the order read and a vault-wide read are
// empty, with dup_seconds covering the gap; the executor re-reads its claim
// just before each charge. Keep NMI's duplicate check on: it is the only
// fence against a stalled replica. An empty search never proves non-execution.
//
// Server card entry (settings.card_entry: server) vaults the card with
// customer_vault=add_customer and keeps only brand, last four and expiry; it
// puts the receiving servers in SAQ D scope and is refused on other rails and
// on custodian-held cards. Webhooks verify HMAC-SHA256 over "<t>.<body>" from
// Webhook-Signature with a 5-minute window; subscription events only wake
// convergence, which re-reads NMI. An unknown live schedule is adopted
// through the billing import or parked (price_unmapped, customer_unlinked,
// product_held, import_owned). Unverified schedules are read in batches of 50,
// or in bulk above 200. nmi_history_months holds a daily read of 25 months of
// transaction reports, the source of `openrails nmi decline-report`.
//
// Tests use openrailstest/nmimock. Its package comment has the wiring.
package nmi
