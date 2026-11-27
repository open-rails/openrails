# Collection and dunning

Collection decides what happens when a customer's card refuses money OpenRails asked for. The decline table maps every rail code to one reason and action; the retry schedule collects money owed (renewals OpenRails collects, usage invoices); arrears delinquency tracks how long a usage debt goes unpaid. Merchants set the retry schedule, for renewals and usage invoices alike, and the arrears grace periods. Renewal admission and the state machine are in
[subscriptions](subscriptions.md), usage rating in [usage billing](usage-billing.md).
Guides: [dunning](../client.md#subscriptions), [spend limits](../client.md#limits-and-the-gate).

## Concepts

- **Decline table:** `internal/decline`: rail code to `billing.DeclineReason`,
  its `billing.DeclineCause` (customer, issuer, fraud, merchant, processing or
  unknown, which fixes the `billing.AttemptOutcome`: declined, refused or
  error) and `Action` (`Retry`, `FixPaymentMethod`, `NonRecoverable`).
- **Dunning policy:** the `dunning_policy` setting (`billing.DunningPolicy`),
  resolved into a `collection.Policy`.
- **Case:** a `past_due` or `awaiting_method` membership, run under the policy
  it recorded at its first decline.
- **Window:** `Policy.Window`, the last offset plus min(24 h, cycle/2). A case
  ends there unless a retry ran late (`subscriptions.CaseEnd`), and never runs
  past the next renewal.
- **Certainty leg:** what a terminal outcome needs (`collection.Certainty*`).
- **Delinquency:** `current`, `grace` or `delinquent` per (merchant, customer,
  currency).

## How it works

**Classifying.** `decline.ClassifyEvidence` looks up the code. The issuer's own
code (NMI's `processor_response_code`, which a subscription collection's
decline reads from the Query API, or Stripe's `network_decline_code`) names the
cause of a generic decline, but never makes it terminal. A CVV mismatch
overrides the code, and a ZIP mismatch replaces a generic decline. A Stripe
charge Radar blocked is classified by its block reason (`blocked_by_psp`), not
the fraudulent decline it reads as. Network advice turns a retry into
`FixPaymentMethod` but never makes it terminal: Stripe `do_not_try_again` or
`confirm_card_data`, Mastercard 03, 21, 01, 40 or 41, or an issuer code in
`decline.IssuerForbidsReattempt`. Mastercard 24 to 30 set `Result.RetryAfter`.
An `Unmapped` code still retries. `decline.AlertUnmapped` logs it, and the
hourly health pass raises `life.decline.unmapped`. Each answer is a
`payment_attempts` row with its outcome, cause, reason, action and the PSP's
and network's advice codes.

**Schedule.** `collection.DefaultPolicy` retries nothing under 4 days, +1 d and
+2 d under 28 days, and +2, +4, +6 and +9 d beyond; transient answers first get
a 5 and 30 minute ladder. `collection.PolicyOf` fills omitted fields, and
`merchantconfig` validates the setting with it. `Policy.Validate` refuses
retries past the shortest cycle they cover, or attempts on one card that could
pass 15 in 30 days or 10 in 24 hours. A cycle of 0 or less is
`collection.ErrUnknownCycle`: nothing is charged or ended, and
`life.cadence.unknown` is raised.

**Subscription dunning.** `SubscriptionLifecycleService.FailMembership` applies
each declined renewal attempt under the subscription's row lock. (Stripe- and
CCBill-owned subscriptions retry at the provider; OpenRails mirrors `past_due`.)

- The first decline opens the case (`openCase`). It stores the policy in full
  (`collection.DeclaredOf`) in `subscriptions.dunning_policy`.
- `Retry` bumps `transient_retries` or `retry_attempts` and sets
  `next_retry_at`, no sooner than the network advised after the decline
  (`FailMembershipParams.RetryAfter`, from `DeclinedAt`) or an earlier
  decline's advice still allows; `retry_advised_at` records an advised retry.
  The case then ends at `subscriptions.CaseEnd`: the window's end, or the slack
  past the last retry the schedule will make, so a delayed case still makes
  every attempt its policy allows, but that slack never runs past the next
  renewal (`NextRenewalAt`: NMI's own next charge for an NMI schedule whose
  accepted schedule dates it, else the cycle). Advice that allows no retry
  before the next renewal leaves the case `past_due` with no retry and
  `grace_ends_at` at that renewal, where the due pass ends it
  (`EndAdviceDeferred`). A case whose last scheduled retry the advice set ends
  exhausted with cancel feedback `issuer_deferred`. A retry late for any other
  reason keeps the policy's schedule, and a quick transient retry still waits
  for standing advice. Advice is a card's: a new card, or the old one an
  account updater replaced, drops it and the deferral it set (`ReplaceMethod`,
  or `ForgetAdvice` when staff choose the card). A spent schedule is
  `lifecycle.DunningExhausted`. A customer's own retryable engine decline spends
  no scheduled retry and keeps the case's count, next retry and end; network
  advice can still delay that retry.
- `FixPaymentMethod` moves the case to `awaiting_method`, sends
  `payment_method_update_required` (`payment_method_failed` when it queues a
  refresh of a possibly reissued card) and sets `grace_ends_at` to the window's end;
  an expired card may queue a refresh ([payment methods](payment-methods.md)).
- `NonRecoverable` or a spent schedule cancels, ends access and queues the provider cancel, given a certainty leg and an open `destructive.Gate`. Otherwise `lifecycle.TerminalHeld` keeps engine members `past_due`, makes NMI-schedule members `unverified`, and raises `life.terminal_outcome.held`, which alerts the merchant's operator once per case; its recommended action names what lets the outcome apply (the merchant's destructive arming, a billing import's cancellation cap, a re-import, or a provider check).
  A held engine case's `grace_ends_at` is the held instant; `EndHeldOutcome` ends it on the next due pass once the merchant is armed. Its held finding closes when the case ends by any route.
- Access while dunning is a grace grant (`dunningAccess`), unless
  `access_during_dunning` is `suspend`.

**Due pass.** `riverjobs.DunningWorker` (`openrails.dunning`) runs in each
hourly `openrails.billing_pass`. It reads merchant ids
(`ListDueDunningMerchants`), then pages `ListDueDunningSubscriptions` inside
each merchant's `RunInMerchantScope`. A due row gets one action: an NMI retry
is enqueued (`ManualRebillHandler.EnqueueScheduled`); an engine renewal is
admitted ([subscriptions](subscriptions.md)); an expired wait ends
(`ExpireAwaitingMethod`), and so does a case the advice deferred to its renewal
(`EndAdviceDeferred`); a stale row parks `unverified` (`ApplyLocalUnknown`);
or the provider is asked by id (`AskProviderPastGrace`). A second scan asks about
provider-billed renewals unpaid past their rail's deadline, 24 hours for an NMI
schedule and 48 for Stripe and CCBill (`AskProviderOverdueRenewal`). A row
whose processing fails raises `life.due_pass.refused`; every row left unchanged
gets `due_recheck_at` from `deferDue`.

**Customer retries.** `POST /v1/me/subscriptions/{id}/retry-now` (NMI
schedules) and paying an engine renewal invoice with the subscription's card pass
`intents.AdmitCustomerRetry` before any provider call. From
`GetCycleCardStanding` it refuses (`ErrRetryNeedsNewMethod`) once the customer's
declines on the card reach `customer_retries` (default 3, per card), after a
non-recoverable decline or forbidding issuer code, and while `awaiting_method` unless
`decline.HolderCanClear`. Another card is always admitted.

**Usage invoices.** An `order_collection` operation charges a due invoice. On a decline, `InvoiceCollectionHandler` applies `collection.FailureAction` to the answer and its advice (Stripe's and the network's) under the merchant's current `dunning_policy`, with the invoice's period as the cycle (at least 4 days); `usage.DeclineCollection` sets `next_collection_attempt_at`, marks it `uncollectible`, or stops collecting. Its transient answers count as failures; there is no transient ladder.

**Delinquency.** `delinquency.EvaluateTx` runs in the transaction of every
usage-invoice change (`usage.Changed`), and from `riverjobs.DelinquencyWorker` at
`next_evaluation_at`. It reads `GetOverdueInvoiceAggregate` under the merchant
policy and the customer's billing-policy overrides; `delinquency.Classify`
decides the state. `UpsertCustomerDelinquency` returns the state before and
after, and `transition_seq` advances only on a change. A change writes a
`host_outbox` row deduplicated on `transition_seq` (`delinquency.grace`,
`.entered`, `.cleared`); entering or leaving `delinquent` also notifies the
customer. Admission refuses `delinquent_unpaid_invoice` only if `IsDelinquent`'s
live recompute agrees. Nothing is revoked or canceled.

Tables: `subscriptions` (`retry_attempts`, `transient_retries`,
`next_retry_at`, `grace_ends_at`, `dunning_policy`, `due_recheck_at`), `invoices`
(`next_collection_attempt_at`), `payment_attempts`, `rebill_cycles`,
`customer_delinquency`, `provider_intents` and `reconciliation_findings`.

## Interfaces

- **Go:** `Client.ListSubscriptions` (`SubscriptionListParams.Dunning`, giving `billing.SubscriptionDunning`), `ListRenewals`, `ListPaymentAttempts`, `ListInvoices`, `WaiveInvoiceBalance`, `MarkInvoiceUncollectible`, `ListFindings`, and `UpdateMerchantConfiguration` (`DunningPolicy`, `ArrearsGraceDays`, `UsageBilling` with each currency's `DelinquencyFloor`).
- **Staff:** `GET /v1/admin/subscriptions?dunning=true`, `GET /v1/admin/renewals`,
  `GET /v1/admin/payment-attempts`, `GET /v1/admin/invoices?overdue=true`,
  `POST /v1/admin/invoices/{id}/waive-balance`, `…/mark-uncollectible`,
  `PATCH /v1/merchant/configuration`.
- **Customer:** `POST /v1/me/subscriptions/{id}/retry-now`,
  `POST /v1/me/invoices/{id}/pay` ([invoices and checkout](invoices.md)).
- **Notices:** `payment_method_failed`, `payment_method_update_required`,
  `invoice_overdue`, `invoice_collection_stopped`, `account_delinquent`,
  `account_delinquency_cleared`; host events `delinquency.*`
  (`billing.DelinquencyHostEvent`); findings `life.due_pass.refused`,
  `life.terminal_outcome.held`, `life.cadence.unknown`, `life.decline.unmapped`.

## Guarantees

### 1. Exactly-once provider calls
No pass calls a provider. Every retry is a `provider_intents` row
(`manual_rebill`, `subscription_collection`, `order_collection`) recorded before
its job, or a customer's request, runs it. An uncertain NMI submission is read,
never resent (`internal/intents/collection_resend.go`). A customer retry needs
an `Idempotency-Key` (reused elsewhere: `payment_idempotency_conflict`).

Checks:
- The due queries skip a row whose intent is `pending`, `in_flight`,
  `unknown_needs_verify` or `failed_retryable`.
- `DunningWorker.processSubscription` calls no provider: it enqueues, admits or
  applies a local transition (OR-J16-S32); the usage pass only enqueues (OR-J26-S53).
- OR-J26-S55: a refused collection keeps its nonexecution proof.

### 2. Agreement with providers
`lifecycle.RenewalDeclined` ignores a decline for a canceled row or another period. The completion handlers also require an unchanged failure count. A provider-run subscription is mirrored, not retried. Converge, the pull, the unknown cohort and NMI verification judge a provider-declared failure against the subscription's own window (`Policy.Window` of its cycle): its case policy's where OpenRails duns it, the built-in one where the provider retries. A row past its case end (`subscriptions.CaseEnd`) is not charged again: an NMI row parks `unverified`, a provider-billed row is asked by id, and an engine renewal whose period elapsed is refused (`life.due_pass.refused`). A delayed case still makes every attempt its policy allows: an engine retry admitted inside its case but sent late (its read before sending failed) is still sent until the period it pays for ends, and its remaining retries follow it, so a charge can land after the window.

Checks:
- `internal/intents/manual_rebill_completion.go` and `internal/modules/money/subscription_collection_intent.go` check the period end and `FailureCount`. Scenarios: OR-J16-S04, S30, S70 and S71.

### 3. Exact money
A retry charges its payload's frozen renewal terms through
`moneyutil.NativeToRailMinorExact`, which refuses inexact amounts. A usage
collection freezes its amount in `InvoiceCollectionPayload`. Delinquency sums
`total - amount_paid` per currency (`GetOverdueInvoiceAggregate` filters
`o.currency`).

Checks:
- The NMI and engine admissions convert with `NativeToRailMinorExact`.

### 4. Access control and tenant isolation
Staff reads need `merchant:customer:read`, write-offs
`merchant:customer:manage`, policy settings `merchant:config:manage`
(`Route.Needs`). `retry-now` needs the customer acting in person
(`actionCustomer`) and finds only that customer's subscription
(`GetUserSubscriptionByID`).

Checks:
- `ListDueDunningMerchants`, `ListOverdueRenewalMerchants`, `ListDueDelinquency`
  and `ListDueUsageCollections` return ids; work runs in `RunInMerchantScope`
  (OR-J16-S05).
- `GetCycleCardStanding` and `GetOverdueInvoiceAggregate` name `merchant_id`.

### 5. Contracts match the code
`billing.DunningPolicy`, `SubscriptionDunning`, `DeclineReason` and
`DelinquencyHostEvent` are in `api/go.txt`. The routes are in
`api/openapi.json` and the tables in `api/schema.txt`. A case stores its full
policy, so a changed default never alters an open case.

Checks:
- `DeclaredOf` writes every field, the transient ladder included (OR-J16-S53);
  `RecordedPolicyOf` reads a missing transient list as none.
- `internal/merchantarchive/contract/dunning_policy.go` validates them on restore.

### 6. Data lifecycle
In `internal/retention/tables.go`,
`payment_attempts` and `rebill_cycles` are `rows` (25 months), `subscriptions`
and `customer_delinquency` are `state`. No partitioned table is read. A
`customer_delinquency` row is never deleted, so its dedupe key never repeats.

Checks:
- Each table comment carries its `Retention:` sentence.
- A merchant restore calls `RebuildDelinquencyDue` and `MarkDelinquencyDue`.

### 7. Work scales with activity
Due work reads partial indexes in bounded cursor pages:
`subscriptions_grace_ends_at_idx`, `subscriptions_next_retry_at_rail_idx`,
`subscriptions_current_period_ends_at_engine_due_global_idx`,
`invoices_usage_collection_due_idx` and `customer_delinquency_due_idx`. A row left
unchanged leaves the due set until `due_recheck_at`.

Checks:
- `deferDue` waits half the overdue age, clamped to 1 minute to 1 hour.
- Scenarios: OR-J26-S02, OR-J26-S40 and OR-J04-S45.

### 8. Abuse protection
`retry-now` is in the `subscriptions` rate-limit bucket (`subscribe` limit, 20
rpm, captcha when configured). It checks the card-testing ledger before charging
(`refuseBlockedCardAttempt`) and counts a refusal (`noteCardDecline`).

Checks:
- `RetryMySubscriptionNow` answers `card_attempts_blocked` before any charge.
- OR-J16-S51 and S52: the retry caps hold the networks' reattempt limits.

### 9. Destructive-action safeguards
A terminal outcome needs a certainty leg and an open `destructive.Gate`. Under `limited`, the due pass holds terminal cancellations. A held engine case ends as a spent schedule on the first due pass after the merchant is armed (`EndHeldOutcome`); its finding closes when the case ends by any route. A held outcome keeps access and raises `life.terminal_outcome.held`, alerting the operator once (OR-J06-S70). Waiving and marking uncollectible run through `destructive.RunLocal` (`waive_order`, `mark_uncollectible`).
`subscription_status_transitions` records the actor of each status change.

Checks:
- The engine, NMI, converge and Solana crank callers pass `TerminalBlocked` from `destructive.Gate` (OR-J34-S39), for any decline, since spending the schedule is terminal too; a declared book holds it through `HoldTerminal` (OR-J34-S35).
- `ExpireAwaitingMethod` keeps a blocked row (OR-J16-S31).

## Tests

Journeys:
- OR-J16-S01 to S05 (schedule), S30 to S32, S70 to S72 (provider-seen declines, due work), S40 to S43 (do-not-retry), S50 to S53 (policy limits), S54, S55 (a failure's window is its own policy's, for its cycle).
- OR-J26-S01 to S04, S30, S40, S50, S51, S53, S55, S59 (usage collection, delinquency); the rest of J26 is in [usage billing](usage-billing.md).
- Related: OR-J04-S42 (`access_while_renewal_held: suspend`), OR-J04-S43, S45,
  S70, and OR-J12-S08 to S11 (expired-card refresh).

Other tests: `internal/decline`, `internal/modules/collection` and
`internal/modules/delinquency` have no unit tests; `critical/` covers them
(`TestDunning`, `TestDunningSchedule`, `TestDoNotRetry`, `TestDueWork`,
`TestUsageBillingContracts`, `TestNMICollectionRefusal`, `TestBillingPass`).

Gaps:
- No scenario sends advice on a declined usage-invoice collection: the Stripe fake has no invoice collection.
- Untested: `access_during_dunning: suspend`; the `life.decline.unmapped` and
  `life.cadence.unknown` findings; a Solana schedule spent while the merchant is disarmed.
- Nothing checks that `decline`'s `reasons` covers every
  `billing.DeclineReasons()` entry.
