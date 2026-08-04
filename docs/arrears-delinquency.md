# Arrears delinquency

For billing in arrears (accrue through the period, pay at the end), someone
eventually stops paying. OpenRails answers the money half of that and hands you
the rest, deliberately:

| | Owner |
|---|---|
| Deciding a payer is delinquent, and when | **OpenRails** |
| Refusing new spend (admission) | **OpenRails** |
| Telling you about it, durably | **OpenRails** |
| Shutting off VMs, seats, storage, jobs, whatever you run | **you** |

OpenRails does not model your resources and never pretends to stop them. It also
**never revokes an entitlement** for an unpaid arrears bill: the service was
already consumed, retracting access does not recover the loss, and an automatic
revocation is exactly how a billing system costs a paying customer their access.

## Two axes, and why they are not one

A charge failing and a debt ageing are different questions with different
answers, and conflating them is the classic arrears bug.

- **Decline bucket** — *why did this charge fail* ⇒ what to do about the **card**.
  Time-independent. Retry, ask them to fix the card, or stop. See
  `internal/modules/collection`.
- **Delinquency** — *how long has this debt gone unpaid* ⇒ what to do about
  **service**. Amount- and time-based, independent of why any single charge
  failed.

An expired card is "fix the card" whether it expired today or in March, *and*
becomes delinquent if the bill stays unpaid past grace. A payer with no card on
file at all can be delinquent without a single decline. Neither state implies the
other.

## The state machine

Per `(merchant, payer, currency)`:

```
current ──overdue──► grace ──past the grace window, over the floor──► delinquent
   ▲                   │                                                  │
   └───────────────────┴──────────────── debt settled ────────────────────┘
```

- **current** — nothing overdue.
- **grace** — money is past its due date, but the grace window has not elapsed
  *or* the debt is below the merchant's amount floor. Visible to you; enforced
  against by nothing.
- **delinquent** — past grace and over the floor. New spend is refused and you
  are signalled.

Every state is **derived** from the payer's overdue open receivables
(`min(due_at)`, `sum(amount_due)` over `open`/`past_due` invoices) against the
merchant's policy. The stored row exists only to remember when a state started
and whether its transition has already been announced — recompute it any time
and you get the same answer.

A debt below the floor never escalates, however old: a rounding remnant on a
year-old invoice must not turn into a shutoff instruction.

## Policy (two knobs, both defaulted)

Manifest (mode 1), under the merchant's `invoice:` block:

```yaml
invoice:
  monthly_floor: 1_000_000          # existing: don't bother collecting below this
  delinquency_grace_days: 7         # or#878: days past due_at before delinquent
  delinquency_amount_floor: 5_000_000   # optional; defaults to monthly_floor
```

API (mode 2): `PUT /v1/merchant/settings` with `arrears_grace_days` /
`arrears_delinquency_floor`; `GET /v1/merchant/settings` returns them.

| Knob | Default |
|---|---|
| `delinquency_grace_days` | **14**. Deliberately generous: a merchant that never touched this knob has not thought about it, and being late to call someone delinquent costs a few days of accrual while being early costs a customer. `0` is a valid explicit choice — delinquent as soon as it is overdue. |
| `delinquency_amount_floor` | **derived from `monthly_floor`** (itself 1 currency unit). A debt you already declared too small to chase is too small to cut anyone off for. |

All amounts are micros.

## What OpenRails enforces: admission

A delinquent payer is refused at `/v1/merchant/admissions` with its **own** deny
code:

```json
{ "allowed": false, "blocked_by": "money", "deny_code": "delinquent_unpaid_invoice" }
```

Distinct from `insufficient_credit` on purpose: "you are over your limit" and
"you have an unpaid invoice" ask different things of the customer, and a host
that cannot tell them apart cannot say anything useful to either.

For usage billing this **is** the meaningful cutoff — refusing new spend is what
stops the bill growing. It revokes nothing and cancels nothing.

The gate fails open by construction. It refuses only when the recorded state says
delinquent **and** a live re-read of the invoices still agrees, so a payer who has
just settled is never held out by our evaluation lag.

## What you enforce: the signal

Delinquency transitions land on a durable, acknowledged feed —
`openrails.host_lifecycle_events`, the same shape as the payment-settlements feed.
Not a webhook: a missed cut-off signal is a revenue leak and a missed restore
signal is an outage for someone who has already paid.

| Event | Meaning |
|---|---|
| `delinquency.grace` | overdue, still inside the grace window — warn them |
| `delinquency.entered` | past grace: OpenRails has stopped admitting new spend. **Shut off what you run.** |
| `delinquency.cleared` | settled. **Restore it.** |

Payload: `from_state`, `to_state`, `overdue_amount`, `overdue_since`,
`overdue_invoices`, `grace_days`, `amount_floor`.

Embedded hosts:

```go
events, err := controlplane.ListPendingHostLifecycleEvents(ctx, app, merchantID, 100)
for _, ev := range events {
    if err := yourShutoff(ctx, ev); err != nil {
        continue // unacked events are redelivered
    }
    _ = controlplane.AcknowledgeHostLifecycleEvent(ctx, app, merchantID, ev.ID)
}
```

Ack **after** your own action is durable. An unacked event is redelivered; an
acked one is gone. Acked rows are pruned after 30 days; pending rows are never
pruned.

Re-announcing the same transition is a no-op — events carry a deterministic
dedupe key, so a re-run never instructs you to shut the same customer off twice.

## Reading the state

- `GET /v1/merchant/delinquency` — the overdue roster (grace + delinquent, oldest
  debt first) plus the effective policy it was judged against. `?state=delinquent`
  filters. Payers in good standing are never returned: it is an exception list,
  not a customer directory.
- `GET /v1/merchant/customers/:customer_id/delinquency` — one payer, per currency.
  An empty list means the payer has never been overdue.

Both are read-only. The state is a reading of invoice truth, so it is settled by
paying the invoice, never by an API call.

## Also notified

The payer gets an in-app notification on the two rungs it can act on:
`account_delinquent` and `account_delinquency_cleared`. Entering grace is silent —
the collection ladder has already told them the charge failed.

## Cadence

The evaluator runs every 15 minutes, driven by indexed due work (payers with an
overdue receivable, plus payers already parked non-current). It never enumerates
customers, and it runs in limited/readonly mode and with no charger armed,
because it moves no money.
