# Dunning

How OpenRails retries a declined renewal it collects: engine memberships on
Stripe and NMI, Solana pulls, and every NMI schedule (`nmi_schedule`).
Provider-owned Stripe and CCBill subscriptions use the provider's own dunning.

## The schedule

The built-in schedule follows the price's billing cycle, so retries always end
inside one cycle. A merchant may replace it with its `dunning_policy` setting; a
case keeps the policy in force at its first decline. Retries are offsets from the
first failure:

| Cycle | Retries | Failures to terminal | Staleness window |
|---|---|---|---|
| under 4 days | none | 1 | min(24 h, cycle/2) |
| 4–27 days | +1 d, +2 d | 3 | 3 days |
| 28 days and longer | +2 d, +4 d, +6 d, +9 d | 5 | 10 days |

Counting the first failure as day 1, a monthly renewal is retried on days 3, 5,
7 and 10; the fifth failure ends the case and cancels the subscription.

A failure older than its staleness window (last offset plus min(24 h, cycle/2))
is never charged: it parks `unverified` with access kept. An unknown cycle (a
one-time price behind a membership) gets no schedule and raises
`life.cadence.unknown`. Each NMI decline costs a fee, which is why this is sparser
than Stripe's eight retries.

An answer that says to try again (`try_again_later`, `processing_error`,
`communication_error`, `issuer_unavailable`) is first retried 5 and 30 minutes
after the attempt. These transient retries are not failures on the schedule; once
they are spent the answer counts as one.

A `dunning_policy` keeps the built-in value of every field it leaves out: `tiers`
(the table above), `transient_retry_minutes` (5 and 30; an empty list means no
transient retries) and `customer_retries` (3).

`customer_retries` is how many declined attempts of the customer's own a renewal
takes on one card: at most 9, and 0 refuses any retry of a card the renewal
declined. A policy is refused (`invalid_param`) when one renewal's attempts on a
card could pass the card networks' limits. For each tier, its scheduled retries,
the transient retries and the customer retries total at most 15 (Visa allows 15
reattempts in 30 days), and the scheduled attempts inside any 24 hours, the first
failure included, plus the transient and customer retries at most 10
(Mastercard's limit for 24 hours). The built-in monthly tier uses 9 of the 15 and
6 of the 10; with the built-in tiers and transient retries `customer_retries` can
be at most 6.

What a decline does:

- **Retry on schedule:** soft issuer declines (insufficient funds, over limit,
  do-not-honor, call issuer, retry later) and gateway, processor or configuration
  errors.
- **Wait for a new card** (`awaiting_method`): bad card data, and codes the
  networks forbid retrying on the same card. The customer gets one
  `payment_method_update_required` notice; a new card resumes at the next due
  pass. The wait ends with the dunning window.
- **Terminal at once:** stolen or fraudulent cards and "stop recurring" codes.

The networks' advice comes before the rail's code. A decline that would be
retried waits for a new card instead when it carries Stripe's `advice_code`
`do_not_try_again` or `confirm_card_data`, a Mastercard Merchant Advice Code
03, 21 or 01 (Stripe's `network_advice_code`), or an issuer response code after
which Visa or Mastercard forbid another attempt on the card (04, 07, 12, 14, 15,
41, 43, 46, 54, 57, R0, R1, R3: Stripe's `network_decline_code`, or NMI's
`processor_response_code` when a Query API read of NMI's own scheduled charge
supplies it). Advice never ends a subscription. NMI's Direct Post answer carries
no issuer code, so a decline of OpenRails' own NMI charge is classified by its
response code alone.

A customer's own retry of the card (`retry-now`, or paying the renewal order with
the subscription's card) is refused with no provider call while the subscription
waits for a new card, unless the latest decline is one the cardholder can clear
on that card: `authentication_required`, `incorrect_zip` or `incorrect_address`.
In any state it is refused after a terminal decline held by the destructive
switch, after an issuer code above, and once the customer's declined attempts on
the card for the same renewal reach the case's `customer_retries`
([frontend](frontend.md#orders) has the refusals). Another card is always
admitted. Customer declines count toward the card-testing blocks.

Terminal failure cancels, revokes access and queues the provider delete. While the
kill switch is off a terminal outcome leaves the membership `past_due`.

On `nmi_schedule` NMI never retries, so its decline is the first failure; retries
count from when the decline happened, even if a provider read found it late.
Under a 96-hour cycle no retry is scheduled: NMI's next charge is the retry.

## Renewals

An engine renewal is an order (`origin: subscription`), one per period. Each try is
an attempt on it; between tries it is `open` and overdue past `due_at`
(`GET /v1/admin/orders?overdue=true`). The member can pay it with a card of their
choice. Ending the subscription voids it.

Each paid engine period is followed by an allowance of
min(24 h, max(2 h, period/10)) (1 h → 2 h, 7 d → 16 h 48 m, 30 d → 24 h): the member
keeps access until the renewal decides. The **due pass**, in each hourly billing
pass and at start, admits renewals whose period ended and retries whose
`next_retry_at` passed, each as its own provider operation. A subscription it
cannot process raises `life.due_pass.refused` and is looked at again after half
the time it has been due (1 minute to 1 hour).

**Held renewals.** A renewal with no outcome past its allowance is held because
collection is stopped (halted fleet, `engine_admission_hold`, breaker, readonly,
kill switch). The member keeps access unless
`dunning_policy.access_while_renewal_held` is `suspend`. `life.renewal.held`
reports the count and oldest age, and resolves when collection resumes. Issuer
authentication never completed closes after an hour for a first payment, after
the allowance for a renewal. An uncertain NMI submission is never resent; Stripe
may replay its original idempotency key twice inside the key's retention window
([provider uncertainty](provider-uncertainty.md)).

**Missed rebills.** An engine renewal not attempted within 1 h of its period
(Solana: 1 h 1 m) reads as missed in `GET /v1/admin/renewals` with why
(`method_unusable`, `refused`, `not_attempted`). Hourly, the rebill watch reads an
`nmi_schedule` renewal with no attempt 24 h after its period from NMI and raises
`life.rebill.missed` per missed cycle. If NMI holds no transaction for the cycle
and its schedule moved on (`provider_skipped`), OpenRails charges the period
itself. A voided or refunded sale (`provider_reversed`) or an unrecorded one
(`provider_unrecorded`) is never charged again. A failed look retries after
15 minutes, doubling to a day.

## Reading a case

`GET /v1/admin/subscriptions?dunning=true` lists open cases (`past_due`,
`awaiting_method`) with `dunning`: `attempts`, `retries_left`, `final_retry_at`,
`next_retry_at`, `waiting_for_new_card` and `last_failure_reason`. A
provider-owned case has no `retries_left` or `final_retry_at`.

## Payment health alerts

The hourly health pass keeps these findings open while their condition holds, per
PSP account:

| Finding | Fires when |
|---|---|
| `life.payments.new_card_decline_spike` | the last 24 h's new-card decline rate is 10 points above the previous 28 days, 50 attempts each side |
| `life.payments.rebill_failure_spike` | first-attempt rebill failures are 10 points above that baseline, or above 25 %, over 50 cycles |
| `life.payments.system_errors` | system errors exceed 2 % of the last hour (20 attempts or more); critical at once on NMI 410/411 |
| `life.decline.unmapped` | a decline code from the last 30 days that no table maps |
| `life.webhooks.silent` | a PSP that sent at least 10 charge webhooks in the previous 28 days sent none in 24 h while pulls found at least 3 |
