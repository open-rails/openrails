# Billing policies

Two real businesses cap different things.

An **API business** caps *outstanding owed*: a $200 credit line, $155 unpaid
means $45 more spend, deny at the cap. A **cloud business** caps *new spend per
window*: $2k a month regardless of unpaid prior invoices — nonpayment feeds
[delinquency](arrears-delinquency.md) and your own shutoff instead.

Both are measured from the double-entry ledger. Never from invoices (those are
presentation and collection artifacts, and they lag) and never from numbers you
send us.

| | Owner |
|---|---|
| Measuring the capped quantity | **OpenRails** |
| Enforcing it at admission | **OpenRails** |
| Which policy binds to which customer | **you** |
| What to do about delinquency | **you** |

A policy is a *named* object. A *binding* points a customer, a trust tier, or
the merchant default at a name. Rebinding is your runtime lever: it changes
which cap applies and moves no money.

## Kinds

| `kind` | Caps | Deny code when it bites |
|---|---|---|
| `outstanding_cap` | LEDGER-measured unpaid arrears. Outstanding owed is subtracted from the line, so $155 unpaid against $200 leaves $45. | `outstanding_cap_reached` |
| `window_spend_cap` | NEW spend per rolling window. Prior debt does **not** gate admission. | `budget_exceeded` |
| `accrual_rate_cap` | The measured accrual RATE, in micros **per hour** — the cloud quota. Prior debt does not gate it either. | `accrual_rate_cap_reached` |

`outstanding_cap_reached` is deliberately distinct from `insufficient_credit`
("this one request does not fit") and from `delinquent_unpaid_invoice` (the time
axis — the debt may not even be due yet). A host that cannot tell them apart
cannot say anything useful to the customer.

### The rate cap, and why it takes an input

`outstanding_cap` and `window_spend_cap` are questions about the past: what is
owed, what has been spent. `accrual_rate_cap` is a question about the future —
*how fast would money burn from now on* — and only you know what you are about
to start. So admission takes your **prospective delta**:

```json
{ "customer_id": "…", "estimated_amount": "1000", "expires_at": "2026-09-16T12:00:00Z", "accrual_rate_delta_per_hour": "2000000" }
```

OpenRails measures what is **already** accruing (rated usage over the policy's
lookback, scaled to an hour) and refuses when `measured + delta > cap`. A zero
delta means the request adds no ongoing rate, so it is gated only on what is
already running.

`accrual_rate_window_seconds` (default 3600, minimum 60) is the measurement
lookback. It changes the smoothing, never the unit: a short window reacts fast
and reads bursty, a long one is stable and forgives a spike.

**What it can see.** Only what has been reported. A deployment admitted seconds
ago has not accrued yet, so your own inventory is always ahead of this reading —
which is exactly why the delta is an input rather than something we try to
infer.

## Declaring

Manifest (mode 1), under the merchant:

```yaml
billing_policies:
  api_line:
    kind: outstanding_cap
    outstanding_cap: 200_000_000        # micros — $200
  cloud_monthly:
    kind: window_spend_cap
    spend_windows:
      - key: monthly
        window: 720h
        limit: 2_000_000_000            # micros — $2000
  cloud_quota:
    kind: accrual_rate_cap
    accrual_rate_cap_per_hour: 10_000_000   # micros/hour — $10/hour deployed
    accrual_rate_window: 15m                # measurement lookback (default 1h)
billing_policy_bindings:
  - policy: api_line                    # merchant default (no tier)
  - policy: cloud_monthly
    tier: cloud
```

API (mode 2), `PUT /v1/merchant/settings`:

```json
{
  "billing_policies": [
    { "name": "api_line", "kind": "outstanding_cap", "outstanding_cap_amount": "200000000" },
    { "name": "cloud_monthly", "kind": "window_spend_cap",
      "spend_windows": [{ "key": "monthly", "window_seconds": 2592000, "limit": "2000000000" }] }
  ],
  "billing_policy_bindings": [
    { "policy": "api_line" },
    { "policy": "cloud_monthly", "tier": "cloud" }
  ]
}
```

Both paths run the **same** validator, so a manifest that boots cannot declare a
policy the API would have refused. Each kind accepts only its own limit: putting
`spend_windows` on an `outstanding_cap` policy is an error, not a silently
ignored field.

Customer assignments use a separate runtime resource in both manifest and API
mode. They are excluded from `GET /v1/merchant/settings`; replacing that
merchant document preserves them. Removing a policy that is still assigned to a
customer is refused. `customer_id` is not accepted in a declaration binding.

### Assigning a customer

The same Go Client operations work embedded and remotely:

```go
name := "cloud_monthly"
assignment, err := client.SetCustomerBillingPolicy(ctx, customerID, &name)
assignment, err = client.GetCustomerBillingPolicy(ctx, customerID)
assignment, err = client.SetCustomerBillingPolicy(ctx, customerID, nil) // inherit again
```

HTTP uses `GET` and `PUT` on
`/v1/merchant/customers/{customer_id}/billing-policy`. PUT requires
`{"policy_name":"cloud_monthly"}` to assign, or `{"policy_name":null}` to clear.
Both operations return `{"customer_id":"…","policy_name":"cloud_monthly"}`;
an unassigned customer returns an explicit `null` policy name. This reads the
explicit assignment, not the effective tier or default policy.

GET requires `merchant:customer-settings:read`; PUT requires
`merchant:customer-settings:update`. Customer self-service permissions cannot
change this resource. The customer and policy must already exist under the
request's merchant: missing/foreign customers return `404 customer_not_found`,
and missing/foreign policies return `404 billing_policy_not_found`. No customer
is implicitly created. Missing, blank, non-string policy names and unknown
fields return `400 invalid_param`; only an explicit JSON `null` clears.

## Resolution

Most specific wins, one lookup:

```
per-customer binding  →  per-tier binding  →  merchant default  →  (none)
```

With no binding at all, admission falls back to the payer's own arrears credit
limit under `outstanding_cap` semantics — the reading that can still refuse.

Policy resolution reads PostgreSQL directly. A committed assignment changes the
next admission in every runtime, without waiting for a process-local cache TTL.

## Other policy fields

| Field | Applies to | Meaning |
|---|---|---|
| `bad_spend_windows` | any kind | Per-payer wasted-spend grace: at most `limit` of host-reported failed spend forgiven per window; overage is charged at report time. |
| `collection_threshold_amount` | any kind | When this payer's accrued arrears is invoiced. Overrides `invoice.collection_threshold` for payers bound here. |
| `delinquency_grace_days` / `delinquency_amount_floor` | any kind | This payer's [delinquency](arrears-delinquency.md) policy. Overrides the merchant-wide `invoice.delinquency_*` values. |
| `policy_currency` | any kind | Currency for checks whose window carries none. Blank means the request's currency. |

Collection and delinquency ride on every kind because *what a payer may owe or
spend* and *when its debt is chased* are separate questions — a cloud tenant's
debt still ages even though it never gates admission.

**`collection_cycle_boundary` is refused per-policy**, deliberately. A payer's
statement periods must tile its lifetime with no gap and no overlap, and
rebinding is a live runtime lever, so a mid-cycle change would bill a stretch
twice or never. It stays merchant-wide as `invoice.billing_period_boundary`.

Amounts are integers in the currency's native units (micros for USD).

Trust levels are assigned by the host or supplied explicitly with an admission request; deposits and paid spend never promote an account.
