# Spend limits and delinquency

How much a customer may owe or spend (billing policies, checked at admission), and
what happens when an arrears bill goes unpaid (delinquency). OpenRails measures
both from the ledger, never from usage orders or numbers you send, and enforces
them at [admission](usage-billing.md#admissions). Which policy binds to whom, and
what to shut off when a customer is delinquent, are yours.

## Billing policies

A policy is a named object; a binding points the merchant default, a trust tier or
a customer at one. Rebinding moves no money and takes effect at the next admission
in every runtime.

| `kind` | Caps | Refusal |
|---|---|---|
| `outstanding_cap` | unpaid arrears from the ledger: $155 owed against $200 leaves $45 | `outstanding_cap_reached` |
| `window_spend_cap` | new spend per rolling window; prior debt does not gate it | `budget_exceeded` |
| `accrual_rate_cap` | the measured accrual rate, native units per hour | `accrual_rate_cap_reached` |

These differ from `insufficient_credit` (this request does not fit) and
`delinquent_unpaid_order` (an unpaid bill past grace), so a host can tell the
customer which it is.

`accrual_rate_cap` asks about the future, so admission takes your prospective
delta (`accrual_rate_delta_per_hour`): OpenRails measures what is already accruing
over `accrual_rate_window_seconds` (default 3600, minimum 60) and refuses when
measured + delta > cap. It sees only reported usage, which is why the delta is an
input. Work billed by the hour is a [running charge](running-charges.md).

```yaml
settings:
  billing_policies:
    - name: api_line
      kind: outstanding_cap
      outstanding_cap_amount: 200000000                                  # $200
    - name: cloud_monthly
      kind: window_spend_cap
      spend_windows:
        - { key: monthly, window_seconds: 2592000, limit: 2000000000 }  # $2000
    - name: cloud_quota
      kind: accrual_rate_cap
      accrual_rate_cap_per_hour: 10000000                                # $10/hour
      accrual_rate_window_seconds: 900
  billing_policy_bindings:
    - policy: api_line            # merchant default
    - policy: cloud_monthly
      tier: cloud
```

The same `settings` go through `PATCH /v1/merchant/configuration`, with the same
validator. Each kind takes only its own limit. A spend window is at most 31 days.
Bindings name no customer: a customer's policy is its `billing_policy` setting
(`PATCH /v1/admin/customers/{customer_id}` with `{"billing_policy": "cloud_monthly"}`,
`null` to inherit; `Client.UpdateCustomer` with `catalog.Value` or `catalog.Null`),
and removing a policy someone is assigned is refused.

Resolution: customer, then tier, then merchant default. With none, admission uses
the customer's own credit limit as an `outstanding_cap`.

| Field (any kind) | Meaning |
|---|---|
| `bad_spend_windows` | failed usage forgiven per window; past it, charged |
| `collection_threshold_amount` | when this customer's arrears become a usage order (overrides `collection_threshold`) |
| `delinquency_grace_days`, `delinquency_amount_floor` | this customer's delinquency policy (overrides `arrears_grace_days`, `arrears_delinquency_floor`) |
| `policy_currency` | currency for windows that carry none; blank is the request's |

`collection_cycle_boundary` is refused per policy: statement periods must tile a
customer's life, so it stays merchant-wide as `billing_period_boundary`. Trust
levels come from the host or the admission request; paying never promotes one.

## Delinquency

Per `(merchant, customer, currency)`:

```
current ──overdue──► grace ──past grace, over the floor──► delinquent
   ▲                   │                                       │
   └───────────────────┴─────────── debt settled ──────────────┘
```

The state is derived from the customer's open usage orders past `due_at` against
the merchant's policy; a debt below the floor never escalates. A card's decline
and a debt's age are separate axes: an expired card is [dunning](dunning.md)'s
problem whether or not the bill is late, and a customer with no card can be
delinquent without a decline.

```yaml
settings:
  monthly_floor: 1000000              # below this, nothing is collected
  arrears_grace_days: 7               # default 14; 0 means at once
  arrears_delinquency_floor: 5000000  # default monthly_floor (itself one currency unit)
```

**What OpenRails does.** A delinquent customer's admissions are refused
`delinquent_unpaid_order` (`blocked_by: money`), and only when a live re-read of
the orders agrees. It never revokes access or cancels anything for an arrears bill:
the service was consumed. Its overdue orders show `usage.delinquent`
(`GET /v1/admin/orders?origin=usage&overdue=true`). The customer gets
`account_delinquent` and `account_delinquency_cleared` notices; entering grace is
silent.

**What you do.** Read `delinquency.grace` (warn), `delinquency.entered` (shut off
what you run) and `delinquency.cleared` (restore it) from the [event log](events.md),
with payload `from_state`, `to_state`, `overdue_amount`, `overdue_started_at`,
`overdue_orders`, `grace_days`, `amount_floor` (`billing.DelinquencyHostEvent`).
Save your cursor after acting; a re-announced transition has the same id.

A customer is evaluated with every change to its usage orders, so paying, voiding
or writing one off clears it at once. Each stores the next instant time alone
changes its state, which the hourly billing pass evaluates; a policy change
re-evaluates the customers it affects. The pass moves no money, so it runs in every
write mode.
