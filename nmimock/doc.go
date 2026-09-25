// Package nmimock is a stateful, deterministic NMI gateway for tests and
// disposable sandbox stacks. It serves the NMI surface OpenRails uses:
//
//   - Customer Vault v5 (/api/v5/customers, billing entries),
//   - v5 payments (read, refund, void, auth probe), plans and subscriptions,
//   - Direct Post /api/transact.php (sale, validate, refund, void,
//     recurring add_subscription/update_subscription/rebill_subscription),
//   - Query API /api/query.php (transaction, recurring, test_mode_status).
//
// Serve it over loopback (New, then point provider_sandbox.nmi_gateway_url at
// URL, whose root also takes all three APIs) or in process (NewUnstarted, then
// use the Mock as an http.RoundTripper or http.Handler). Time comes only from
// Options.Clock.
//
// Tests seed state (Tokenize, AddVault, AddPlan, AddSchedule, AddSale),
// inspect what the gateway saw (Sales, Ledger, Attempts, Calls, Validations)
// and inject failures (SetDecline, DeclineValidations, LoseSales,
// DropSaleResponses, RefuseDuplicates, QueryUnavailable, FailRequests, Hold,
// Intercept).
// RenewSchedule and RunDue play NMI's recurring engine.
//
// Only tests and sandbox commands may import it; guard_test.go enforces this.
//
// # Known differences from real NMI
//
//   - Cards: a Collect.js token made by Tokenize names its card. Any other
//     token ending in four digits is a visa with those last four; last four
//     DeclineLast4 declines sales with 202. Tokens stay valid after use,
//     except when adding a billing entry.
//   - Declines: responsetext is always "DECLINE" (real text varies by
//     processor). A card declined "vault" is refused when stored. Card
//     verification (type=validate) approves funds declines 202 and 203.
//   - Duplicate checks: the gateway-wide window (Options.DuplicateWindow)
//     matches card brand, last four and amount; dup_seconds matches the same
//     within one vault. Real NMI also weighs other fields.
//   - Query API: filters order_id, transaction_id, customer_vault_id,
//     subscription_id (comma list), action_type, start_date, end_date,
//     result_limit and page_number. Other filters are ignored. Transactions
//     carry the fields OpenRails reads plus condition, cc_number and
//     response_text; the recurring report returns subscription id, order,
//     next charge date and plan id only. A refund is its own transaction.
//   - Indexing lag: a sale is invisible to the Query API until
//     Options.IndexLag has passed on the mock clock (or HideSales/Reveal).
//     v5 payment reads see it at once.
//   - v5 JSON: responses carry the fields OpenRails decodes; others are
//     omitted. Cursors are decimal offsets. Error bodies use the
//     {"type","error_code","message"} envelope with 400 or 404.
//   - Recurring engine: schedules bill only when a test calls RenewSchedule or
//     RunDue; a charge is dated the schedule's next billing time (a test may
//     force it early, so it can postdate the clock), and a failed charge
//     advances to the next date without retrying.
//     rebill_subscription charges the schedule amount without moving it.
//   - Webhooks are never sent; tests deliver notices themselves.
//   - Currency is whatever the request names (USD for schedule charges);
//     there is no settlement, batching or chargeback.
//   - Authentication is not checked: any security_key or Authorization works.
package nmimock
