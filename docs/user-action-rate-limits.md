# Customer cancellation and card-removal limits

OpenRails uses one merchant-wide budget for customer-requested subscription
cancellations and stored-card removals. Both apps and every backend replica
share it, independent of ordinary per-IP/user HTTP rate limits.

The rolling 24-hour limit is the larger of:

- ten distinct affected customers;
- one percent of customers with a successful positive payment in the previous
  30 days, rounded up.

The rolling-hour limit is the larger of five distinct customers or one per
400 recent paying customers (rounded up). For 4,000 recent paying customers,
the limits are ten per rolling hour and 40 per rolling day. There is no
per-minute limit. A burst is checked on every request; it cannot wait for the
hour to end before being detected. Failed payments,
registrations, zero-value transactions, refunds and future-dated payments do
not increase the paid-customer count. Currency and transaction amount do not
otherwise affect the calculation.

Multiple removals/cancellations for the same customer count once within the
rolling day. Admission is atomic in PostgreSQL before destructive work. A
refused request returns 429 and `Retry-After`, records an internal finding,
and makes no card/subscription change. No permanent hold is opened; a request
can be retried when enough distinct customers leave the window. Scheduled
renewal work, membership expiry and delayed provider cancellation do not
consume this customer-request budget.

Configuration (`UserActionRateLimit` in Go):

```yaml
user_action_rate_limit:
  minimum_customers_per_hour: 5
  paid_customers_per_hourly_customer: 400
  minimum_customers_per_day: 10
  paid_customers_per_daily_customer: 100
```

Zero fields use these defaults. Negative fields are invalid. This protection
is still enabled when `rate_limits_disabled` delegates HTTP limits to a host.

The server binds a reservation to its actual resource: a payment-method ID,
or a subscription ID and lifecycle revision. HTTP idempotency keys cannot
change or share that binding. Retrying the same action within 24 hours does
not count again. A resumed subscription has a new lifecycle revision, while
the same customer still occupies only one daily slot. Old failed actions
retried after their reservation expires must pass the current limit again.

Reservation rows contain opaque IDs, not contact information, and have no
cascading customer foreign key: deleting an account cannot free a slot. The
existing cleanup worker prunes expired rows in bounded batches after 24 hours.
The card, subscription and provider-operation records remain authoritative for
completed-action idempotency. Reservations are transient deployment state and
are not imported or reset by merchant restore.
