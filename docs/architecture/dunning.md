# Collection and dunning

Collection decides what happens when a customer's card refuses money OpenRails
asked for. The decline table maps every rail code to one reason and action; the
retry schedule collects money owed (renewals OpenRails collects, usage orders);
arrears delinquency tracks how long a usage debt goes unpaid. Merchants set the
renewal schedule and the arrears grace periods; usage orders retry on the
built-in schedule. Renewal admission and the state machine are in
[subscriptions](subscriptions.md), usage rating in [usage billing](usage-billing.md).
Guides: [dunning](../client.md#subscriptions), [spend limits](../client.md#limits-admissions-and-the-gate).

## Concepts

- **Decline table:** `internal/decline`: rail code to `billing.DeclineReason`,
  its `billing.DeclineCause` (customer, issuer, fraud, merchant, processing or
  unknown, which fixes the `billing.AttemptOutcome`: declined, refused or
  error) and `Action` (`Retry`, `FixPaymentMethod`, `NonRecoverable`).
- **Dunning policy:** the `dunning_policy` setting (`billing.DunningPolicy`),
  resolved into a `collection.Policy`.
- **Case:** a `past_due` or `awaiting_method` membership, run under the policy
  it recorded at its first decline.
- **Window:** `Policy.Window`, the last offset plus min(24 h, cycle/2). Nothing
  older is charged.
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
  (`FailMembershipParams.RetryAfter`). The case then ends at
  `collection.Policy.CaseEnd`: the window's end, or the slack past the last
  retry the schedule will make, so a delayed case still makes every attempt
  its policy allows, but never past the next renewal's due time. A retry
  advised past it ends the case as exhausted, with cancel feedback
  `issuer_deferred`. A spent schedule is `lifecycle.DunningExhausted`.
- `FixPaymentMethod` moves the case to `awaiting_method`, sends
  `payment_method_update_required` (`payment_method_failed` when it queues a
  refresh of a possibly reissued card) and sets `grace_ends_at` to the window's end;
  an expired card may queue a refresh ([payment methods](payment-methods.md)).
- `NonRecoverable` or a spent schedule cancels, ends access and queues the
  provider cancel, given a certainty leg and an open `destructive.Gate` (a
  Solana pull checks no gate). Otherwise `lifecycle.TerminalHeld` keeps engine
  members `past_due`, makes NMI-schedule members `unverified`, and raises
  `life.terminal_outcome.held`.
- Access while dunning is a grace grant (`dunningAccess`), unless
  `access_during_dunning` is `suspend`.

**Due pass.** `riverjobs.DunningWorker` (`openrails.dunning`) runs in each
hourly `openrails.billing_pass`. It reads merchant ids
(`ListDueDunningMerchants`), then pages `ListDueDunningSubscriptions` inside
each merchant's `RunInMerchantScope`. A due row gets one action: an NMI retry
is enqueued (`ManualRebillHandler.EnqueueScheduled`); an engine renewal is
admitted ([subscriptions](subscriptions.md)); an expired wait ends
(`ExpireAwaitingMethod`); a stale row parks `unverified` (`ApplyLocalUnknown`);
or the provider is asked by id (`AskProviderPastGrace`). A second scan asks about
provider-billed renewals 48 hours overdue (`AskProviderOverdueRenewal`). A row
whose processing fails raises `life.due_pass.refused`; every row left unchanged
gets `due_recheck_at` from `deferDue`.

**Customer retries.** `POST /v1/me/subscriptions/{id}/retry-now` (NMI
schedules) and paying an engine renewal order with the subscription's card pass
`intents.AdmitCustomerRetry` before any provider call. From
`GetCycleCardStanding` it refuses (`ErrRetryNeedsNewMethod`) once the customer's
declines on the card reach `customer_retries`, after a non-recoverable decline
or forbidding issuer code, and while `awaiting_method` unless
`decline.HolderCanClear`. Another card is always admitted.

**Usage orders.** An `order_collection` operation charges a due order. On a
decline, `OrderCollectionHandler` applies `collection.FailureAction` with the
order's period as the cycle (at least 4 days); `usage.DeclineCollection` sets
`next_collection_attempt_at`, marks it `uncollectible`, or stops collecting.

**Delinquency.** `delinquency.EvaluateTx` runs in the transaction of every
usage-order change (`usage.Changed`), and from `riverjobs.DelinquencyWorker` at
`next_evaluation_at`. It reads `GetOverdueOrderAggregate` under the merchant
policy and the customer's billing-policy overrides; `delinquency.Classify`
decides the state. `UpsertCustomerDelinquency` returns the state before and
after, and `transition_seq` advances only on a change. A change writes a
`host_outbox` row deduplicated on `transition_seq` (`delinquency.grace`,
`.entered`, `.cleared`); entering or leaving `delinquent` also notifies the
customer. Admission refuses `delinquent_unpaid_order` only if `IsDelinquent`'s
live recompute agrees. Nothing is revoked or canceled.

Tables: `subscriptions` (`retry_attempts`, `transient_retries`,
`next_retry_at`, `grace_ends_at`, `dunning_policy`, `due_recheck_at`), `orders`
(`next_collection_attempt_at`), `payment_attempts`, `rebill_cycles`,
`customer_delinquency`, `provider_intents` and `reconciliation_findings`.

## Interfaces

- **Go:** `Client.ListSubscriptions` (`SubscriptionListParams.Dunning`, giving
  `billing.SubscriptionDunning`), `ListRenewals`, `ListPaymentAttempts`,
  `ListOrders`, `WaiveOrderBalance`, `MarkOrderUncollectible`, `ListFindings`,
  and `UpdateMerchantConfiguration` (`DunningPolicy`, `ArrearsGraceDays`,
  `ArrearsDelinquencyFloor`, `MonthlyFloor`).
- **Staff:** `GET /v1/admin/subscriptions?dunning=true`, `GET /v1/admin/renewals`,
  `GET /v1/admin/payment-attempts`, `GET /v1/admin/orders?overdue=true`,
  `POST /v1/admin/orders/{id}/waive-balance`, `…/mark-uncollectible`,
  `PATCH /v1/merchant/configuration`.
- **Customer:** `POST /v1/me/subscriptions/{id}/retry-now`,
  `POST /v1/me/orders/{id}/pay` ([orders and checkout](orders.md)).
- **Notices:** `payment_method_failed`, `payment_method_update_required`,
  `order_overdue`, `order_collection_stopped`, `account_delinquent`,
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
`lifecycle.RenewalDeclined` ignores a decline for a canceled row or another
period. The completion handlers also require an unchanged failure count. A
provider-run subscription is mirrored, not retried. A row past its window is
never charged: an NMI row parks `unverified`, a provider-billed row is asked by
id, and an engine renewal whose period elapsed is refused (`life.due_pass.refused`).

Checks:
- `manual_rebill_completion.go` and `subscription_collection_intent.go` check
  the period end and `FailureCount`. Scenarios: OR-J16-S04, S30, S70 and S71.

### 3. Exact money
A retry charges its payload's frozen renewal terms through
`moneyutil.NativeToRailMinorExact`, which refuses inexact amounts. A usage
collection freezes its amount in `OrderCollectionPayload`. Delinquency sums
`total - amount_paid` per currency (`GetOverdueOrderAggregate` filters
`o.currency`).

Checks:
- The NMI and engine admissions convert with `NativeToRailMinorExact`.

### 4. Access control and tenant isolation
Staff reads need `merchant:customer:read`, write-offs
`merchant:customer:manage`, policy settings `merchant:config:manage`
(`Route.Needs`). `retry-now` needs the customer acting in person
(`customerActionPayer`) and finds only that customer's subscription
(`GetUserSubscriptionByID`).

Checks:
- `ListDueDunningMerchants`, `ListOverdueRenewalMerchants`, `ListDueDelinquency`
  and `ListDueUsageCollections` return ids; work runs in `RunInMerchantScope`
  (OR-J16-S05).
- `GetCycleCardStanding` and `GetOverdueOrderAggregate` name `merchant_id`.

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
`orders_usage_collection_due_idx` and `customer_delinquency_due_idx`. A row left
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
A terminal outcome needs a certainty leg and an open `destructive.Gate` (a
Solana pull checks no gate). Under `limited`, the due pass holds the end of an
`awaiting_method` case. A held outcome keeps access and raises
`life.terminal_outcome.held`. Waiving and marking uncollectible run through
`destructive.RunLocal` (`waive_order`, `mark_uncollectible`).
`subscription_status_transitions` records the actor of each status change.

Checks:
- The engine, NMI and converge callers pass `TerminalBlocked` from
  `destructive.Gate`; the Solana crank (`solana/pull/crank.go`) passes none.
- `ExpireAwaitingMethod` keeps a blocked row (OR-J16-S31).

## Tests

Journeys:
- OR-J16-S01 to S05 (schedule), S30 to S32, S70, S71 (provider-seen declines,
  due work), S40 to S43 (do-not-retry), S50 to S53 (policy limits).
- OR-J26-S01 to S04, S30, S40, S50, S51, S53, S55 (usage collection,
  delinquency); the rest of J26 is in [usage billing](usage-billing.md).
- Related: OR-J04-S42 (`access_while_renewal_held: suspend`), OR-J04-S43, S45,
  S70, and OR-J12-S08 to S11 (expired-card refresh).

Other tests: `internal/decline`, `internal/modules/collection` and
`internal/modules/delinquency` have no unit tests; `critical/` covers them
(`TestDunning`, `TestDunningSchedule`, `TestDoNotRetry`, `TestDueWork`,
`TestUsageBillingContracts`, `TestNMICollectionRefusal`, `TestBillingPass`).

Gaps:
- No scenario declines a usage-order collection; that path uses
  `decline.Classify` (no advice) and the built-in schedule, not `dunning_policy`.
- Untested: `access_during_dunning: suspend`; the `life.decline.unmapped` and
  `life.cadence.unknown` findings; a Solana schedule spent with the switch off.
- The floors (`arrears_delinquency_floor`, `monthly_floor`, `DefaultAmountFloor`
  1,000,000) are one number across currencies, one unit only at 6 decimals.
- Converge judges a failed renewal against the built-in window
  (`reconcile.DefaultDunningWindow`), not the merchant's `dunning_policy`.
- Nothing checks that `decline`'s `reasons` covers every
  `billing.DeclineReasons()` entry.
