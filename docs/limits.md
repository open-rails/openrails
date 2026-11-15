# Limits

Abuse limits on customer-facing routes, captcha escalation, the card-testing
ledger, and the merchant-wide budget for customer cancellations and card
removals. Staff destructive actions have their own
[admission policy](destructive-actions.md).

Counters, captcha challenges, card-testing declines and admin lockouts live in
Redis when configured, shared by every instance, otherwise in each process's
memory (one instance only); never in PostgreSQL. While a configured Redis does not
answer each process counts in memory, and what it counted still counts when Redis
returns.

## Rate limits

On by default: the server and `openrails.New` seed the built-in limits when
`rate_limits` and `captcha` are nil. `rate_limits_disabled: true` makes the
middleware a passthrough for a host whose gateway limits instead. Counting is per
1-minute window, per bucket and per subject (the client IP, and the user on
authenticated requests); a request is refused when any subject trips, and the
headers report the strictest.

| Bucket | Key | Default rpm | Routes |
|---|---|---|---|
| purchase | `purchase` | 10 | `POST /v1/me/orders`, an order's `/pay` and `/confirm`, `POST /v1/orders/{id}/solana-pay` |
| subscriptions | `subscribe` | 20 | `POST/PUT/DELETE /v1/me/subscriptions*` |
| payment methods | `payment` | 40 | `/v1/me/payment-methods*` |
| webhook | `webhook` | 1200 | `<prefix>/v1/webhooks/*` (a rail's webhooks share its source IPs) |
| catalog ask | `catalog-ask` | 10 | `POST /v1/console/catalog/ask` |

Other routes are not limited here: a generic per-address ceiling belongs to the
proxy in front. A bucket left out of `rate_limits` is unlimited, an unknown key is
reported and ignored, and a limit of 0 or less means 60. A pay URL is also limited
per order: 120 reads and 10 pays a minute. In-process Client calls count nothing.

The client IP is the socket peer unless `trusted_proxies` lists your load
balancer's CIDRs, in which case `X-Forwarded-For` is walked right to left past
trusted hops. Set it behind a proxy, or every client shares the proxy's bucket.

A declared `Content-Length` over the bucket's ceiling is refused `413` with
`Retry-After: 60` before counting (64 KiB for purchase, subscriptions and payment
methods; 1 MiB elsewhere). Webhooks have per-rail caps instead (CCBill 16 KiB, NMI
and Basis Theory 64 KiB, Stripe 256 KiB).

Responses carry `X-RateLimit-Limit`, `X-RateLimit-Remaining`,
`X-RateLimit-Reset` (epoch seconds) and `Retry-After` on `429` and `413`.

## Captcha

Setting `captcha.site_key` and `captcha.secret_key` enables it;
`captcha.provider` is `turnstile` (default), `recaptcha-v3` or `hcaptcha`.
Thresholds are fixed.

- A subject reaching 3× a bucket's limit in a window is challenged for 15 minutes.
  Only the purchase, payment-method and subscription buckets escalate; staff,
  console and server-to-server routes never see a captcha.
- A challenged request gets `403 captcha_required` with `X-Captcha-Required: true`
  (metadata: provider, site key, bucket) until it sends a valid
  `X-Captcha-Token`. A solve clears the challenge and its counters.
- In card-testing attack mode (below) every subject on that merchant's captcha
  buckets must solve one; each decline seen keeps it up another hour.
- Clients render the challenge from `GET /v1/config`'s `captcha` block or the
  error's metadata.

## Card testing

Card saves and purchase payments check declines before any provider call and
answer `429 card_attempts_blocked` with `Retry-After`. A request refused before a
provider call is not a decline.

- Per customer and per client address (IPv6: the /64): 6 declines in 15 minutes
  block for the window; 10 in 24 hours block for the day.
- Per merchant: 100 declines in 24 hours from at least 25 customers and 25
  addresses is attack mode, blocking any subject with a decline in the last 15
  minutes. A customer without a recent decline is never blocked, so a buyer can
  always add a card. Attack mode ends as declines age out.

## Customer cancellations and card removals

One merchant-wide budget, shared by every replica and app, covers
customer-requested subscription cancellations and stored-card removals. It is
enforced even with `rate_limits_disabled`.

- Rolling day: the larger of 10 distinct customers or 1 % of customers with a
  successful payment in the previous 30 days, rounded up.
- Rolling hour: the larger of 5 distinct customers or 1 per 400 recent paying
  customers. (4,000 paying customers: 10 an hour, 40 a day.)

A customer counts once per rolling day however many actions they take. Admission
is atomic in PostgreSQL (`user_destructive_reservations`, opaque ids only, pruned
after 24 hours) before any change; a refusal answers `429` with `Retry-After`,
records a finding and changes nothing. It opens no lasting hold. Renewals, expiry
and delayed provider cancellation do not spend it. A reservation is bound to the
payment method, or to the subscription and its lifecycle revision; idempotency
keys cannot rebind it.

```yaml
user_action_rate_limit:            # Go: UserActionRateLimit; zero fields use these
  minimum_customers_per_hour: 5
  paid_customers_per_hourly_customer: 400
  minimum_customers_per_day: 10
  paid_customers_per_daily_customer: 100
```
