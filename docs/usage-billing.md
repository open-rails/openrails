# Automatic usage billing

A host opts in with `usage_billing_enabled: true` (`Config.UsageBillingEnabled`).
It is off by default; subscription-only sites need no usage-billing jobs.
Orders a host creates and credit grants work either way.

Each merchant declares its defaults in its configuration:

```yaml
usage_billing:
  billing_day: 13
  currencies:
    - {currency: USD, credit_limit: 100000000, minimum_invoice_amount: 5000000}
```

The merchant billing day seeds new account schedules. Changing it leaves existing
account anchors unchanged; explicitly set or clear an account anchor to move it.
Credit limits and minimum amounts continue to inherit dynamically.

Amounts are integer native units: USD uses six decimal places, so this is a
$100 debt limit and a $5 minimum invoice. All dates use UTC. Days 29–31 clamp
to a shorter month's last day without permanently changing the anchor.

The customer can spend prepaid funds first. Spending credit requires their
currency's valid default payment method and collection agreement. The ledger
limits debt, including unpaid invoices; a failed payment does not reopen credit.
An admission that would exceed the remaining credit requests collection of the
existing balance and refuses the new work until payment frees capacity. Usage
reporting still records work already performed.

When debt reaches the limit, or the monthly date arrives, pending charges become
one usage order and the existing collection system attempts the default card.
The invoice threshold is normally the credit limit. An explicitly configured
legacy collection threshold can lower it, but never increase the debt limit.
Amounts below the minimum remain unbilled and carry into the next period. A
threshold payment never resets the monthly date. Existing invoice retries and
unknown-provider-result recovery continue independently.

`PATCH /v1/admin/customers/{customer_id}` customizes a customer:

```json
{
  "credit_limits": [{"currency": "USD", "amount": "75000000"}],
  "usage_billing": [{
    "currency": "USD",
    "billing_anchor": "2026-01-13T00:00:00Z",
    "minimum_invoice_amount": "1000000"
  }]
}
```

An omitted field stays unchanged. A null billing anchor restores the merchant's
billing day; a null minimum restores its default. `credit_limits` with amount
`"0"` explicitly disables credit; `{ "currency": "USD", "inherit": true }`
restores the merchant default. Existing database rows retain their explicit
limits during migration, including zero. Reads include `next_bill_at`.

Each account stores when it is next due: its bill date, or at once when
spending crosses its threshold, an admission asks to collect, or the
merchant's usage terms change. A threshold crossing also queues a pass in the
accrual transaction; otherwise one pass every five minutes reads the due
accounts across merchants, then the orders whose next charge is due, and
queues each charge as an `order_collection` operation that its own job runs.
An account with nothing pending has no bill date until it spends again. An
order whose customer has no card that can pay it waits until one is
designated. Account due records and invoice creation serialize with spending;
there are no per-account River timers and no once-per-thirty-day collection
guard.

Deployment order: deploy this code to every worker before enabling the feature.
The usage pass is the `openrails.usage_orders` job kind; OpenRails schedules
it, and a host never inserts it.
