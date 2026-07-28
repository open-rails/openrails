# Rate Limiting

OpenRails rate-limits on a fixed 1-minute window, per *bucket* (endpoint category) and per
*subject* (dimension). Every request is counted against each applicable subject and blocked when
**any** trips — headers reflect the strictest. Counters live in Redis when configured; a Redis
error falls back to a per-process in-memory counter for that check. One net/http middleware
(`RateLimitHTTP`, `internal/http/middleware/ratelimit_neutral.go`) serves both surfaces —
embedded `/billing/v1/...` paths are normalized to `/v1/...` before classification.

**On by default** (#742): standalone `config.Load` and `embedded.New` both seed the curated
defaults below whenever `rate_limits`/`captcha` are left nil. To opt out (your own gateway fronts
billing), set `rate_limits_disabled: true` (env `RATE_LIMITS_DISABLED`) — the middleware becomes
a pure passthrough. Host-facing summaries: [frontend-integration.md](frontend-integration.md),
[embedded-integration.md](embedded-integration.md).

## Subjects

| Subject | Key | Applies to |
|---|---|---|
| IP | `ip:<addr>` | all requests |
| User | `user:<user_id>` | authenticated requests (mount auth before the limiter) |

The IP is the proxy-aware resolved client (#746): with `trusted_proxies` empty (default) it is
the raw socket peer — a spoofed `X-Forwarded-For` has zero effect; with your LB's CIDRs
configured, `X-Forwarded-For` is walked right-to-left past trusted hops to the real client. Set
it whenever OpenRails sits behind a proxy, or all traffic collapses onto the proxy's one IP
bucket. Details: `trusted_proxies` in [operator-guide.md](operator-guide.md).

## Buckets

`ClassifyBucket` maps path + method; defaults (`rate_limits.<key>.requests_per_minute`):

| Bucket | Config key | Default rpm | Routes |
|---|---|---|---|
| `checkout` | `checkout` | 10 | `POST /v1/checkout*` only — see note |
| `subscriptions` | `subscribe` | 20 | `POST/PUT/DELETE /v1/me/subscriptions*` |
| `payment-methods` | `payment` | 40 | `/v1/me/payment-methods*` (any method) |
| `webhook` | `webhook` | 1200 | `/v1/webhooks*`, `/v1/merchants/*/webhooks/*` |
| `captcha` | — | unlimited | `/v1/captcha/status`, `/v1/captcha/client.js` |
| `default` | `default` | 300 | everything else |

A bucket with no configured limit falls back to the `default` entry; a configured limit ≤ 0
means 60 rpm.

> **Honest classification note**: the `checkout` bucket matches only the `/v1/checkout` path
> prefix (the public checkout surface). The browser self-service `POST /v1/me/checkout` and the
> customer-treasury `POST /v1/customers/{id}/checkout` are **not** classified as `checkout` —
> they land in `default` (300 rpm), so the tight card-testing limit does not apply to them.

> **Webhooks are per-IP, and all webhooks from a rail share one source-IP bucket** (fixed rail
> IPs). The high default absorbs rebill runs and event bursts without 429-ing payment events;
> webhooks are independently protected by signature verification, the IP allowlist, and per-rail
> body caps — this limit is a DoS floor, not the primary control.

## Payload-size shedding

Before any counting, a declared `Content-Length` over the bucket ceiling is rejected with `413`
(+ `Retry-After: 60`); the body is also wrapped in `MaxBytesReader` so chunked uploads are capped
on read. Ceilings: `checkout`/`subscriptions`/`payment-methods` 64 KiB, `default` 1 MiB.
Webhooks are deliberately absent — the handler enforces per-rail caps (CCBill 16 KiB, NMI 64 KiB,
Basis Theory 64 KiB, Stripe 256 KiB).

## Response headers

- `X-RateLimit-Limit`, `X-RateLimit-Remaining`, `X-RateLimit-Reset` (epoch seconds) reflect the
  strictest applicable subject.
- `Retry-After` (seconds) on `429` and on the `413` payload rejection.
- `X-Captcha-Required: true` on captcha challenges (`403`).

## Captcha escalation

Configuring `captcha.site_key` + `captcha.secret_key` IS the enablement — there is no separate
knob. `captcha.provider` is `turnstile` (default), `recaptcha-v3`, or `hcaptcha`; verify/script
URLs, thresholds, and TTLs are hardcoded policy, not config.

- A subject whose request count reaches **3×** its bucket limit within the window is marked
  challenged for **15 minutes**. Only the `checkout`, `payment-methods`, and `subscriptions`
  buckets escalate — but once challenged, the subject must solve on **any** `/v1` route except
  webhooks and the captcha endpoints.
- While challenged, requests get `403` with `X-Captcha-Required` and error code
  `captcha_required` (metadata carries provider, site key, bucket) until a valid token is sent in
  the `X-Captcha-Token` header. A successful solve clears the challenge and resets the
  challenged buckets' counters.
- A site-wide card-testing attack mode (#371) can require a solve from every subject regardless
  of individual counts; an individual solve never clears it.
- Clients poll `GET /v1/captcha/status` and load `/v1/captcha/client.js` — both exempt from
  limiting.
