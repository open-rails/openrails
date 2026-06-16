<!-- openrails issue tracker — ACTIVE issues -->

> One `# #<id>: <name>` section per issue, separated by `---` lines; section anchor for
> tooling is a line starting with `# #`. IDs are stable for an issue's whole lifecycle and
> share ONE per-repo id space across progress.md / future.md / completed.md; new issues take `next_id` below and bump it.
> CONCURRENT EDITS: only ever edit/append your own issue's section with targeted string
> replacement — never rewrite the whole file.


next_id: 510

---

# #508: Processor-backed invoice collection

**Status:** COMPLETE 2026-06-16: OpenRails can collect finalized/open invoices through the supported saved-payment
rails: Stripe test-mode invoices and NMI/Mobius stored-vault sales. The collection router validates
merchant/customer/payment-method scope, rejects locally failed payment methods, excludes CCBill/Solana from arbitrary
invoice collection, records settled/failed `invoice_payments`, applies settled payments to invoices and the owed ledger,
persists Stripe `in_...` ids, and is wired into `ArrearsChargeWorker`.

The remaining provider-platform items that were previously listed here were pruned as over-broad for this issue:
async Stripe webhook repair, Connect routing, manual/admin retry UI, out-of-band payment recording, support reporting,
and generalized provider capability matrices belong in future operational/provider-hardening issues if needed.

## Tasks
- [x] Inventory current invoice collection tables and saved-method data:
      `invoices`, `invoice_items`, `invoice_payments`, `money_transactions`, `money_settings`,
      `payment_methods`, and `processor_customers`.
- [x] Implement `money.ScopedCharger` to validate merchant/customer/payment-method scope before provider dispatch.
- [x] Reject locally failed/stale payment methods before provider dispatch.
- [x] Exclude CCBill and Solana from arbitrary invoice collection.
- [x] Route NMI/Mobius invoice collection through stored customer-vault sales.
- [x] Route Stripe invoice collection through Stripe invoice item/create/finalize/pay against a saved `pm_...`.
- [x] Persist Stripe external invoice ids on OpenRails invoices.
- [x] Record successful invoice collection as settled `invoice_payments` plus invoice-linked `owed_payment` ledger rows.
- [x] Record hard declines as failed `invoice_payments` without reducing the receivable.
- [x] Keep transient adapter errors retryable without creating local settled payments.
- [x] Wire `ArrearsChargeWorker` to the configured runtime collection router.
- [x] Add guarded live processor tests for Stripe test mode and NMI/Mobius sandbox, requiring
      `TEST_ENV=true OPENRAILS_LIVE_PROCESSOR_TESTS=1`.

## Acceptance
- [x] Open invoices with a valid saved Stripe payment method can be collected and marked paid.
- [x] Open invoices with a valid saved NMI/Mobius vault payment method can be collected and marked paid.
- [x] A repeated collection job cannot double-apply a paid invoice.
- [x] A hard decline leaves the invoice receivable intact and records failure metadata.
- [x] CCBill/Solana cannot be selected for arbitrary invoice collection.
- [x] A merchant/customer cannot collect with another merchant/customer's saved payment method.
- [x] A locally failed payment method cannot be used for invoice collection.

## Validation
- [x] `task sqlc`
- [x] Fake-adapter success/failure/retry integration tests.
- [x] Stripe fake-server success/failure integration tests.
- [x] NMI fake-provider success/failure integration tests.
- [x] Guarded Stripe test-account integration:
      `TEST_ENV=true OPENRAILS_LIVE_PROCESSOR_TESTS=1 go test -tags integration ./internal/modules/money -run 'TestLive(Stripe|NMI)InvoiceCollection' -count=1 -v`
- [x] Guarded NMI/Mobius sandbox integration:
      `TEST_ENV=true OPENRAILS_LIVE_PROCESSOR_TESTS=1 go test -tags integration ./internal/modules/money -run 'TestLive(Stripe|NMI)InvoiceCollection' -count=1 -v`
- [x] Full money integration suite: `go test -tags integration ./internal/modules/money -count=1`
- [x] `go test ./...`
- [x] `task build`

---

# #507: Remove legacy non-money gates from service admit

OpenRails still carries a legacy hot-path throughput layer from the earlier Tensorhub design:

- `tenant_throughput` lets the host send rate-limit policy in every admit request.
- `amounts` drives request/token/image/unit windows in OpenRails.
- tier policy `throughput` / `release_windows` and queue-unit reservations enforce RPM/TPM/IPM/RPD-style limits.
- `block_checks` runs an unrelated blocklist gate before the real prepaid/money admission logic.
- tier-policy `entitled_resources` / resource allowlist denies invokes for reasons that belong in Tensorhub's own
  endpoint authorization layer, not OpenRails' billing admission.
- `max_single_charge_amount` / per-job cost cap is an arbitrary service-admit denial that duplicates the real
  affordability check and should not be part of the generic billing gate.

That model is wrong for the current billing architecture. The payer/customer is the subject being governed, and
OpenRails' hot admit path should answer a narrower money question: can this customer afford this estimated request after
existing holds and delegated-spend reservations? Admit-time requests should not carry rate-limit policy, and OpenRails
should not enforce old request/token/image throughput limits or a separate max-in-flight-dollar cap for Tensorhub-style
admission. Prepaid balance + computed credit capacity + active holds already bound spend; Tensorhub can still use
in-flight-dollar policy as a scheduler fairness/reordering signal outside OpenRails' admit gate.

## Target Model
- Trust-tier policy is configured out of band by the merchant/operator and stored in OpenRails.
- Admit requests report only stable identity and billing facts: `customer_id`, `invoker`, `invoker_type`, `resource`,
  `currency`, `estimated_amount`, `request_id`, `source`, and expiry.
- OpenRails no longer accepts host-supplied throughput policy on the hot path.
- OpenRails no longer uses `amounts` to enforce request/token/image throughput windows.
- OpenRails no longer enforces tier-policy `throughput`, per-release throughput overrides, or queue-unit reservations.
- OpenRails no longer enforces `max_concurrent_held_amount` / Tensorhub `max_inflight_micros` as a hard admit gate.
  If Tensorhub keeps a max-in-flight-dollar policy, it is a scheduler fairness/reordering signal, not a second
  affordability check in OpenRails.
- Other money controls remain admission/accounting controls, not payer throughput limits: prepaid balance, Redis holds,
  optional arrears credit capacity, and wasted-spend abuse windows for refunded/costly failures.
- Name the money gate as `start_capacity` everywhere practical:
  `prepaid_available + remaining_arrears_credit - active_holds`. Admit only when the estimated charge fits that
  capacity.
- Arrears exposure should follow the redesigned invoice model. The final gate should derive owed exposure from pending
  unbilled invoice items plus open/past-due invoice receivables, not from a live account-level
  `outstanding_owed_amount` counter. Any use of that counter is transition-only and should disappear with #506.
- The DB capacity snapshot and Redis hold placement must remain race-safe. Redis protects admit-vs-admit races by
  atomically summing active holds, but tests should prove that balance/credit changes cannot slip between the DB
  snapshot and Redis hold placement and admit work beyond real capacity.
- The blocklist gate is also wrong here. Tensorhub/OpenRails prepaid job admission is not a payment-instrument fraud
  flow, and there is no current requirement to block arbitrary identifiers before an invoke. Remove the layer from
  service admit rather than preserving or relocating it in this path.
- Gate #6 is two concerns mixed together. Keep tier resolution only where service admit still needs the payer's resolved
  trust tier to load retained money policy such as wasted-spend windows. Remove the `entitled_resources` / resource
  allowlist denial from service admit. Endpoint/resource authorization belongs to the host application before billing
  admit.
- Current tier resolution also loads `max_single_charge_amount`, but Gate #7 removes that arbitrary per-job cost cap
  from the final service-admit policy surface.
- The per-job cost cap should be removed from service admit. If a request is too expensive, prepaid/arrears
  `start_capacity` already denies it; if the host wants endpoint-specific pricing or product policy, that belongs in
  Tensorhub before billing admit.
- Gate #8 should stay as a wasted-spend abuse backstop, not a normal throughput limit. It has two distinct policies:
  payer/customer wasted spend gets a grace budget and is charged through report-time accounting beyond that point;
  delegated-invoker wasted spend is an admit-time hard cutoff when the invoker is already over budget.
- Remove the manual payer/customer suspension gate from service admit. `suspended_at` is a vague manual policy override
  that does not belong in the hot billing admission path. A payer should be denied because money capacity, delegated-spend
  policy, wasted-spend cutoff, or in-flight exposure says no, not because an operator toggled a generic frozen state.
- Move arrears/payment-method eligibility out of the hot admit path. Admit should consume a precomputed credit capacity:
  "how much can this customer borrow right now?" That separate credit-policy path can consider trust tier, invoice
  receivables, functioning payment method, collection status, and risk. The hot path should then compare
  `prepaid_available + remaining_credit_capacity - active_holds` against the estimate, the same shape as prepaid.
- Tensorhub's current target is credit capacity `0`, so Tensorhub-style admit is effectively prepaid-only plus active
  holds.
- After this cleanup, the intended OpenRails service-admit policy gates are only:
  1. Payer/customer affordability and hold placement: compare `estimated_amount` against `start_capacity` after local FX
     conversion where needed, then place the idempotent request hold if allowed.
  2. Customer-funded invoker spend authorization and budget: when `invoker != customer`, OpenRails must find an explicit
     payer-configured delegated-spend grant for that invoker, role, invoker tier, or other configured scope, then reserve
     against its money-denominated spend windows. This applies equally to Tensorhub-native users, such as org members
     spending an org/customer balance, and to remote-application delegated users, such as Cozy-Art JWKS users spending
     Cozy-Art's customer balance. No matching delegated-spend grant means deny; a missing policy is not unlimited spend.
  3. Delegated-invoker wasted-spend cutoff: when `invoker != customer`, deny if the invoker is already past the
     platform-imposed wasted-spend budget for the relevant window. Direct customer credentials are not hard-denied here;
     customer wasted spend is handled by report-time accounting/charging beyond the grace budget.
  Merchant/service authentication, request idempotency, Redis hold atomicity, and FX conversion are required mechanics,
  but they are not additional Tensorhub user-governance gates.
- Gate #9 and Gate #10 are the old request/token/image throughput idea and should be removed: no per-payer/per-resource
  unit windows, no Tensorhub `amounts` enforcement, and no queue/batch unit reservations in OpenRails admission.
- The retained delegated-spend gate is the generalized "customer lets invoker spend his money" policy surface:
  money-denominated authorization + budget windows/reservations over estimated spend, not RPM/TPM/IPM/image counters. It
  must work for Cozy-Art-style org + remote-application/JWKS users spending the Cozy-Art customer balance, for Tensorhub
  native org members whose role allows them to spend an org/customer's money, and for Tensorhub-native delegated users.
  The payer/customer principal decides the spend policy: windows such as max spend per 5 hours, 7 days, month, or any
  overlapping configured period. Tensorhub owns credential validation and endpoint/resource authorization; OpenRails owns
  the billing authorization question "may this invoker spend this customer's money?" Current `buildBudgetScopes` behavior
  is per-invoker only and does not fail closed when no matching delegated-spend policy exists; fix that instead of treating
  missing policy as unlimited spend. If role, invoker-tier, group, org-membership, remote-application, or other
  customer-funded scopes are supported, they must be explicit delegated-spend policy scopes rather than stale implicit
  subject/role pools. These reservations should follow the same idempotency and release/capture lifecycle as the request
  hold so denied, failed, retried, and settled work cannot leak allocated-spend capacity.
- Cross-repo audit finding: Tensorhub currently has multiple overlapping surfaces for this idea. It defines delegated-user
  tier budget windows, role budgets, and tenant self-caps in `platformpolicy.Document`; syncs tier windows to OpenRails
  `TierPolicy.BudgetWindows`; syncs role/self caps to OpenRails budget policies; sends delegated roles on admit; and also
  exposes a Tensorhub-side `/platforms/me/generation-budget/check` preflight that passes caller-supplied windows into
  OpenRails `BudgetCheck`. OpenRails has scope primitives for `invoker`, `role`, and `subject`, but current
  `buildBudgetScopes` applies only per-invoker scopes. The implementation pass must collapse this into one intentional
  customer-funded invoker-spend model rather than preserving all of these partially-overlapping paths.
- Remove the payer-level max-in-flight-dollar gate from OpenRails admit. Affordability is already covered by
  `start_capacity`; Tensorhub may keep in-flight-dollar fairness in its scheduler to queue or reorder work, but
  OpenRails should not deny solely because a customer is over a fairness cap.
- Capture/usage reporting can still record operational metadata, but it must not reintroduce an admission-time
  rate-limit policy surface.

## Tasks
- [ ] Inventory the legacy throughput admission surface:
      `AdmitRequest.TenantThroughput`, `AdmitInput.TenantThroughput`, `Amounts`, `admission.ResolvedPolicy.Throughput`,
      `ReleaseThroughput`, `QueueLimits`, `ThroughputForRelease`, Redis limiter calls in `Admitter.Admit`, SDK structs,
      embed/remote clients, HTTP handlers, tests, docs, and generated examples.
- [ ] Inventory the blocklist admit surface:
      `block_checks`, `AdmitBlockCheck`, `admission.BlockCheck`, `abuse.BlocklistService` dependency construction in
      `pkg/service.Admit`, `BlockedBy="blocked"`, service admit/batch wire shape, SDK structs, docs, and tests.
- [ ] Remove `tenant_throughput` from service admit and admit-batch request/response/client contracts.
- [ ] Remove the gate #3 implementation from `pkg/service.Admit`: the pre-admitter
      `TenantThroughput` Redis check must disappear rather than be renamed or moved.
- [ ] Remove admit-time `amounts` consumption from OpenRails policy enforcement. If a field remains for telemetry, rename
      and route it only to durable usage metadata/capture, not to admission gates.
- [ ] Delete per-(merchant, payer, resource) unit-throughput checks from `Admitter.Admit`.
- [ ] Delete queue-unit reservation acquire/release from admission settlement paths.
- [ ] Remove tier-policy `throughput`, `release_windows`, and `queue_limits` from the enforceable policy model, schema JSON
      parsing/writing, SDK types, docs, and tests unless another non-Tensorhub host has a live use case.
- [ ] Remove `block_checks` / blocklist evaluation from the service admit contract. Do not keep a blocklist dependency in
      this path; any future fraud control must be introduced separately with an explicit current requirement.
- [ ] Remove `entitled_resources` / `BlockedBy="resource"` evaluation from service admit. Preserve `resource` only as a
      stable reporting/provenance key unless another current billing requirement justifies it.
- [ ] Keep tier resolution only for retained money policy that truly belongs in OpenRails admit, such as payer
      wasted-spend windows. It must not imply endpoint authorization or max-in-flight-dollar admission denial.
- [ ] Remove `max_single_charge_amount` / per-job cost cap from service admit, tier policy SDK/wire structs, docs, and
      tests unless another current non-Tensorhub billing requirement justifies it.
- [ ] Keep the wasted-spend abuse gate, but make the contract explicit: payer/customer wasted spend has a grace budget
      and is charged through report-time accounting beyond that point; delegated invokers are hard-cut from admit when
      they are already over their wasted-spend budget.
- [ ] Keep and verify delegated-spend budget windows as money-denominated reservations over `estimated_amount`. They must
      not depend on Tensorhub `amounts`, request/token/image units, or endpoint resources.
- [ ] Make delegated-spend fail closed: if `invoker != customer`, OpenRails must require at least one matching
      payer-configured delegated-spend grant/scope before placing a money hold. A missing matching policy denies rather
      than allowing unlimited delegated spend.
- [ ] Define the generalized customer-funded invoker spend model for "customer lets invoker spend his money": direct
      invoker, delegated user, verified org role, invoker tier, or another explicit configured scope. Keep payer/customer
      trust tier separate from invoker/delegated-user spend tier.
- [ ] Ensure the delegated-spend model is source-agnostic: Tensorhub-native org users and remote-application delegated
      users are both just invokers spending a payer/customer principal's money under that principal's configured
      overlapping money windows.
- [ ] Decide the canonical OpenRails storage surface for delegated-spend policy. Prefer explicit budget-scope policies
      (`scope=invoker`, `scope=role`, and optionally a named invoker-tier/group scope if needed) over hiding
      customer-funded invoker-spend windows inside payer trust-tier `TierPolicy.BudgetWindows`.
- [ ] Reconcile Tensorhub policy sync with OpenRails enforcement: either make synced `RoleBudgets` / tenant self-caps
      actually participate in `Admitter.buildBudgetScopes`, or remove the sync/API fields if they are not part of the
      final customer-funded invoker-spend model.
- [ ] Revisit Tensorhub `/platforms/me/generation-budget/check`: it should be a read/preflight view over the same stored
      OpenRails delegated-spend policy used by admit, not a second path where Tensorhub passes ad hoc budget windows for
      OpenRails to evaluate.
- [ ] Verify delegated-spend reservations use request-level idempotency and are released or settled alongside the money
      hold lifecycle, including admit denial rollback, post-admit failure release, retry, and capture.
- [ ] Clean stale budget comments/fields that describe subject/role pools as accidental behavior. Role or invoker-tier
      support is allowed only if it is intentionally modeled as delegated-spend policy.
- [ ] Remove `max_concurrent_held_amount` / Tensorhub `max_inflight_micros` as an OpenRails hard admit gate, including
      deny codes, response fields that exist only for hard-denial scheduling, tier policy SDK/wire fields, docs, and
      tests unless another current non-Tensorhub billing requirement justifies them.
- [ ] If Tensorhub keeps max-in-flight-dollar fairness, keep it in Tensorhub's scheduler/configuration surface and make it
      explicitly soft/work-conserving: it reorders or queues requests, but affordability is still decided by OpenRails
      `start_capacity`.
- [ ] Keep and verify FX conversion for retained money policies: if the admit estimate is in EUR and a delegated-spend or
      wasted-spend policy is in USD (or any other configured policy currency), convert locally before evaluating it. All
      values remain integer micros in their respective currencies.
- [ ] Remove manual payer/customer suspension from service admit and delete or quarantine the `suspended_at` /
      `suspend_reason` account-freeze surface if no other current product requirement uses it.
- [ ] Move arrears/payment-method eligibility out of service admit. The hot path should consume already-computed
      `remaining_credit_capacity` and should not perform payment-method setup checks itself.
- [ ] For Tensorhub configuration, set credit capacity to `0` until a real arrears product is deliberately enabled.
- [ ] Keep and verify non-throughput admit/accounting gates: prepaid/credit capacity, Redis hold placement, and
      wasted-spend gates.
- [ ] Rename/refactor the solvency gate around the explicit `start_capacity` model:
      `prepaid_available + remaining_arrears_credit - active_holds`, with denial when `estimated_amount` exceeds it.
- [ ] Align arrears capacity with the invoice-receivables redesign (#506): derive exposure from pending invoice items and
      open/past-due invoice balances; remove or clearly quarantine any remaining `outstanding_owed_amount` dependency as
      transition-only.
- [ ] Add regression coverage for the DB-snapshot/Redis-hold race boundary: concurrent admits, captures/releases,
      deposits/withdrawals/collections, and credit-line changes must not allow active holds to exceed current start
      capacity.
- [ ] Keep the Tensorhub prepaid admit gate narrow: no daily/monthly spend caps, blocklists, token/request rates,
      arbitrary per-job caps, max-in-flight-dollar hard denials, or other user-governance policy in this path. The normal
      payer controls are prepaid reserve, computed credit capacity, active holds, and costly-failure backstops.
- [ ] Update Tensorhub integration guidance: Tensorhub must stop sending `tenant_throughput` and `amounts` for admission.
      Any `max_inflight_micros` style policy belongs in Tensorhub scheduler fairness, not OpenRails service admit.
- [ ] Remove or update stale comments that describe OpenRails as enforcing Tensorhub RPM/RPD/TPM/IPM on admit.
- [ ] Slim the admit response contract after the removals: do not return resolved scheduling policy such as
      `max_concurrent_held_amount` or per-job charge caps once they no longer drive OpenRails denial. Keep only the
      admit decision, deny reason, hold/capacity facts, policy currency/amount, and retained delegated-spend or
      wasted-spend window status that a caller can legitimately display or act on.
- [ ] Coordinate the Tensorhub sibling cleanup: Tensorhub must remove stale #486 assumptions about OpenRails-owned
      `max_inflight_micros`, per-job caps, request/token/image `amounts`, host-supplied `tenant_throughput`, and ad hoc
      generation-budget checks. Tensorhub should retain endpoint authorization and scheduler fairness locally, while
      OpenRails owns affordability, delegated-spend authorization/budget, and delegated wasted-spend cutoff.

## Acceptance
- `POST /v1/service/admit` and `/v1/service/admit/batch` have no host-supplied throughput-policy field.
- OpenRails admit decisions cannot be affected by request/token/image `amounts`.
- No Redis throughput or queue-unit reservation is created during admit; Redis is used for money holds and any remaining
  explicitly money-denominated windows only.
- OpenRails service admit has no hard max-in-flight-dollar denial; in-flight-dollar fairness, if retained, is a
  Tensorhub scheduler concern.
- Cross-currency admits compare policy-currency micros after local FX conversion for retained delegated-spend and
  wasted-spend policies; no retained money policy silently assumes USD-only request amounts.
- Payer/customer wasted spend is charged beyond its grace budget through report-time accounting, not by an admit denial.
- Delegated-invoker wasted spend remains an admit-time hard cutoff when the invoker is already over budget.
- Tensorhub-style admit has no manual customer suspension gate and no prepaid payment-setup gate.
- Arrears/payment-method eligibility is not checked inline by service admit; admit consumes a computed credit-capacity
  value. Tensorhub sets that capacity to `0` for now.
- Delegated-spend budget windows are retained only as money-denominated estimated-spend reservations.
- Delegated-spend authorization fails closed: `invoker != customer` cannot spend customer money unless a matching
  payer-configured delegated-spend grant/scope exists.
- Delegated-spend policy supports explicit customer-funded invoker scopes such as direct invoker, delegated user,
  verified org role, org membership, remote-application user, or invoker tier without confusing those invoker scopes with
  the payer/customer trust tier.
- Tensorhub and OpenRails have one delegated-spend contract: Tensorhub authors/syncs policy, OpenRails stores/enforces
  reservations and actuals, and preflight reads the same policy admit will enforce.
- Delegated-spend reservations are idempotent and cannot leak capacity across denied, failed, retried, or
  captured requests.
- The OpenRails admit response no longer exposes removed scheduling/capacity policy fields whose only purpose was
  Tensorhub's old hard or soft max-in-flight-dollar design.
- Tensorhub-style admit does not accept or evaluate blocklist identifiers.
- Tensorhub-style admit does not deny on resource allowlists; endpoint/resource authorization is host-owned and happens
  before billing admit.
- Tensorhub-style admit does not deny on an arbitrary per-job cost cap; affordability is handled by `start_capacity`.
- Tier resolution still happens before money policy evaluation and is not treated as resource authorization.
- The solvency gate is documented and implemented as `start_capacity`; prepaid and arrears paths share the same final
  admit predicate.
- Arrears exposure is invoice-derived after #506, with no live account-level owed counter required for correctness.
- Tests cover concurrent DB balance/credit changes around Redis hold placement and prove no stale capacity admit.
- Payer trust-tier policy is represented as stored OpenRails configuration, not request payload.
- Tensorhub-style admission remains protected by money-denominated controls and the hold lifecycle.
- Docs clearly state that host applications configure payer/customer policy ahead of time and report billable estimates at
  admit time.

## Validation
- `task sqlc`
- `go test ./internal/modules/admission ./internal/modules/ratelimit ./pkg/service ./internal/http/handlers`
- `go test ./...`
- `task build`

---

# #506: Redesign arrears billing around invoice receivables

OpenRails currently models arrears by writing `owed_accrual` money transactions as usage is spent and by incrementing
`money_settings.outstanding_owed_amount`. Invoices are period statements that summarize usage and money movements after
the fact. That is not how Stripe/OpenMeter-style arrears invoicing works: billable items accumulate, an invoice is
created in draft, finalization turns it into an open receivable, and payments are applied to that invoice.

The target model:

- OpenMeter inspiration: gathering/pending invoice lines provide visibility into accruing usage before the final invoice.
- Stripe inspiration: invoices move through `draft -> open -> paid` or `void/uncollectible`; an open invoice is the
  receivable, and payments/attempts belong to that invoice.
- OpenRails should keep the implementation simpler than either system, but copy the core accounting boundaries.

References:
- OpenMeter gathering invoices: https://openmeter.io/docs/billing/invoicing/gathering-invoices
- OpenMeter invoice structure: https://openmeter.io/docs/billing/invoicing/invoices
- Stripe invoice lifecycle: https://docs.stripe.com/invoicing/overview
- Stripe subscription invoices: https://docs.stripe.com/billing/invoices/subscription

## Target Model
- `usage_events` remains the durable usage/audit source.
- pending billable charges are represented as first-class invoice items/lines before invoicing. These rows have
  `invoice_id NULL` while pending, similar to Stripe pending invoice items and OpenMeter gathering lines.
- invoice creation collects pending billable items for a period into a `draft` invoice.
- invoice finalization snapshots line items, totals, customer/billing context, invoice number, and due dates, then moves
  the invoice to `open`.
- an `open` invoice is the receivable. It has `total_amount`, `amount_paid`, `amount_due`, `currency`, `issued_at`,
  `due_at`, and collection metadata.
- payment attempts target a specific invoice. Successful settled payments create invoice-linked payment rows and reduce
  that invoice's `amount_due`.
- invoice state, not an account-level live owed counter, answers whether the customer paid the bill.
- current customer debt is derived from open/past-due invoices, optionally through a rebuildable projection.
- unbilled usage is never called "owed"; "owed" means an issued/open invoice receivable.
- arrears credit is a customer policy: for example, a customer may accrue up to $200 of unbilled/open arrears before
  OpenRails must generate/collect an invoice or deny further arrears spend.
- invoice generation is triggered by either period close, manual/admin action, or a credit-line threshold/cap hit.
- failed collection must feed back into admission/dunning so a customer cannot continue building a large in-arrears
  balance after payment cannot be collected.
- the motivating hot path is Tensorhub-style admission: before accepting a work request, OpenRails must decide whether
  the payer/customer/org has enough prepaid balance or remaining arrears capacity, then place a Redis hold for the
  estimated charge.

## Schema Tasks
- [x] Inventory current arrears paths:
      `AccrueOwed`, `ChargeOutstanding`, `SpendCredits`, capture/settle paths, invoice finalization, dunning, tests, and
      SQL queries that read or write `money_settings.outstanding_owed_amount`.
- [x] Add a first-class `invoice_items` or `invoice_lines` table:
      `id`, `merchant_id`, `customer_id`, `currency`, nullable `invoice_id`, `source_type`, `source_id`, `event_type`,
      `period_from`, `period_to`, `invoice_at`, `quantity`, `unit_amount`, `amount`, `status`, `metadata`,
      `created_at`, `updated_at`.
- [x] Add idempotency constraints so one usage/charge fact cannot create duplicate pending invoice items.
- [x] Update `openrails.invoices` to become the receivable table:
      `invoice_number`, `subtotal_amount`, `total_amount`, `amount_paid`, `amount_due`, `status`, `collection_method`,
      `issued_at`, `due_at`, `paid_at`, `voided_at`, `uncollectible_at`, `sent_at`, `finalized_at`, `external_invoice_id`.
- [x] Fix the invoice uniqueness key to include `currency` if OpenRails continues to create one invoice per
      `(merchant, customer, period, currency)`.
- [x] Add invoice payment/attempt storage, either as a new `invoice_payments` table or invoice-linked
      `money_transactions`:
      `invoice_id`, `amount`, `currency`, `status`, `processor`, `processor_payment_id`, `attempted_at`,
      `settled_at`, `failure_code`, `failure_message`.
- [x] Decide whether credit notes/voids are first-class now or represented initially as negative invoice items on a
      replacement invoice. Explicit `voided` and `uncollectible` invoice states are implemented first; credit notes are
      deferred until a real adjustment/refund workflow requires them.
- [ ] Remove `money_settings.outstanding_owed_amount` or convert it into a clearly documented rebuildable projection
      such as `open_invoice_balance_amount`.
- [x] Keep or replace `credit_limit_amount` as the arrears credit-line policy:
      it caps pending unbilled invoice items plus open/past-due invoice balances for a customer/currency.
- [ ] Add fields needed for collection/admission policy, such as `collection_failed_at`, `last_payment_attempt_at`,
      `next_payment_attempt_at`, `arrears_blocked_at`, or equivalent invoice/account state if the current tables already
      have a better home.

## Workflow Tasks
- [ ] Change usage/capture settlement so arrears usage creates pending invoice items, not immediate receivables.
      Pending invoice item creation is implemented for `AccrueOwed`, usage spends that spill into owed, and hold captures
      that spill into owed. The legacy `outstanding_owed_amount` counter still exists as a transition projection, so the
      "not immediate receivables" half remains open.
- [x] Define the Tensorhub/OpenRails admission decision explicitly:
      - if arrears credit line is `$250` and pending/open owed exposure is already `>= $250`, reject new work until
        payment collection succeeds or an admin raises/unblocks the line.
      - if arrears credit line is `$0`, prepaid balance is `$10`, and estimated charge is `$0.50`, admit the request and
        place a `$0.50` Redis hold against prepaid balance.
      - if prepaid balance is insufficient but arrears capacity remains, admit only up to
        `credit_limit_amount - pending_unbilled_arrears - open_invoice_amount_due - active_holds`.
      Current code enforces the decision against the existing hold mechanism; the Redis hold migration remains #505.
- [ ] Add invoice draft creation for a period:
      collect pending invoice items where `invoice_at <= cutoff`, attach them to one draft invoice per
      `(merchant, customer, currency, period)`, and snapshot line details.
- [ ] Add threshold-triggered invoice creation:
      when pending unbilled arrears plus open invoice balance reaches the customer credit line or configured threshold,
      create/finalize an invoice immediately instead of waiting for month end.
- [x] Add invoice finalization:
      validate draft totals, assign invoice number, freeze editable monetary fields, set `status = open`, set `issued_at`
      and `due_at`, and book receivable ledger movement if the ledger needs invoice-issued entries.
- [x] Add invoice collection:
      for `collection_method = charge_automatically`, create a payment attempt for the open invoice and charge the saved
      payment method; for `send_invoice`, leave the invoice open until manual/out-of-band payment is recorded.
- [ ] Replace customer-level `ChargeOutstanding` sweeps with invoice-level collection over open/past-due invoices.
- [x] Apply payments to invoices:
      successful settled payment increments `amount_paid`, decrements `amount_due`, links the payment/transaction to
      `invoice_id`, and marks the invoice `paid` when `amount_due = 0`.
- [x] On failed payment, keep the invoice `open` or transition it to `past_due` based on due date/retry policy; do not
      erase or reduce the receivable.
- [x] On failed automatic collection, tighten admission:
      block or reduce further arrears spend until payment succeeds, an admin intervenes, or the customer adds a usable
      payment method.
- [x] Add `void` and `mark uncollectible` flows for open invoices. Voids should reverse or neutralize the receivable;
      uncollectible should preserve the debt history as bad debt.
- [ ] Rework dunning/admission checks to derive outstanding debt from open/past-due invoices, not from usage-time owed
      accruals.
- [x] Admission should compute arrears capacity as:
      `credit_limit_amount - pending_unbilled_arrears - open_invoice_amount_due - active_holds`, with failed-payment
      and suspension state able to force capacity to zero.
- [ ] Admission should compute total available start capacity as prepaid spendable balance plus remaining arrears
      capacity, minus active Redis holds. Holds must be created before admitting work and released/captured by the
      caller's stable Tensorhub request id as tracked in #505.
- [ ] Keep prepaid balance spending separate: prepaid invoices can remain receipts/statements, but arrears invoices are
      receivables with payment state.

## API and Reporting Tasks
- [x] Expose invoice list/detail with invoice status, totals, due dates, payment state, and line items.
- [ ] Add service/admin endpoints or jobs to create draft invoices, finalize invoices, collect an invoice, void an
      invoice, mark uncollectible, and record out-of-band payment.
- [ ] Add statement/reporting queries that can answer:
      "What did this customer owe for March 2026?", "What was paid against that invoice?", "What is still due?", and
      "What is the customer's current open invoice balance?"
- [ ] Add payment allocation reporting so support/admin can see exactly which payment paid which invoice.
- [ ] Update docs/comments so "invoice" means bill/receivable for arrears, while "statement" means historical
      informational summary where no payment is due.

## Test Tasks
- [ ] Add integration tests for the real arrears timeline:
      March usage accumulates as pending invoice items, April invoice generation creates a March draft/open invoice,
      April payment reduces that invoice, and the March invoice still reports March charges correctly.
- [ ] Add integration tests for automatic collection success, automatic collection failure, manual/out-of-band payment,
      partial payment, past-due transition, void, and uncollectible handling.
      Coverage now exists for automatic collection success, failed collection staying open/blocking arrears,
      manual/out-of-band payment, partial payment, void, and uncollectible. Past-due transition coverage remains open.
- [ ] Add integration tests for a $200 arrears credit line:
      usage can accrue below the line, reaching the line triggers invoice generation/collection, successful collection
      restores arrears capacity, and failed collection blocks further arrears growth.
- [ ] Add admission integration tests for the Tensorhub use case:
      owed exposure over limit rejects; `$0` arrears line with enough prepaid balance admits and creates a Redis hold;
      mixed prepaid-plus-arrears capacity admits only when the estimated charge fits remaining capacity.
      Partial coverage exists for owed exposure and zero-line/prepaid behavior; Redis-specific coverage waits for #505.
- [ ] Add regression tests proving usage-time admission/spend does not create invoice receivables until an invoice is
      finalized/open.
- [ ] Add tests proving current open balance is derived from open/past-due invoices and can be rebuilt from invoice and
      payment rows.
- [ ] Add RLS/cross-merchant tests for invoices, invoice lines, and invoice payments.
      HTTP-level self-service coverage now proves an authenticated delegated subject sees its own invoice payment state
      and another subject cannot read that invoice through `/v1/self/invoices`.
- [x] Run `task sqlc`, targeted money/invoice integration tests, `go test ./...`, and `task build`.

## Acceptance
- A period bill is represented by an invoice and its line-item snapshot, not by filtering usage-time owed payments.
- Pending usage/charges can be inspected before invoicing without being treated as issued debt.
- A customer credit line caps the sum of pending unbilled arrears, open invoice balances, and active admit holds.
- Admission for Tensorhub-style work requests uses prepaid balance first, then remaining arrears capacity, and rejects
  when the payer/customer/org is already over its allowed owed exposure.
- Hitting the configured arrears cap can trigger immediate invoice generation and collection instead of waiting for the
  scheduled month-end run.
- Finalizing a draft invoice creates an open receivable with immutable totals and due dates.
- Payments are allocated to invoices and reduce invoice due state without changing the historical charge total.
- A customer is considered to have paid a bill because the invoice is `paid` or `amount_due = 0`, not because a
  customer-level owed counter happened to drop.
- Failed collection can suspend or cap further arrears spend so unpaid customers cannot keep growing the balance.
- Open current owed balance is derived from open/past-due invoice receivables.
- `money_settings.outstanding_owed_amount` is removed or explicitly downgraded to a rebuildable projection.
- March 2026 statements can show prior balance, new charges, payments/credits, ending balance, and remaining amount due.
- Admission and dunning behavior continue to work without treating unbilled usage as already-issued debt.

## Validation
- `task sqlc`
- `go test -tags integration ./internal/modules/money -run 'TestFinalizeInvoice|TestAuthorizeAndHold_Arrears|TestChargeOutstanding' -count=1`
- `go test -tags integration ./pkg/service -run 'TestListInvoices|TestAuthorizeAndHold|CreditLine|Invoice' -count=1`
- `go test ./migrations/postgres`
- `go test -tags integration ./internal/modules/money -count=1`
- `go test ./internal/modules/money ./pkg/service ./migrations/postgres`
- `go test ./...`
- `task build`
- `go test -tags integration ./internal/modules/money -run 'TestFinalizeInvoice|TestInvoiceCollectionDecline|TestAuthorizeAndHold_Arrears|TestChargeOutstanding|TestCaptureHold_ArrearsSpillsToOwed' -count=1`
- `go test -tags integration ./internal/modules/money -count=1`
- `go test ./internal/modules/money ./pkg/service ./internal/http/handlers -run TestNonExistent -count=0`
- `go test -tags integration ./pkg/service -run 'TestListInvoices|TestAuthorizeAndHold|CreditLine|Invoice|TestCreditFacade_RLS_Under_OpenRailsApp|TestGetUsage_Breakdown' -count=1`
- `go test -tags integration ./internal/modules/money -run 'TestFinalizeInvoice|TestInvoiceCollectionDecline|TestRecordOutOfBandInvoicePayment|TestInvoiceVoidAndUncollectibleLifecycle' -count=1`
- `go test -tags integration ./internal/modules/money -count=1`
- `go test -tags integration ./tests -run TestSelfInvoicesHTTP_ReflectsReceivablePaymentsAndScopesToSubject -count=1`

---

# #505: Move admit-time holds out of the durable money ledger

**Completed:** 2026-06-16

OpenRails hard-cut request lifecycle holds to Redis state keyed by caller `request_id`.
`money_transactions` no longer stores admit/release/expiry hold rows; capture posts only real debit / owed-accrual
transactions and optional usage events. Tensorhub-side naming/resource follow-through is tracked in Tensorhub's own
progress file.

OpenRails currently uses `money_transactions` for both actual money movement and pre-work hold/reservation state.
That makes the ledger harder to reason about: admit-time holds, releases, and expirations are not money events.

The target model is:

- `money_transactions` records only durable money movement: deposits, debits/captures, owed accruals, withdrawals,
  refunds/credits if added later.
- `usage_events` records durable metered usage/reporting events.
- admit-time holds are ephemeral Redis reservations used only to prevent concurrent overspend while work is in flight.
- reservation identity is caller-supplied and stable. OpenRails should not generate a separate reservation id. The
  caller must provide a merchant-scoped `request_id` for the request/hold lifecycle, and capture/release must refer to
  that same request id.
- the admit contract should be explicit about identity: `payer/customer_id` is who pays, `invoker` is who caused the
  request, and `invoker_type` tells OpenRails whether the invoker is a payer-controlled credential or a delegated user.
- `resource` should be a stable policy/reporting key when possible, not a mutable display name. Tensorhub can still put
  endpoint/function display names in usage metadata.
- `source` remains durable provenance/idempotency namespace for ledger/usage rows, but it is not the hold identity.

The admission check should effectively be:

`available_to_start = spendable_balance + allowed_owed_amount - active_redis_holds`

If `available_to_start >= estimated_amount`, OpenRails creates a TTL-bound Redis hold at
`merchant + request_id` and admits the request. The hold value stores payer/customer, invoker, invoker type, currency,
estimated amount, source/provenance, resource, created time, and expiry. Completion captures by `merchant + request_id`,
releases the hold, and records the actual usage/charge in Postgres. Cancel/failure releases by the same request id when
possible, and TTL expiry handles worker crashes.

## Tasks
- [x] Inventory every current hold path: authorize/admit, capture, release, expiry, settlement, tests, SDK/API payloads,
      and generated SQL.
- [x] Hard-cut the hold API contract so `request_id` is a required stable caller-assigned identifier for admit,
      capture, and release; remove OpenRails-generated reservation ids from admit/capture/release semantics.
- [x] Change the admit/capture/release response and SDK types to stop exposing `reservation_id`; return hold metadata
      such as `hold_expires_at` when useful, but require the caller to settle by `request_id`.
- [x] Define Redis hold keys as `openrails:hold:{merchant_id}:{request_id}`.
- [x] Define Redis hold values with payer/customer, invoker identity/type, currency, estimated amount, source,
      resource, created time, expiry, and TTL. For Tensorhub, `request_id` is the Tensorhub request id and
      `source = "invoke"` remains provenance for durable ledger/usage rows.
- [x] Require and validate `invoker_type` on money-bearing admits so OpenRails can distinguish direct payer credentials
      from delegated users without guessing from invoker string shape.
- [x] Audit Tensorhub/OpenRails integration naming and remove remaining request-lifecycle `actor` vocabulary in favor of
      `invoker` / `invoker_type` on the admit path. Tensorhub follow-through is tracked in its own progress issue.
- [x] Decide the stable `resource` identity Tensorhub should send for OpenRails gates and reports. Prefer a stable
      endpoint/release/function id over mutable `tenant/endpointName`; keep human-readable names in usage metadata.
- [x] Keep `source` as provenance for durable rows (`invoke`, `training`, `wasted_spend`, etc.) while ensuring Redis
      hold lookup uses only `merchant_id + request_id`.
- [x] Change admission to subtract active Redis holds from spendable balance plus allowed owed amount before admitting
      new work.
- [x] Create Redis holds during admit/authorize instead of inserting `money_transactions` rows with `transaction_type =
      'hold'`.
- [x] Capture and release Redis holds by `merchant + request_id`, not by a generated reservation handle and not by
      resource/payer/currency key material.
- [x] Release Redis holds on capture, cancel, explicit release, and failed work; rely on TTL as the crash fallback.
- [x] On successful completion, record only the actual durable charge: `usage_events` plus `money_transactions` debit /
      owed-accrual rows as needed.
- [x] Remove hold/release/expiry transaction states from `money_transactions` schema, queries, models, tests, and API
      semantics.
- [x] Keep `money_transactions` focused on posted money movement and update comments/docs to make that invariant clear.
- [x] Decide whether any long-running job class needs longer TTLs or periodic hold refresh; keep this in Redis unless a
      concrete recovery/reconciliation need proves Postgres reservations are required.
- [x] Add concurrency tests proving a payer with low remaining balance cannot start enough simultaneous estimated work
      to overspend available balance.
- [x] Add cancellation/failure tests proving released/expired Redis holds make capacity available again without creating
      ledger rows.
- [x] Add ledger tests proving admit/release/expiry without completed work creates no `money_transactions` rows.
- [x] Run `task sqlc`, targeted admission/money integration tests, `go test ./...`, and `task build`.

## Acceptance
- Admit-time reservations are Redis enforcement state, not durable ledger rows.
- Reservation identity is the caller's stable merchant-scoped `request_id`.
- OpenRails does not mint or require a separate reservation id for Tensorhub-style request lifecycles.
- Admit/capture/release APIs and SDKs settle by `request_id`, not `reservation_id`.
- Money-bearing admits require explicit `invoker_type`, and direct-payer vs delegated policy no longer depends on
  parsing `invoker`.
- Tensorhub sends a stable OpenRails `resource` key for policy/reporting, with mutable names preserved as metadata.
- `money_transactions` contains only real money movement.
- Successful completed work still creates the expected usage event and ledger debit/owed rows.
- Failed/cancelled/pre-work requests do not create money ledger entries.
- Concurrent admits are bounded by `balance + allowed owed - active holds` for the request currency.

---

# #330: nmi-immediate-subscription-checkout-stuck-pending

**Completed:** no
**Status:** IN_PROGRESS 2026-06-08: immediate NMI checkout activation patch is implemented; package tests and focused mock-provider regression pass; configured-account Mobius recurring test has been added and compiles/skips locally because `PROCESSORS_MOBIUS_SECURITY_KEY` is not set. Remaining work: run real Mobius credential test and repair/replay Paul2 after fixed code is deployed.

Fix the NMI/Mobius subscription checkout path where an immediately approved recurring checkout is persisted as pending, leaving host apps such as Doujins stuck on the pending-subscription screen even though NMI accepted the transaction.

## Metadata

- Category: bug
- Status: in_progress
- Passes: false

## Live findings from Doujins/Paul2 on 2026-06-08

- OpenRails received the checkout request and `POST /v1/self/checkout` returned HTTP 200.
- NMI/Mobius accepted the card vault and transaction: payment vault `314825442`, provider transaction `12162933364`, provider subscription `12162933429`.
- Local checkout session `019ea96b-221b-7274-a341-8cdc85cb72d6` was marked `succeeded` with processor `mobius` and amount `2300 usd`.
- Local subscription `019ea96b-223b-75b1-926b-35956d10eba5` stayed `pending`, with no current period start/end timestamps.
- Local payments only had a pending attempt row keyed as `nmi_sub_attempt:sub_cb204b034d2c3ec46be93b0470ff44df`; there was no completed payment row keyed by the real NMI transaction id.
- No billing entitlement rows were created for the user, so Doujins correctly kept showing the pending-subscription state.

## Root cause

`processNMISubscription` called `AddRecurringSubscription` successfully, but `completeNMISubscriptionRegistration` intentionally created a local pending subscription and returned a pending checkout response. The initial NMI response was not used to synchronously activate an immediate subscription, set the current billing period, create a completed payment, or grant entitlements. Webhooks should not be required for the initial happy path when NMI immediately approves the transaction; delayed/future starts can remain pending.

## Desired behavior

For immediate NMI/Mobius subscription approvals, OpenRails should finish the local checkout atomically from the direct provider response: mark the subscription active, set current period timestamps, record the completed payment against the real provider transaction id, grant entitlements, and return a succeeded checkout response. Delayed-start subscriptions and genuinely asynchronous provider states should remain pending and rely on follow-up provider events/reconciliation.

**Tasks:**
- [x] Capture live-stack evidence for the Paul2 failure: NMI accepted the transaction, checkout succeeded, subscription stayed pending, payment stayed as a pending attempt, and no entitlement was granted.
- [x] Identify root cause in the NMI checkout finalization path.
- [x] Patch immediate NMI subscription finalization to activate the subscription, set period dates, create the completed payment, and grant entitlements without waiting for a webhook.
- [x] Preserve pending behavior for delayed/future-start NMI subscriptions.
- [x] Fix stale integration-test helpers that still query billing tables by `user_id` instead of `tenant_subject_id`.
- [x] Add/validate focused integration coverage proving an immediate NMI subscription checkout grants the premium entitlement synchronously.
- [x] Add an actual NMI test-account integration path so the live provider contract is exercised, guarded by Mobius/NMI test credentials.
- [ ] Run the actual Mobius/NMI configured-account recurring-subscription integration with `PROCESSORS_MOBIUS_SECURITY_KEY` set.
- [x] Run focused OpenRails checkout/module tests and NMI regression tests.
- [ ] After deployment/restart, repair or replay the affected live pending subscription row for Paul2 if it still exists.

---

# #328: robinhood-coinbase-usdc-funding-sessions

**Completed:** no
**Status:** PARTIAL 2026-06-08: Implemented Solana-only USDC funding session APIs, persistence, config, Coinbase hosted-session adapter with CDP JWT auth, Coinbase Hook0-signed Onramp webhook/status ingestion, Robinhood launch-template handoff, provider eligibility gates, self-service routes, idempotency, structured insufficient-USDC funding context on checkout errors, backend Solana USDC balance verification, focused tests, and DB-backed self-service API tests for create/get, tenant/user isolation, idempotency, and unsupported provider/network rejection. Retained for future provider integration work; current Doujins UX uses manual Robinhood/Coinbase links plus connected-wallet balance checks instead of OpenRails provider sessions. Remaining: real Robinhood partner adapter/status docs and access.

Plan and implement OpenRails-owned USDC funding sessions for host apps that need users to buy or transfer USDC into their own live Solana wallet before completing a Solana wallet checkout.

## Metadata

- Category: feature
- Status: partial
- Passes: false

## Goal

- OpenRails should expose a provider-backed funding-session API for Robinhood and Coinbase only. Host apps such as Doujins can ask OpenRails for a funding URL, send the user to the provider in a new tab or popup, then resume checkout after OpenRails and/or the host app verifies that the user's live Solana wallet has enough USDC.

## Product Behavior

- The user already has or creates a Robinhood/Coinbase account on the provider site.
- OpenRails does not custody funds and does not collect provider KYC; the provider handles account login, payment method, buy/transfer, KYC, and compliance.
- Provider redirect/return means the provider flow ended; it is not proof that the wallet is funded.
- Completion must be based on provider status/webhooks when available plus Solana wallet-balance verification.
- Only offer a provider when it can fund USDC on Solana. Coinbase/Base and all EVM chains are out of scope.

## Scope

- Implement Robinhood and Coinbase integration surfaces only for Solana USDC.
- Do not implement Ramp, Transak, MoonPay, PayPal, Venmo, Base, Ethereum, Polygon, Arbitrum, Optimism, or bridge paths for this issue.
- Keep provider abstraction narrow but extensible enough that more providers could be added later without changing the host-app contract.

**Tasks:**
- DESIGN:
- [x] Define the OpenRails funding-session contract for browser self-service callers: provider preference, wallet address, asset, network, requested amount, checkout_session_id, return_url, and idempotency key.
- [x] Define provider statuses and normalize them into OpenRails statuses such as created, opened, pending_provider, pending_settlement, funded, failed, expired, and cancelled.
- [x] Define Solana-only compatibility rules for USDC funding.
- [x] Decide whether funding amount comes from the checkout-session shortfall, an explicit requested amount, or both with server-side validation. Implemented explicit requested amount with optional checkout_session_id context.
- [x] Decide how provider ranking is configured per tenant: Robinhood preferred, Coinbase fallback. Implemented default provider order with Robinhood first and Coinbase second.
-
- DATA MODEL:
- [x] Add a funding/onramp session table with tenant_id, user_id, checkout_session_id, provider, wallet_address, asset, network, requested_amount, provider_session_id, provider_url, status, return_url, idempotency key, timestamps, and provider metadata.
- [x] Add indexes for tenant/user lookup, checkout_session_id lookup, provider_session_id lookup, and idempotency.
- [x] Store provider secrets/config in OpenRails config, never in host apps.
-
- API:
- [x] Add `POST /v1/self/usdc-funding-sessions` to create a Robinhood/Coinbase funding session for the authenticated browser user.
- [x] Add `GET /v1/self/usdc-funding-sessions/:id` to return normalized funding status and provider URL/status details safe for frontend polling.
- [x] Add `GET /v1/self/usdc-funding-options` to list eligible Robinhood/Coinbase options for wallet, network, asset, amount, and optional checkout_session_id.
- [x] Add provider webhook/status callback endpoints where Coinbase supports them. Implemented signed Coinbase Onramp webhook ingestion on the existing provider webhook route; Robinhood remains blocked on partner docs/access.
- [x] Enforce self-service auth, tenant boundaries, and idempotency on funding-session create/read routes.
-
- PROVIDERS:
- [x] Implement a Coinbase provider adapter that creates a hosted onramp URL/session with destination wallet, network, asset, amount, return URL, and partner/user reference, including short-lived CDP JWT bearer generation from Coinbase secret API keys.
- [x] Implement Coinbase status/webhook handling and map provider lifecycle into OpenRails funding-session status. Coinbase success maps to pending_settlement; only live Solana wallet-balance verification can mark funded.
- [ ] Implement a Robinhood provider adapter after partner docs/access are available, supporting external handoff into Robinhood Connect and funding into the user's live wallet.
- [ ] Implement Robinhood status/webhook handling if exposed by partner API; otherwise rely on return handling plus on-chain wallet-balance verification.
- [x] Add provider availability checks so unsupported network/asset combinations are hidden rather than offered.
-
- WALLET VERIFICATION:
- [x] Reuse existing Solana USDC balance-checking code where possible to verify the funded wallet before marking a session funded for Solana checkout.
- [x] Do not add Base/EVM balance verification for this issue; Solana is the only supported chain.
- [x] Ensure returning from a provider only triggers polling/checking; it must not mark the session funded by itself.
-
- CHECKOUT INTEGRATION:
- [x] Allow a funding session to reference the checkout session that produced an insufficient-USDC state.
- [x] Ensure insufficient-USDC API errors expose enough structured amount/network/wallet context for host apps to create a funding session. Added `error.metadata.usdc_funding` with Solana network, USDC asset, wallet, decimal amount/balance/shortfall, and base-unit values.
- [x] Keep final subscription/payment creation in the existing checkout confirmation path after the wallet is funded.
-
- VERIFY:
- [x] Add unit tests for provider eligibility and network compatibility gates.
- [x] Add API tests for create/get funding session, tenant isolation, idempotency, and unsupported-provider/network rejection.
- [x] Add provider adapter tests with mocked Coinbase responses.
- [x] Add wallet-balance verification tests proving redirect alone is insufficient through status semantics and frontend polling contract.
- [x] Document the host-app integration contract for Doujins in config.example.yaml and the tracker issue.

---


# #108: admin-user-search

**Completed:** no

Sophisticated user search for admin dashboard

## Metadata

- Category: feature
- Status: not_started
- Passes: false

**Tasks:**
- STEPS:
- [ ] Design search API:
- [ ]   - GET /v1/admin/users?q=...&filters... - search users with billing data
- [ ] Search fields:
- [ ]   - email (partial match from subscriptions/payments)
- [ ]   - user_id (exact match)
- [ ]   - processor_subscription_id (exact match for support lookups)
- [ ]   - transaction_id (exact match for payment support)
- [ ] Filters:
- [ ]   - has_subscription=true/false
- [ ]   - subscription_status=active/cancelled/past_due/etc
- [ ]   - processor=nmi/ccbill/solana
- [ ]   - has_entitlement=premium/etc
- [ ]   - created_after, created_before (date range)
- [ ] Sorting:
- [ ]   - sort_by=newest/oldest/last_payment/subscription_start
- [ ] Pagination:
- [ ]   - page, page_size, cursor-based pagination for large results
- [ ] Performance:
- [ ]   - Add database indexes for search fields
- [ ]   - Consider full-text search for email
- [ ]   - Rate limit search queries
- NOTE: Billing service searches its own data. Full user profile search (username, etc) lives in main host-app API.

---

# #111: admin-rate-limiting

**Completed:** no

Add rate limiting to admin endpoints to limit blast radius of compromised JWT

## Metadata

- Category: security
- Status: not_started
- Passes: false

**Tasks:**
- STEPS:
- [ ] PROBLEM: If admin JWT is leaked, attacker could cancel/refund thousands of users before detection
- [ ] Implement per-admin-user rate limiting using Redis (keyed by admin user ID from JWT)
- [ ] Define rate limits for destructive operations:
-     - Cancellations: 5/minute, 10/hour, 50/day
-     - Refunds: 5/minute, 10/hour, 50/day
-     - Entitlement revocations: 5/minute, 10/hour, 50/day
- [ ] Define rate limits for bulk/expensive operations:
-     - Extend operations: 3/minute
-     - Off-channel payments: 10/minute
-     - Admin grants: 10/minute
- [ ] On rate limit exceeded: lock out admin for extended period (e.g., 1 hour) and alert
- [ ] Add alerting/notification when rate limits are approached (e.g., 80% threshold)
- [ ] Create admin rate limit middleware that wraps destructive endpoints
- [ ] Allow super-admin or manual override to unlock rate-limited admin if legitimate
- [ ] Log all rate limit events for security audit
- BENEFIT: Limits blast radius - attacker can only affect ~5-10 users before getting locked out

---

# #118: admin-dashboard-metrics-overhaul

**Completed:** no

Overhaul admin dashboard metrics to provide useful business intelligence

## Metadata

- Category: feature
- Status: not_started
- Passes: false

## Details

- api_design: {"dashboard_summary":{"endpoint":"GET /v1/admin/metrics/summary","returns":"Key KPIs for dashboard cards (MRR, active subs, churn rate, etc.)"},"revenue_over_time":{"endpoint":"GET /v1/admin/metrics/revenue?start=DATE&end=DATE&granularity=day|week|month","returns":"Time series of revenue data"},"subscriptions_over_time":{"endpoint":"GET /v1/admin/metrics/subscriptions?start=DATE&end=DATE&granularity=day|week|month","returns":"Time series of subscription counts"},"churn_analysis":{"endpoint":"GET /v1/admin/metrics/churn?start=DATE&end=DATE","returns":"Churn breakdown by reason, cohort retention"},"processor_breakdown":{"endpoint":"GET /v1/admin/metrics/processors","returns":"Revenue and counts by processor"}}
- implementation_notes: ["MRR calculation: sum(price.amount / (price.billing_cycle_days / 30)) for active subs","Need to handle different billing cycles (monthly, quarterly, annual)","Solana one-time payments are not subscriptions - track separately","Consider caching expensive aggregations (refresh every 5 min)","Use PostgreSQL window functions for period-over-period comparisons","May want ClickHouse for historical analytics at scale"]
- problems: ["Limited metrics - only subscription counts, no revenue or business KPIs","Confusing naming - 'without_auto_renew' actually means 'cancelled but still in period'","No revenue metrics - MRR, ARR, total revenue, average revenue per user","No churn metrics - churn rate, retention rate, LTV","No Solana support - processor metrics exclude Solana one-time payments","No period comparisons - can't compare this week/month vs previous","No cohort analysis - can't track retention by signup month","No conversion funnel - signups to first payment to recurring","Tests are minimal - just verify response structure, not actual data accuracy"]
- proposed_metrics: {"revenue_metrics":["MRR (Monthly Recurring Revenue) - sum of active subscription monthly values","ARR (Annual Recurring Revenue) - MRR * 12","Total revenue this period - sum of all payments in date range","Revenue by processor - breakdown by CCBill, NMI, Solana","ARPU (Average Revenue Per User) - total revenue / active users"],"subscription_metrics":["Total active subscriptions","New subscriptions this period","Cancelled subscriptions this period (with breakdown by reason)","Net subscriber change (new - cancelled)","Subscriptions by status (active, past_due, cancelled)","Subscriptions by product/tier"],"churn_metrics":["Monthly churn rate - cancelled / active at start of month","Voluntary churn - user/merchant initiated","Involuntary churn - payment failures","Retention rate by cohort - % of month-N signups still active"],"payment_metrics":["Successful payments this period","Failed payments this period","Payment success rate","Refunds issued","Chargebacks received","Average payment amount"],"one_time_purchases":["Solana payments count and value","One-time purchase revenue (non-subscription)"]}

**Tasks:**
- STEPS:
- [ ] Define exact metrics needed for admin dashboard UI
- [ ] Design API response formats for each endpoint
- [ ] Implement MRR/ARR calculation logic
- [ ] Implement churn rate calculation
- [ ] Add revenue aggregation queries
- [ ] Add one-time purchase tracking (Solana)
- [ ] Add period comparison support (vs last period)
- [ ] Add caching layer for expensive queries
- [ ] Update existing metrics endpoints or create new ones
- [ ] Write comprehensive tests with seeded data
- [ ] Document all metrics definitions

---

# #126: test-architecture-improvements

**Completed:** no

Tests should use real structs and functions from production code, not invent test-specific abstractions

## Metadata

- Category: testing
- Status: not_started
- Passes: false

## Details

- current_problems: ["SubscriptionOptions helper struct uses different field names than models.Subscription (PeriodStart vs CurrentPeriodStartsAt)","Tests can pass while using wrong field names because helpers translate between them","When a model field is renamed/removed, tests may not break because helper abstracts it away","Developers get confused about which struct to use and what fields are available"]
- example_bad: {"code":"suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{PeriodStart: now})","problem":"SubscriptionOptions.PeriodStart doesn't exist on models.Subscription"}
- example_good: {"code":"sub := &models.Subscription{CurrentPeriodStartsAt: now, ...}; suite.CreateTestSubscription(sub)","benefit":"Uses real model, compiler catches field name errors"}
- philosophy: {"principle":"Tests should verify actual production code behavior, not test-specific wrappers","goal":"When production code changes, tests should break if they're testing the affected behavior","anti_pattern":"Test helper structs that diverge from production models hide API changes and create maintenance burden"}
- recommendations: ["Use models.Subscription directly in tests instead of SubscriptionOptions","Use api.ListResponse[T] for parsing API responses instead of map[string]interface{}","Import and use the actual request/response structs from handlers","If a helper is needed, it should take the real model struct as input, not a parallel struct","Test assertions should use strongly-typed response parsing"]

**Tasks:**
- REFACTORING:
- [ ] Audit all test helper structs (SubscriptionOptions, PaymentMethodOptions, etc.)
- [ ] Replace helper structs with direct model usage where possible
- [ ] Update CreateTestSubscriptionWithOptions to accept *models.Subscription
- [ ] Use json.Unmarshal with actual API response types instead of map[string]interface{}
- [ ] Add linter rule or CI check to discourage map[string]interface{} in tests

---

# #158: ccbill-dunning-grace-entitlements

**Completed:** no

Adjust CCBill subscription entitlement logic to model paid term + retries (dunning) + end-of-term expiration. Use CCBill webhook fields like nextRetryDate/nextRenewalDate to drive subscription status and finite entitlement windows, and cap any grace extensions to avoid unlimited free access.

**Tasks:**
- SPEC:
- [ ] Verify CCBill webhook sequence in sandbox: cancel mid-term -> Expiration at end-of-term (and timing)
- [ ] Decide grace policy during retries: disabled vs enabled; cap strategy (extend-to-nextRetryDate vs fixed extra days)
- [ ] Decide policy for CCBill `Cancellation.source=failedRB`: treat as terminal end (no more retries) vs still paid-through end-of-term
-
- DATA MODEL:
- [x] Ensure we can store current_period_ends_at (from nextRenewalDate) for CCBill subscriptions
- [x] Store next_retry_at (from nextRetryDate) and optional grace_ends_at (policy derived)
- [x] Decide whether to reuse existing `subscriptions.next_retry_at` fields for CCBill (recommended) vs add CCBill-specific columns
-
- WEBHOOK -> STATE MACHINE:
- [x] NewSaleSuccess: set paid-term end; grant entitlements for [now, paid_term_end)
- [x] RenewalSuccess: extend paid-term end; extend entitlement windows
- [x] RenewalFailure: set past_due/dunning; persist next_retry_at from webhook; optionally extend entitlements within grace cap
- [x] Cancellation: mark cancelled but keep access until paid_term_end (do NOT revoke immediately)
- [x] Expiration: mark ended/expired and end/revoke entitlements
-
- CODE CHANGES (expected touch points):
- [x] `internal/services/webhook_ccbill.go`: parse and apply `nextRenewalDate` on `NewSaleSuccess` and `RenewalSuccess` (don’t rely solely on `price.billing_cycle_days`)
- [x] `internal/services/webhook_ccbill.go`: on `RenewalFailure`, parse `nextRetryDate` and update `subscriptions.status=past_due` + `subscriptions.next_retry_at` (avoid calling `SubscriptionLifecycleService.FailMembership`, which schedules NMI-style retries)
- [x] `internal/services/webhook_ccbill.go`: on `Cancellation`, call `CancelMembership` with `RevokeAccess=false` (today it passes true)
- [x] `internal/services/lifecycle_service.go`: update `CreateMembership` entitlement grant behavior for CCBill to create finite windows ending at `current_period_ends_at` (instead of indefinite `end_at=NULL`)
- [x] `internal/services/lifecycle_service.go`: update `RenewMembership` for CCBill to extend entitlements to match the new `current_period_ends_at` (today renewal doesn’t extend entitlements at all unless a downgrade changes entitlement set)
- [x] `internal/db/repo/entitlement.go`: make `IsEntitled` / “active entitlement” queries explicitly exclude `deleted_at` (don’t rely on implicit soft-delete behavior)
- [x] `config/config.go`: add a feature flag / config for CCBill grace behavior (enable/disable + max cap)
-
- ANALYTICS:
- [x] Ensure ClickHouse subscription_events reflect dunning/cancelled/expired transitions for CCBill
-
- TESTS:
- [x] Add integration test: NewSaleSuccess -> RenewalFailure(nextRetryDate) keeps access until paid_term_end (+ optional grace) -> RenewalSuccess extends window
- [x] Add integration test: Cancellation mid-term does NOT revoke immediately; entitlement ends at paid_term_end; verify Expiration handling if CCBill sends it
- [x] Add unit test for `EntitlementRepo.IsEntitled` to ensure deleted rows are excluded (regression guard)

---


# #164: cloudflared-managed-dev-tunnel

**Completed:** no

Make `Config.Cloudflared` an actual supported dev feature: OpenRails can (optionally) run and manage a Cloudflare Tunnel for local/dev, so a full local stack (e.g., multiple host apps + billing) can be accessed from an external device (phone) over HTTPS.

## Metadata

- Category: devx
- Status: planned
- Passes: false

## Goal

- With `cloudflared.tunnel_token` configured, a developer can start OpenRails and get a stable public hostname that routes to the local billing instance, usable from a phone browser/app.

## Non-goals

- Do not require Cloudflared in production.
- Do not make Cloudflared a hard dependency for boot.

## Notes

- This is for local/dev convenience (public ingress), not webhook signature bypass.
- Prefer explicit opt-in (config flag or separate command) so production never spawns subprocesses unexpectedly.

**Tasks:**
- DESIGN:
- [ ] Decide opt-in mechanism: (A) standalone CLI subcommand `openrails cloudflared up` that supervises both OpenRails + cloudflared, or (B) OpenRails spawns cloudflared when `cloudflared.tunnel_token` is set and `env=dev`
- [ ] Decide what constitutes “success”: tunnel established + /health/ready reachable via public hostname
-
- IMPLEMENTATION (if spawning subprocess):
- [ ] Add a small `pkg/cloudflared` supervisor that starts `cloudflared tunnel run` with the configured token and captures stdout/stderr into structured logs
- [ ] Ensure clean shutdown: propagate SIGINT/SIGTERM, kill child process group, avoid zombies
- [ ] Add a readiness check that probes the configured `cloudflared.public_hostname` (or local route) and reports status
-
- CONFIG / DOCS:
- [ ] Clarify config semantics in config.example.yaml (token is secret; hostname is non-secret; tunnel name optional)
- [ ] Document a phone-access workflow in docs (prereqs: cloudflared installed or image available; set APIURL/CORS origins appropriately)
-
- SECURITY / SAFETY:
- [ ] Ensure the service API (port 8060) is not accidentally exposed publicly by default; only expose the public API unless explicitly configured
- [ ] Ensure `api_key` and auth verification are still required for protected routes
-
- VERIFY:
- [ ] Add a lightweight unit test for supervisor command construction (no subprocess exec)
- [ ] Manual dev verification: start stack + tunnel and hit /health/ready from an external device

---

# #288: processor-routing-and-fallback-policy

**Completed:** no

Add deterministic processor selection and fallback policy so checkout can choose the best available rail for a tenant/product/tier/user context before creating a one-processor checkout session.

## Metadata

- Category: feature
- Status: planned
- Passes: false

## Motivation

- Hyperswitch wins on payment orchestration. OpenRails should not chase broad smart-routing, but customers do need basic processor redundancy and product/tenant-aware routing, especially high-risk merchants with Stripe plus NMI/Mobius/CCBill/Solana.
- Keep checkout sessions one-processor-at-a-time; route before session creation.

## Non-goals

- No ML routing.
- No 200-connector orchestration platform.
- No automatic retry to a second processor after a successful authorization attempt unless the processor contract makes that safe.

**Tasks:**
- DESIGN:
- [ ] Define routing inputs: tenant_id, product_id, price_id, tier_group, amount, currency, billing cycle, user country/state when known, processor availability, processor capability metadata (#291), and explicit client preference.
- [ ] Define routing outputs: selected processor, fallback candidates, reason, and policy version.
- [ ] Decide precedence: explicit price/provider config > tenant policy > product/tier_group policy > global default.
- [ ] Decide failure classes that can trigger fallback before checkout finalization: processor unavailable, unsupported capability, credential missing, sandbox/live mismatch, hard validation failure. Do not fallback after a successful charge.
-
- DATA MODEL / CONFIG:
- [ ] Add routing policy representation in DB or catalog manifest: allowed processors, preferred order, disabled processors, and optional per-tier overrides.
- [ ] Extend catalog-as-code manifest only if needed; prefer using existing provider lists as the first version.
- [ ] Record selected processor and routing reason on checkout_sessions for auditability.
-
- IMPLEMENTATION:
- [ ] Add a ProcessorRouter service used before checkout session creation and legacy checkout flows.
- [ ] Integrate with processor health/capability metadata so unsupported processors are filtered with explicit errors.
- [ ] Add dry-run endpoint/CLI to explain routing for a price/user/tenant without creating a checkout.
- [ ] Ensure idempotency keys bind to the resolved processor/policy so retries do not unexpectedly switch rails.
-
- VERIFY:
- [ ] Unit-test policy precedence and fallback filtering.
- [ ] Integration-test Stripe primary -> Mobius fallback when Stripe is disabled/unavailable before charge.
- [ ] Integration-test product constrained to Mobius does not route to Stripe even if Stripe is configured.

---

# #290: provider-certification-matrix

**Completed:** no

Publish and maintain a provider certification matrix for Stripe, NMI/Mobius, CCBill, and Solana that records exactly which customer-visible flows are supported, sandbox/devnet tested, live/test-mode tested, and known-limited.

## Metadata

- Category: product
- Status: planned
- Passes: false

## Motivation

- OpenRails' strongest differentiator is deep support for real non-Stripe rails, especially NMI-compatible high-risk gateways like Mobius. Customers need confidence that the specific flows they care about actually work.
- This should become both documentation and an executable certification harness where practical.

**Tasks:**
- MATRIX DESIGN:
- [ ] Define provider capabilities and certification statuses: not_supported, manual_only, unit_tested, integration_tested, sandbox_certified, live_test_mode_certified, devnet_certified, production_certified.
- [ ] Define flows to track: catalog/product push, price/recurring-plan push, one-time checkout, recurring checkout, vault/tokenization, rebill, cancellation, deferred cancellation, refund, dispute/chargeback, webhook handling, subscription sync/backfill, catalog drift detection.
- [ ] Include processor-specific notes: NMI product/prices are local while recurring prices push as NMI recurring plans; CCBill catalog actions may be manual; Solana recurring requires on-chain readback/devnet certification.
-
- DOCS:
- [ ] Add docs/providers.md or equivalent with the current matrix and exact tested commands.
- [ ] Add NMI-compatible gateway guidance: required security_key, Collect.js/tokenization key if needed, direct/query endpoints, test_mode behavior, and how Mobius/NMI white-label accounts map to the same interface.
- [ ] Add troubleshooting for common provider failures: bad endpoint, key belongs to different gateway user, sandbox URL not supported, recurring plan query mismatch, webhook signature failure.
-
- EXECUTABLE CERTIFICATION:
- [ ] Add or formalize focused integration tests for NMI/Mobius sale, vault, recurring plan create/readback, and query API.
- [ ] Add Stripe test-mode catalog apply + subscription sync certification steps.
- [ ] Add Solana devnet read-back certification for plan accounts and recurring lifecycle.
- [ ] Add CCBill manual-action verification path so unsupported remote catalog operations surface as pending_manual_actions rather than errors.
-
- PROCESS:
- [ ] Make certification matrix updates part of provider-related PRs.
- [ ] Record last verified date, environment, and command for each provider flow without exposing secrets.

---

# #291: processor-capability-metadata

**Completed:** no

Expose processor capability metadata in code, APIs, catalog planning, checkout validation, and admin/provider status so OpenRails can explain what each rail supports instead of relying on scattered processor-specific conditionals.

## Metadata

- Category: architecture
- Status: planned
- Passes: false

## Motivation

- Stripe, NMI/Mobius, CCBill, and Solana have different lifecycle semantics. Customers need predictable errors and routing decisions. OpenRails also needs a shared capability source for routing/fallback, catalog-as-code, checkout validation, and the provider certification matrix.

**Tasks:**
- CAPABILITY MODEL:
- [ ] Define ProcessorCapabilities with booleans/enums for recurring, one_time, vault/tokenization, hosted checkout, redirect checkout, direct sale, catalog push, recurring plan push, refund, dispute, cancel immediate, cancel deferred, remote subscription listing, remote dedup check, webhooks, drift enumeration, and manual actions.
- [ ] Add capability details for current processors: stripe, mobius/NMI-backed, ccbill, solana.
- [ ] Distinguish processor class capabilities (NMI-backed) from named provider overrides (Mobius).
-
- INTEGRATION POINTS:
- [ ] Use capabilities in checkout validation instead of hard-coded processor switches where practical.
- [ ] Use capabilities in catalog apply/plan to decide provider actions and pending_manual_actions.
- [ ] Use capabilities in routing/fallback policy (#288) so unsupported rails are filtered before checkout.
- [ ] Surface capabilities through admin/provider status endpoints and docs.
-
- ERRORS / UX:
- [ ] Return structured unsupported-capability errors with processor, capability, and suggested alternative when possible.
- [ ] Ensure user-facing errors are clean while admin/debug surfaces retain processor detail.
-
- VERIFY:
- [ ] Unit-test capability metadata for each supported processor.
- [ ] Regression-test known special cases: CCBill manual catalog actions, NMI recurring plan push, Stripe remote dedup check, Solana one-off/recurring distinction.

---

# #320: Add Hyperswitch payment vault support for cloud and self-hosted deployments

**Completed:** no

Add Hyperswitch as an optional OpenRails payment provider and payment-method vault integration, covering both Hyperswitch Cloud and self-hosted Hyperswitch. This is payment vaulting/tokenization, not HashiCorp Vault merchant-secret storage. OpenRails should store only opaque Hyperswitch customer/payment-method identifiers plus non-sensitive metadata, while PAN/card collection stays in Hyperswitch-hosted/client-side tokenization flows or equivalent PCI-scoped Hyperswitch surfaces.

This issue must reconcile with future issue #297 (`deplatforming-resilient-card-vault`): Hyperswitch should not be positioned as the default adult/high-risk deplatforming-resilient vault until its contractual/export/compliance posture is explicitly certified. Hyperswitch Cloud may still be useful for lower-risk deployments or as an optional provider, and self-hosted Hyperswitch may be a break-glass/advanced deployment path if the operator accepts and satisfies the PCI requirements.

This should integrate with the provider capability and routing work in #288, #290, and #291: Hyperswitch can be selected as a processor/vault provider when the tenant/provider config says it is available, and OpenRails can route checkout/setup flows through it without treating it as a generic secret vault.

**Tasks:**
- [ ] Research the current Hyperswitch Cloud and self-hosted API surfaces needed for customers, payment methods, payment/setup intents, saved payment methods, refunds/voids, webhooks, connector routing, and vault/tokenization modes; record any version assumptions in docs.
- [ ] Reconcile the implementation plan with future issue #297: document whether Hyperswitch is an optional provider, a lower-risk deployment choice, or a PCI-heavy break-glass/self-hosted vault path; do not make it the default portable adult/high-risk vault without explicit certification.
- [ ] Define the OpenRails provider config shape for `hyperswitch`: cloud vs self-hosted mode, API base URL, optional vault/base URL split if Hyperswitch requires it, merchant/profile/account identifiers, API key secret reference, webhook secret reference, return/callback URLs, and test/live mode.
- [ ] Store Hyperswitch credentials in the existing tenant secret store path; do not store processor API keys in bootstrap YAML, database rows, logs, or generated frontend config.
- [ ] Extend provider capability metadata (#291) so Hyperswitch can advertise supported flows: payment-method vault/tokenization, one-time checkout, recurring/setup/mandate behavior if supported, refunds, webhooks, processor-side routing, remote payment-method listing, and manual certification status.
- [ ] Add a provider adapter/client abstraction that lets NMI customer-vault IDs and Hyperswitch payment-method IDs fit the same OpenRails payment-method model without hard-coding NMI semantics into checkout/subscription flows.
- [ ] Implement Hyperswitch customer/payment-method setup flow using an opaque token or hosted/client-side Hyperswitch collection result; persist only tenant subject, provider, external customer ID, external payment method ID, brand/last4/expiry metadata, and status.
- [ ] Implement checkout/subscription charge paths that can use a saved Hyperswitch payment method and reconcile the resulting payment/subscription state back into OpenRails records.
- [ ] Implement webhook verification and event handling for successful payment, failed payment, refund/void, payment-method updates/deletes, and any recurring/mandate lifecycle events OpenRails relies on.
- [ ] Add self-hosted operational docs: required Hyperswitch services, base URL/TLS requirements, webhook reachability, secret injection, health checks, and local compose/dev smoke path if practical.
- [ ] Add cloud operational docs: required Hyperswitch Cloud credentials, webhook setup, provider certification checklist entry (#290), and tenant bootstrap/provider-link examples.
- [ ] Add tests with a mocked Hyperswitch server/client for tokenization/setup, saved-method charge, failure mapping, webhook signature verification, idempotency, tenant isolation, and no-sensitive-card-data persistence/logging.
- [ ] Validate with focused Go tests for provider/checkout/vault/webhook code, compile-only full package coverage, `task build`, and an optional live sandbox/self-hosted smoke test when credentials or local Hyperswitch are available.

---

# #331: Tune card-abuse rate-limit windows (#371): 15-min 3->captcha/6->block, 24h 10->block per user+ip, keep global 100->attack-mode

**Completed:** no

The #371 CardAbuseGuard (internal/modules/abuse/card_abuse.go), middleware subject-key plumbing, checkout hook, and integration tests already landed on master. This refines the policy to the agreed windows. Card-testing is defined by volume over time, so pair a short BURST window with a rolling DAILY cap, both keyed per user AND per IP. Final thresholds (user-specified): a 15-MINUTE rolling window per (user+ip) where 3 failed card charges -> future attempts require captcha, and 6 failed charges -> cut off (blocked) for the remainder of that 15-min window; PLUS a 24-HOUR rolling cap of 10 failed charges per (user+ip) -> cut off for the day. Keep the existing site-wide GLOBAL cap of 100 failed charges / 24h -> attack mode (captcha for every request from everyone).

**Tasks:**
- [ ] DefaultCardAbuseConfig: short window 15 min, CaptchaAfter=3, BlockAfter=6 (block lasts the remainder of the window)
- [ ] Add a second per-subject rolling 24h window with a cap of 10 failed charges -> block; the Limiter already returns multiple windows, so configure the daily window + add the second-window check in RecordChargeFailure
- [ ] Keep both subject keys (user: and ip:) so a logged-in attacker rotating cards and a bot rotating accounts behind one IP are both caught
- [ ] Keep the global 24h/100 attack-mode (captcha for everyone) unchanged
- [ ] Extend the abuse integration tests (real Redis) to cover the 15-min 3/6 thresholds and the 24h/10 daily cap; go build ./... + go vet ./... clean

---

# #333: admin-e2e-suite-needs-control-plane-wiring

**Completed:** no
**Status:** OPEN 2026-06-10 (Claude): diagnosed during the #332 failure review; not started. All other e2e failures from that review are fixed (see completed #332 + commits 9fe72d4 openrails, 68cb6a1 tensorhub, b72f61e8 cozy-art).

The admin e2e tests (tests/admin_payments_test.go, admin_metrics_test.go, admin_entitlements_source_test.go, admin_offchannel_payments_test.go) fail with 500 {'message':'authorization unavailable'} — the nil-admin-checker fail-closed path. DIAGNOSIS (2026-06-10): pre-existing #312 bit-rot, documented in the suite itself (testcontainer_suite.go Auth config comment: 'Admin-route integration tests therefore require the embedded control plane wired with the test admin granted openrails:admin'). The #312 hard-cut moved admin authority from JWT claims (tests still mint operatorAdminClaims() tokens via test_helpers.go) to the LIVE control-plane permission check (routes_admin.go only sets AdminPermissionChecker when s.controlPlane != nil; the suite never enables Auth.ControlPlane -> checker nil -> admin_neutral.go/ginmw admin gates 500 fail-closed).

WHAT THE FIX NEEDS:
1. suite.Config.Auth.ControlPlane = &config.ControlPlaneConfig{Enabled: true} (ginboot embcp.Attach then builds it; authkit profiles schema is already applied by migrate.RunPostgres).
2. The admin test users must EXIST in authkit profiles.users with ids equal to the JWT subs the suite mints, then be made members + granted the admin role (controlplane Bootstrap/AddMember/AssignRole or authkit core APIs — NOT raw SQL into roles). authkit core.CreateUser(email, username) generates its own id, so either (a) resolve the verifier's user mapping (issuer+sub -> user) and mint tokens for created users' real ids, or (b) add a test-only authkit user-provisioning hook.
3. operatorAdminClaims()/CreateAdminToken in tests/test_helpers.go are then vestigial — admin authority comes from the role grant, not claims.

NOTE: the admin-METRICS tests' 'Table test_analytics.daily_metrics does not exist' failures were a STALE TEST ENV artifact (migratekit records the ClickHouse ledger in postgres; reusing a postgres DB across runs against fresh CH containers skips re-apply). On a fresh env CH applies fine and those tests fail at the same admin-auth gate instead. Squashed CH baseline (cb9200b) is NOT at fault (daily_metrics present; migrations/clickhouse schema_test passes).

**Tasks:**
- [ ] Enable Auth.ControlPlane in testcontainer_suite config and verify embcp.Attach builds it in ginboot.
- [ ] Provision admin test users in authkit profiles.users matching the JWT subs (or mint tokens from created users' ids); AddMember + AssignRole openrails:admin via control plane / authkit core APIs.
- [ ] Re-run tests/admin_*.go on a fresh env; remove vestigial operatorAdminClaims if green.
- [ ] (env hygiene) consider failing loudly or re-validating when the migratekit CH ledger says applied but the CH database lacks the tables (stale-ledger detection).

---


# #340: e2e: 5 tier/lifecycle tests fail on master (pre-existing, surfaced by #334 verification)

**Completed:** no
**Status:** open (filed 2026-06-10 during #334 verification)

TestTierGroupDetection, TestEntitlementChangesOnTierChange, TestScheduledDowngrade, TestRenewMembershipDuplicateTransactionIsNoOp, TestLifecycleServiceUsesMockClock fail identically on origin/master (bun era) and on the sqlc-migration branch — verified via worktree runs on 2026-06-10 during #334 final verification, so they are NOT migration regressions. Observed modes: duplicate key on uq_subscriptions_tenant_subject_tier_group_active across subtests (CleanupSubscriptionsForUser resolves non-UUID test user ids to uuid.Nil via identity.TenantSubjectIDFromString, so per-user cleanup deletes nothing), and mock-clock/lifecycle assertion failures. Distinct from the admin_* family (#333). Suspect shared-suite state + the broken per-user cleanup; fix the cleanup to resolve the tenant subject through billing.tenant_subjects (issuer 'openrails:legacy-user') instead of pure-parsing.

---

# #343: doujins-legacy-billing-import

**Completed:** no
**Status:** planned 2026-06-10; prerequisite/companion to #107 (reconcile corrects whatever the import gets wrong)

One-time migration of legacy doujins user billing state (subscriptions, entitlements, payment-method references) from the legacy machine into OpenRails, ahead of the processor reconciliation (#107). The import is best-effort by design: after it lands, run #107 advisory then enforce against NMI + CCBill (the source of truth) to correct the drift the legacy system accumulated (manual dunning dead for months, failed renewals never downgraded, duplicate subscriptions).

## Metadata

- Category: migration/tooling
- Status: planned 2026-06-10
- Passes: false

## Notes

- Requires production NMI + CCBill credentials wired to the doujins tenant in OpenRails (legacy machine has the real ones; the current dev key is the Mobius SANDBOX account).
- Legacy export source/format TBD (legacy doujins DB); rows land in billing.* under the doujins tenant — make sure tenant_id is stamped correctly (see #336).
- #107 bootstrap mode is the fallback for anything the legacy export can't provide.

## Dunning forensics input

Preserve legacy dunning state verbatim on import — last_retry_at, retry_attempts, next_retry_at (and any legacy dunning/rebill log tables) — do NOT zero these out, they are the evidence #107's dunning-forensics report needs to distinguish 'dunning tried and failed' from 'dunning never ran'. Also produce a quick legacy-side report at export time: per past_due/stale subscription, were rebill attempts ever recorded (count, timestamps, outcomes), and when did the dunning job last touch anything.

## Safe-boot flags for the cutover

Boot OpenRails with production credentials in passive mode: `FEATURE_FLAGS_DUNNING_MODE=dry_run_only` (NOT `off` — off changes rebill-failure semantics to immediate cancellation; dry_run_only runs the workflow, logs due subscriptions, attempts zero charges, preserves retry state) and optionally `FEATURE_FLAGS_DISABLE_ENTITLEMENT_EXPIRATION=true` while reconciling. CRITICAL: the default dunning mode is ON and the periodic job fires every 4h — importing months of stale past_due subscriptions with next_retry_at in the past and booting with defaults would mass-charge every one of them. Set dry_run_only BEFORE first boot with imported data. User-initiated flows (vault saves, requested charges, checkout) are unaffected by these flags.

## Migration-coverage audit vs ~/doujins `migrate legacy` (2026-06-10, verified against doujins-legacy PHP + doujins Go code)

COVERED by the existing migration: user identity via legacy_user_identities + AuthKit (emails w/ fallback resolution, password hashes); Mobius subscription_id + CCBill ccbill_sub_id -> processor_subscription_id; customers_vaults.customer_vault_id -> billing.payment_methods.vault_id + billing.processor_customers; manual_exp/manual_expiry -> admin_grants + entitlements; chargeback/void -> cancel_type; billings_logs/mobius_logs/cancellations_logs -> ClickHouse events; role_user premium WITHOUT a billing source is explicitly detected and recorded as blocked ("hardcoded premium access was not inferred", subscription_entitlements.go:823) — suspicion-2 drift is surfaced at migration time, not silently dropped.

GAP 1 — rebill/dunning attempt history NOT migrated: users_logs (action='rebill-attempt', Rebill-Success/Rebill-Failed, source='RebillingSystem') and vault_logs (full rebill responses) have ZERO references in internal/legacy_migrate. This is THE evidence for 'did manual dunning run / fail'. Fix: migrate both to ClickHouse subscription/payment events like the other log tables, or at minimum archive the legacy dump before decommission; the export-time forensics report must read these tables.

GAP 2 — in-flight dunning arrives dead: legacy status 'void' (the dunning target: RebillingSystem processes void subs with customers_vaults.retry_status='ON') maps to cancelled/cancel_type=merchant (subscriptions.go:63,711); retry state (retries/retry_date/retry_status) survives only as JSON in payment_methods.metadata, never as past_due + last_retry_at/retry_attempts/next_retry_at on billing.subscriptions — so OpenRails dunning (past_due-only) will never resume them. Probably the RIGHT default (auto-resuming charges on months-stale cards = mass-charge hazard), but make it an explicit decision; #107 will surface these as PS-2/PS-3 against NMI's live recurring state and the admin queue disposes of them.

GAP 3 — payment history is initial-transaction-only: one billing.payments row per subscription from subscriptions.transaction_id, and NONE for void/chargeback subs (legacySubscriptionPayment returns nil, subscriptions.go:1261); years of rebill charges exist only in users_logs/vault_logs (not migrated) and at the processors. Acceptable iff #107 PS-4 backfill (NMI transaction query + CCBill DataLink/transaction exports) lands; otherwise local revenue history is permanently incomplete. Also ccbill_rebill (next rebill date) is metadata-only — fine, CCBill dunns on its own side.

Also boot the cutover with `FEATURE_FLAGS_DISABLE_PROCESSOR_SUBSCRIPTION_DELETIONS=true` (#344 kill switch) so no remote NMI subscriptions are deleted while local state is being converged; the dunning window (#344, default 15d) prevents stale-backlog charges even once dunning_mode=on.

**Tasks:**
- [ ] Inventory legacy doujins billing schema + produce export from the legacy machine
- [ ] Map legacy users -> tenant_subjects (authkit identities) under the doujins tenant
- [ ] Import subscriptions / payments / entitlements / payment-method references (correct tenant_id stamping, see #336)
- [ ] Preserve legacy dunning state on import (last_retry_at / retry_attempts / next_retry_at + legacy dunning logs) — evidence for #107 forensics, do not reset
- [ ] Legacy-side dunning report at export time: per stale subscription, rebill attempts recorded (count/timestamps/outcomes); when the dunning job last ran at all
- [ ] GAP 1: migrate users_logs + vault_logs (rebill-attempt evidence) to ClickHouse events — or archive legacy dump pre-decommission; forensics report reads them — filed as doujins #387 (incl. dump-coverage findings: the loaded 2026-06-09 dump lacks users_logs/vault_logs; billing_methods/card_infos in no dump)
- [ ] GAP 2: decide disposition of legacy void+retry_status=ON subs (stay cancelled vs revive as past_due with retry fields) — explicit decision, default stay-cancelled + admin queue via #107 — doujins #387
- [ ] GAP 3: confirm #107 PS-4 payment backfill covers rebill history, or extend migration to synthesize payments from users_logs/vault_logs — doujins #387
- [ ] Wire production NMI + CCBill credentials to the doujins tenant
- [ ] Boot with FEATURE_FLAGS_DUNNING_MODE=dry_run_only (+ optionally DISABLE_ENTITLEMENT_EXPIRATION=true) BEFORE first start with imported data; flip to on only after enforce-mode convergence
- [ ] Run #107 advisory against NMI + CCBill; review the drift report
- [ ] Run #107 enforce; work the admin action queue (duplicate subscriptions -> manual cancel+refund)

---

# #494: Multi-currency support — native currencies + abstract units + internal FX converter

**Completed:** partial — native-currency budget groundwork complete in this repo; FX converter capstone not built. Migration 031 shipped the additive
`currency` columns; this pass adds migrations 032/033 and wires the code to the final native-money shape:
`currency + amount/limit/balance` language, `budget_inflight_holds.amount/captured`, renamed money settings/spend
cap columns, and budget window/hold keys scoped by currency.

**Outcome (2026-06-15):**
- [x] Recreated migration 032 as plain DDL so sqlc sees it: `budget_reservations` ->
      `budget_inflight_holds`; old hold amount/captured columns -> `amount/captured`; per-invoker spend
      limit columns use amount-language names.
- [x] Added migration 033 for the deeper native-money rename: money settings columns now use
      `max_spend_per_day`, `max_spend_per_month`, `max_outstanding_owed_amount`,
      `low_balance_threshold`, `outstanding_owed_amount`, and `credit_limit_amount`; JSONB policy/schedule
      documents are migrated to `limit`, `min_cumulative_paid_amount`,
      `max_concurrent_held_amount`, and `max_single_charge_amount`.
- [x] Re-keyed budget state and holds by currency:
      `budget_window_state UNIQUE(merchant_id, customer_id, invoker_id, currency, window_key)` and
      `budget_inflight_holds UNIQUE(merchant_id, customer_id, invoker_id, currency, source, source_id)`,
      with matching aggregation indexes.
- [x] Threaded native currency through admit -> budget reserve -> money hold. Empty currency still resolves
      to `USD`; budgets now validate against the OpenRails native currency registry and reject custom-credit
      units for these native-money tables.
- [x] Settlement by hold now captures/releases budget holds by `(payer, currency, source, source_id)`.
- [x] Renamed public/service SDK amount fields and wire JSON away from fixed-precision wording:
      `EstimatedAmount`, `BalanceAmount`, `AvailableAmount`, `OutstandingOwedAmount`, `Limit`,
      `ReportWastedSpend(... amount ...)`, `threshold_amount`, etc.
- [x] Regenerated sqlc and updated generated-field call sites (`Amount`, `Captured`,
      `MaxSpendPerDay`, `MaxSpendPerMonth`, `CreditLimitAmount`).

**Not built here:** the FX converter capstone, now tracked as #501. The current implementation supports same-currency budget
deduction and prevents cross-currency row/key collisions. Cross-currency deduction still needs a budget
currency policy contract plus rate-source/staleness/rounding decisions before conversion can be applied
correctly.

**Verification:**
- `task sqlc`
- `go test ./...`

---
