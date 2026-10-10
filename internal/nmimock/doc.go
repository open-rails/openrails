// Package nmimock is a stateful, deterministic NMI gateway for tests and
// disposable sandbox stacks. It serves the surface OpenRails uses: v5
// customers, payments, plans and subscriptions; Direct Post
// /api/transact.php; and the Query API /api/query.php.
//
// Serve it over loopback (New; point provider_sandbox.nmi_gateway_url at URL)
// or in process (NewUnstarted, as an http.RoundTripper or http.Handler). Time
// comes only from Options.Clock. Only tests and sandbox commands may import it
// (guard_test.go).
//
// # Known differences from real NMI
//
//   - Cards: a Tokenize token names its card; any other token ending in four
//     digits is a visa with those last four, and DeclineLast4 declines sales
//     with 202. Tokens stay valid after use, except when adding a billing
//     entry. A card's AVS and CVV letters ride every answer for it.
//   - Declines: responsetext is always "DECLINE". A card declined "vault" is
//     refused when stored; type=validate approves funds declines 202 and 203.
//     A card stored by number must pass Luhn and carry an MMYY expiry; its
//     brand comes from its first digits and its cvv is not checked.
//   - Duplicate checks: Options.DuplicateWindow matches brand, last four and
//     amount gateway-wide; dup_seconds the same within one vault.
//   - Query API filters: order_id, transaction_id, customer_vault_id,
//     subscription_id (comma list), action_type, start_date, end_date,
//     result_limit, page_number; others are ignored. cc_bin defaults by brand;
//     processor_response_code is "00" on approval, "51" behind 202 and "05"
//     behind other declines. The recurring report carries subscription id,
//     order, next charge date and plan id only. A refund is its own
//     transaction.
//   - Indexing lag: a sale is invisible to the Query API until Options.IndexLag
//     passes on the mock clock (or HideSales/Reveal); v5 reads see it at once.
//   - v5 JSON carries only the fields OpenRails decodes. Cursors are decimal
//     offsets; errors use the {"type","error_code","message"} envelope with
//     400 or 404.
//   - Recurring engine: schedules bill only on RenewSchedule or RunDue, dated
//     the schedule's next billing time (so a forced early charge can postdate
//     the clock); a failed charge advances without retrying.
//     rebill_subscription charges the schedule amount without moving it.
//   - No webhooks, settlement, batching or chargebacks. Currency is whatever
//     the request names (USD for schedule charges). Authentication is not
//     checked.
package nmimock
