# Rate Limiting

OpenRails rate-limits on a fixed 1-minute window, per *bucket* (endpoint category) and per
*subject* (dimension). Every request is counted against each applicable subject and blocked when
**any** trips — headers reflect the strictest. Counters live in Redis when configured; a Redis
error falls back to a per-process in-memory counter for that check. One net/http middleware
(`RateLimitHTTP`, `internal/http/middleware/ratelimit_neutral.go`) serves both surfaces —
embedded `/billing/v1/...` paths are normalized to `/v1/...` before classification.

**On by default**: the standalone loader and `openrails.New` both seed the curated
defaults below whenever `rate_limits`/`captcha` are left nil. To opt out (your own gateway fronts
billing), set `rate_limits_disabled: true` (env `RATE_LIMITS_DISABLED`) — the middleware becomes
a pure passthrough. Host-facing summaries: [frontend-integration.md](frontend-integration.md),
[embedded-integration.md](embedded-integration.md).

## Subjects

| Subject | Key | Applies to |
|---|---|---|
| IP | `ip:<addr>` | all requests |
| User | `user:<user_id>` | authenticated requests (mount auth before the limiter) |

The IP is the proxy-aware resolved client: with `trusted_proxies` empty (default) it is
the raw socket peer — a spoofed `X-Forwarded-For` has zero effect; with your LB's CIDRs
configured, `X-Forwarded-For` (every header line, joined in order) is walked right-to-left past
trusted hops to the real client. Set
it whenever OpenRails sits behind a proxy, or all traffic collapses onto the proxy's one IP
bucket. Details: `trusted_proxies` in [operator-guide.md](operator-guide.md).

## Buckets

`ClassifyBucket` maps path + method; defaults (`rate_limits.<key>.requests_per_minute`):

| Bucket | Config key | Default rpm | Routes |
|---|---|---|---|
| `checkout` | `checkout` | 10 | Browser checkout mint/pay POSTs — see note |
| `subscriptions` | `subscribe` | 20 | `POST/PUT/DELETE /v1/me/subscriptions*` |
| `payment-methods` | `payment` | 40 | `/v1/me/payment-methods*` (any method) |
| `webhook` | `webhook` | 1200 | `<prefix>/v1/webhooks/*` |
| `metrics-ask` | `metrics-ask` | 10 | `POST /v1/merchant/metrics/ask` |
| `catalog-ask` | `catalog-ask` | 10 | `POST /v1/merchant/catalog/ask` (including drafting) |
| `dashboard-generate` | `dashboard-generate` | 10 | `POST /v1/merchant/dashboard/widgets/generate` |
| `captcha` | — | unlimited | `/v1/captcha/status`, `/v1/captcha/client.js` |

Every other route is unlimited here: a generic per-address ceiling belongs to the proxy in front
of OpenRails (Traefik). A bucket left out of `rate_limits` is not limited, any other key refuses
boot, and a configured limit ≤ 0 means 60 rpm.

AI features use these same per-IP and authenticated-user buckets. There is no
separate per-merchant daily quota. Each feature has its own counter; catalog
drafting tools are part of the one catalog question. Consent and per-request
model/tool limits still apply. These are HTTP abuse limits on both standalone
and embedded routes; trusted in-process Client operations follow the normal
library boundary and do not consume HTTP counters.

> The `checkout` bucket covers POSTs to and under `/v1/me/checkout-sessions`
> and under `/v1/checkout-sessions/` and `/v1/checkout-attempts/`. Read-only GETs are not
> limited. A checkout session is also limited per session id, whatever address
> presents it: 120 reads and 10 pays a minute.

> **Webhooks are per-IP, and all webhooks from a rail share one source-IP bucket** (fixed rail
> IPs). The high default absorbs rebill runs and event bursts without 429-ing payment events;
> webhooks are independently protected by signature verification, the IP allowlist, and per-rail
> body caps — this limit is a DoS floor, not the primary control.

## Payload-size shedding

Before any counting, a declared `Content-Length` over the bucket ceiling is rejected with `413`
(+ `Retry-After: 60`); the body is also wrapped in `MaxBytesReader` so chunked uploads are capped
on read. Ceilings: `checkout`/`subscriptions`/`payment-methods` 64 KiB; every other route has the 1 MiB request body limit.
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
  buckets escalate or ask for a solve. Merchant, console and server-to-server API routes never
  meet a captcha.
- While challenged, requests get `403` with `X-Captcha-Required` and error code
  `captcha_required` (metadata carries provider, site key, bucket) until a valid token is sent in
  the `X-Captcha-Token` header. A successful solve clears the challenge and resets the
  challenged buckets' counters.
- Card-testing attack mode is per merchant and decided by the ledger below. While it
  holds, every subject on that merchant's captcha buckets must solve a captcha; each decline
  seen in attack mode keeps it up for another hour, so it lapses an hour after the declines stop
  or the ledger's window drops below the threshold. It applies where the merchant is known
  before authentication (the configured merchant or its `api_host`); an individual solve never
  clears it. Without a captcha configured, nothing is challenged and the ledger's blocks are the
  whole policy.
- Clients poll `GET /v1/captcha/status` and load `/v1/captcha/client.js` — both exempt from
  limiting.

## Card-testing ledger

Cards the provider refused are counted in PostgreSQL
(`billing.card_attempt_failures`), so blocks hold on every replica without
Redis or captcha. A request refused before any provider call (a
missing field, an unconfigured PSP) is not a decline. Card saves, checkout
creation and confirmation (browser routes and the embedded or remote Client)
check it before any provider call and answer `429` `card_attempts_blocked`
with `Retry-After`:

- per customer and per client address (an IPv6 client is its /64; through the
  Client, the `customer.client_ip` the host supplies): 6 declines in 15
  minutes block for the window; 10 in 24 hours block for the day;
- per merchant: 100 declines in the last 24 hours from at least 25 customers
  and 25 addresses is attack mode, where any subject with a decline in the
  last 15 minutes is blocked. Customers with no recent decline are never
  blocked by it, so a buyer can always add a card, for example to recover a
  past-due membership. The window slides: attack mode ends as declines age
  out.

Redis, when configured, remains the captcha accelerator above.
