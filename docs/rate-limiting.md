# Rate Limiting

OpenRails rate-limits requests on a fixed 1-minute window. Limits are enforced
per *bucket* (endpoint category) and across multiple *subjects* (dimensions)
simultaneously — a request is throttled if **any** applicable subject is over its
limit. Counters live in Redis when configured, with an in-memory fallback.

Implementation: [`internal/http/middleware/ratelimit.go`](../internal/http/middleware/ratelimit.go).

## Subjects (dimensions)

Every request is counted against each subject that applies to it:

| Subject  | Key                  | Applies to                | Limit        |
|----------|----------------------|---------------------------|--------------|
| IP       | `ip:<addr>`          | All requests              | Bucket limit |
| User     | `user:<user_id>`     | Authenticated requests    | Bucket limit |

- **IP** uses the socket peer address (`RemoteAddr`), not `X-Forwarded-For`, so it
  cannot be spoofed by a client header. Run behind a trusted proxy that sets the
  real peer address if terminating TLS upstream.
- **User** keys on the authenticated JWT `user_id`, so a single account is limited
  even as it rotates IPs.

## Buckets (endpoint categories)

Buckets are classified by path + method in `classifyBucket`. Defaults
(`config.yaml` → `rate_limits`, requests per minute):

| Bucket            | Config key   | Default rpm | Routes                                   |
|-------------------|--------------|-------------|------------------------------------------|
| `checkout`        | `checkout`   | 10          | `POST /v1/checkout*`                      |
| `subscriptions`   | `subscribe`  | 20          | `POST/PUT/DELETE /v1/me/subscriptions*`   |
| `payment-methods` | `payment`    | 40          | `/v1/me/payment-methods*`                 |
| `webhook`         | `webhook`    | 1200        | `/v1/webhooks/*`                          |
| `default`         | `default`    | 300         | everything else under `/v1/*`             |
| `captcha`         | —            | unlimited   | `/v1/captcha/status`, `/v1/captcha/client.js` |

> **Webhooks are per-IP, and all webhooks from a rail share one source-IP
> bucket** (fixed rail IPs). The high `webhook` default exists so rebill runs
> and event bursts are never throttled into 429s (which would delay payment/
> entitlement processing). Webhooks are independently protected by signature
> verification, the IP allowlist, and per-rail body caps — the rate limit is
> a DoS floor, not the primary control. Because it is per-IP, raising it does not
> meaningfully help an attacker (each attacker IP is still capped on its own).

## Payload-size throttling

Before any counting or body read, requests are rejected early with
`413 Payload Too Large` when the declared `Content-Length` exceeds the bucket
ceiling (`bucketMaxContentLength`):

| Bucket            | Max Content-Length |
|-------------------|--------------------|
| `checkout`        | 64 KiB             |
| `subscriptions`   | 64 KiB             |
| `payment-methods` | 64 KiB             |
| `default`         | 1 MiB              |

Webhook routes are intentionally absent here — they enforce per-rail caps
(CCBill 16 KiB, NMI 64 KiB, Stripe 256 KiB) directly in the webhook handler, on
top of the global 1 MiB body limit.

## Response headers

- `X-RateLimit-Limit`, `X-RateLimit-Remaining`, `X-RateLimit-Reset` reflect the
  strictest applicable subject.
- `Retry-After` (seconds) is set on `429` and on the `413` payload rejection.

## Captcha escalation

When captcha is enabled, an IP/user that blows past an *extreme* multiple of its
limit is challenged; subsequent requests must carry a valid `X-Captcha-Token`
until the challenge TTL expires. See [`internal/captcha`](../internal/captcha).
