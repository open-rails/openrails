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

`outstanding_cap_reached` is deliberately distinct from `insufficient_credit`
("this one request does not fit") and from `delinquent_unpaid_invoice` (the time
axis — the debt may not even be due yet). A host that cannot tell them apart
cannot say anything useful to the customer.

`accrual_rate_cap` is reserved and currently **refused** with a clear error.

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
billing_policy_bindings:
  - policy: api_line                    # merchant default (no tier)
  - policy: cloud_monthly
    tier: cloud
```

API (mode 2), `PUT /v1/merchant/settings`:

```json
{
  "billing_policies": [
    { "name": "api_line", "kind": "outstanding_cap", "outstanding_cap_amount": 200000000 },
    { "name": "cloud_monthly", "kind": "window_spend_cap",
      "spend_windows": [{ "key": "monthly", "window_seconds": 2592000, "limit": 2000000000 }] }
  ],
  "billing_policy_bindings": [
    { "policy": "api_line" },
    { "policy": "cloud_monthly", "tier": "cloud" },
    { "policy": "cloud_monthly", "customer_id": "…" }
  ]
}
```

Both paths run the **same** validator, so a manifest that boots cannot declare a
policy the API would have refused. Each kind accepts only its own limit: putting
`spend_windows` on an `outstanding_cap` policy is an error, not a silently
ignored field.

Per-customer binding is API-only. In manifest mode the YAML *is* the
configuration and changing it means a restart, which is the wrong shape for
per-customer segmentation — and it would put customer identifiers into a
committed file. `GET /v1/merchant/settings` likewise returns only the
declarative rungs.

## Resolution

Most specific wins, one lookup:

```
per-customer binding  →  per-tier binding  →  merchant default  →  (none)
```

With no binding at all, admission falls back to the payer's own arrears credit
limit under `outstanding_cap` semantics — the reading that can still refuse.

Resolutions are cached per process and invalidated on every policy or binding
write, so a tightened cap bites on the next admission rather than at the end of
a TTL.

## Other policy fields

| Field | Applies to | Meaning |
|---|---|---|
| `bad_spend_windows` | either kind | Per-payer wasted-spend grace: at most `limit` of host-reported failed spend forgiven per window; overage is charged at report time. |
| `policy_currency` | either kind | Currency for checks whose window carries none. Blank means the request's currency. |

All amounts are micros.
