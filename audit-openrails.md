# HTTP API Design Audit — authkit + openrails
*2026-06-18 · SiegePal / Shinobi-Online*

---

## Overview

This document audits the HTTP API layer of both codebases — route design, request/response shapes, auth middleware, and how well the current surface fits the multi-merchant SaaS model. The codebase was originally designed for a single-tenant, single-merchant deployment; it has been significantly extended toward multi-tenancy, but several single-tenant assumptions remain baked into the API surface.

The audit is structured as: full route inventory → auth + middleware chain → request/response shapes → design issues by severity.

---

## openrails — Full Route Inventory

### Route surfaces and URL prefixes

| Prefix | Auth | Purpose |
|--------|------|---------|
| `GET /health`, `/healthz`, `/readyz` | None | Health/liveness checks |
| `/v1/products`, `/v1/prices`, `/v1/checkout/*`, `/v1/me/*`, `/v1/stripe/*` | Optional/Required AuthKit JWT | End-user billing (user routes) |
| `/v1/admin/*` | Required + `openrails:admin` (live check) | Per-merchant operator admin |
| `/v1/merchant/catalog/*` | Service token OR admin + `catalog:write` | Merchant catalog management |
| `/v1/service/*` | Service token (pinned merchant) | Server-to-server billing API |
| `/v1/self/*` | Delegated access token (pinned merchant) | Browser-direct self-service |
| `/v1/merchant-admin/*` | Delegated access token + `openrails:merchant:*` | Browser-direct merchant admin |
| `/v1/webhooks/:provider` | Signature verification (configured merchant) | Global inbound webhooks |
| `/v1/m/:merchant/webhooks/:provider` | Signature verification (slug-resolved merchant) | Per-merchant inbound webhooks |
| `/v1/admin/merchants/*` | Required + `openrails:admin` (live check) | Platform: merchant provisioning |
| `/v1/platform/*` | Required + `openrails:platform:superadmin` | Platform superadmin (cross-merchant) |
| `/auth/*` | Selective AuthKit route groups | Login, token, user, orgs |

### User routes — `/v1/*`

```
GET    /products
GET    /prices
GET    /solana/config
GET    /solana/tokens
POST   /checkout                          (required)
GET    /checkout/:id                      (required)
POST   /checkout/:id/confirm              (required)
GET    /checkout/:id/solana-pay
POST   /checkout/:id/solana-pay
POST   /solana/recurring/enroll           (required)
GET    /me/status                         (required)
GET    /me/subscriptions                  (required)
GET    /me/subscriptions/:id              (required)
PUT    /me/subscriptions/:id/payment-method (required)
POST   /me/subscriptions/:id/cancel       (required)
POST   /me/subscriptions/:id/resume       (required)
POST   /me/subscriptions/:id/change-tier  (required)
POST   /me/subscriptions/:id/change-tier/preview (required)
GET    /me/payments                       (required)
GET    /me/payment-methods                (required)
POST   /me/payment-methods                (required)
PUT    /me/payment-methods/:id            (required)
DELETE /me/payment-methods/:id            (required)
GET    /me/notifications                  (required)
GET    /me/notifications/unread-count     (required)
POST   /me/notifications/:id/read         (required)
GET    /me/credits                        (required)
GET    /me/credits/:currency              (required)
GET    /me/credits/:currency/transactions (required)
GET    /me/products                       (required)
GET    /me/products/:product_id/access    (required)
POST   /stripe/portal                     (required)
```

### Admin routes — `/v1/admin/*`

```
GET    /subscriptions
GET    /subscriptions/:id
POST   /subscriptions/:id/cancel
GET    /payments
GET    /payments/:id
POST   /payments/:id/refund
GET    /users/:user_id/payments
POST   /users/:user_id/payments/off-channel
GET    /repair-alerts
GET    /manual-rebill-attempts
GET    /intents
GET    /users/:user_id
GET    /users/:user_id/entitlements
GET    /users/:user_id/nmi
GET    /users/:user_id/nmi/metrics
GET    /users/:user_id/ccbill
GET    /users/:user_id/ccbill/metrics
POST   /users/:user_id/entitlements
DELETE /users/:user_id/entitlements/:id
GET    /users/:user_id/product-access
POST   /users/:user_id/product-access
DELETE /users/:user_id/product-access/:id
GET    /metrics/summary
GET    /metrics/revenue
GET    /metrics/subscriptions
GET    /metrics/processors
GET    /metrics/churn
POST   /reconcile/runs
GET    /reconcile/runs
GET    /reconcile/runs/:id
GET    /reconcile/findings
POST   /reconcile/findings/:id/ack
POST   /reconcile/findings/:id/dismiss
GET    /catalog/features                  (entitlement features)
POST   /solana/recurring/plans
```

### Merchant action routes — `/v1/merchant/catalog/*`

```
POST   /products
GET    /products
GET    /products/:id
GET    /products/by-slug/:slug
PATCH  /products/:id
POST   /products/:id/activate
POST   /products/:id/deactivate
POST   /products/:id/reconcile
POST   /prices
GET    /prices
GET    /prices/:id
PATCH  /prices/:id
POST   /prices/:id/activate
POST   /prices/:id/deactivate
POST   /prices/:id/reconcile
GET    /drift
POST   /drift/refresh
POST   /drift/reconcile-all
GET    /orphans
GET    /stripe/orphans
```

### Service routes — `/v1/service/*`

```
POST   /admit                             (credits:write + credits:spend)
POST   /admit/batch                       (credits:write + credits:spend)
GET    /budget                            (credits:read)
PUT    /payer-spend-limits                (credits:write)
PUT    /tier-schedules                    (credits:write)
PUT    /merchant-configuration            (credits:write)
GET    /tier                              (credits:read)
POST   /wasted-spend                      (credits:write)
GET    /abuse-usage                       (credits:read)
PUT    /credit-limit                      (admin)
GET    /credit-limit                      (credits:read)
PUT    /invoker-spend-limits              (credits:write)
GET    /invoker-spend-limits              (credits:read)
POST   /customers/by-external-subject/entitlements  (entitlements:read)
GET    /customers/:customer_id/entitlements         (entitlements:read)
GET    /users/:user_id/product-access               (entitlements:read)
GET    /invokers/:invoker/credits                   (credits:read)
POST   /credits/windows                   (credits:write + credits:spend)
POST   /credits/settle                    (credits:write + credits:spend)
POST   /credits/windows/:id/refill        (credits:write + credits:spend)
POST   /credits/windows/:id/close         (credits:write)
GET    /credits/balance                   (credits:read)
POST   /credits/deposit                   (credits:write)
POST   /credits/withdraw                  (credits:write)
POST   /credits/holds/:id/capture         (credits:write + credits:spend)
POST   /credits/holds/:id/release         (credits:write)
POST   /credits/hold/:id/capture          (credits:write + credits:spend)  ← duplicate route (singular/plural)
POST   /credits/hold/:id/release          (credits:write)                  ← duplicate route
POST   /credits/usage/rollup              (credits:read)
POST   /credits/usage/resource-revenue    (credits:read)
GET    /credits/transactions/lookup       (credits:read)
GET    /credits/invokers/:invoker         (credits:read)                   ← duplicate of /invokers/:invoker/credits
PUT    /credits/account-settings          (credits:write)
GET    /credits/account-settings          (credits:read)
GET    /credits/transactions              (credits:read)
```

### Self-service routes — `/v1/self/*`

```
GET    /account                           (billing:read)
PUT    /account/settings                  (billing:write)
GET    /account/transactions              (billing:read)
GET    /status                            (billing:read)
GET    /credits                           (billing:read)
GET    /credits/:currency                 (billing:read)
GET    /credits/:currency/transactions    (billing:read)
GET    /usage                             (billing:read)
GET    /invoices                          (billing:read)
GET    /invoices/:id                      (billing:read)
GET    /payments                          (billing:read)
GET    /entitlements/active               (billing:read)
GET    /subscriptions                     (billing:read)
GET    /subscriptions/:id                 (billing:read)
POST   /subscriptions/:id/cancel          (subscription:cancel)
POST   /subscriptions/:id/resume          (subscription:cancel)
POST   /subscriptions/:id/change-tier     (subscription:cancel)
POST   /subscriptions/:id/change-tier/preview (subscription:cancel)
PUT    /subscriptions/:id/payment-method  (payment-methods:manage)
POST   /subscriptions/:id/solana-cancel-tx (subscription:cancel)
POST   /subscriptions/:id/solana-cancel   (subscription:cancel)
POST   /subscriptions/:id/solana-tier-change (subscription:cancel)
POST   /subscriptions/:id/solana-tier-change/confirm (subscription:cancel)
GET    /payment-methods                   (billing:read)
POST   /payment-methods                   (payment-methods:manage)
PUT    /payment-methods/:id               (payment-methods:manage)
DELETE /payment-methods/:id               (payment-methods:manage)
GET    /wallets/solana                    (billing:read)
PUT    /wallets/solana                    (wallets:manage)
DELETE /wallets/solana                    (wallets:manage)
POST   /checkout                          (checkout:create)
GET    /checkout/:id                      (billing:read)
POST   /checkout/:id/confirm              (checkout:create)
GET    /usdc-funding-options              (billing:read)
POST   /usdc-funding-sessions             (checkout:create)
GET    /usdc-funding-sessions/:id         (billing:read)
```

### Merchant-admin routes — `/v1/merchant-admin/*`

```
GET    /metrics/summary                   (merchant:billing:read)
GET    /metrics/revenue                   (merchant:billing:read)
GET    /metrics/subscriptions             (merchant:billing:read)
GET    /metrics/processors                (merchant:billing:read)
GET    /metrics/churn                     (merchant:billing:read)
GET    /secrets                           (merchant:secrets:list)
GET    /secrets/registry                  (merchant:secrets:list)
PUT    /secrets/*name                     (merchant:secrets:write)
DELETE /secrets/*name                     (merchant:secrets:delete)
POST   /secrets/validate/*name            (merchant:secrets:test)
GET    /repair-alerts                     (merchant:billing:read)
GET    /manual-rebill-attempts            (merchant:billing:read)
GET    /users/:user_id                    (merchant:billing:read)
GET    /users/:user_id/payments           (merchant:billing:read)
GET    /users/:user_id/entitlements       (merchant:billing:read)
GET    /users/:user_id/nmi                (merchant:billing:read)
GET    /users/:user_id/nmi/metrics        (merchant:billing:read)
GET    /users/:user_id/ccbill             (merchant:billing:read)
GET    /users/:user_id/ccbill/metrics     (merchant:billing:read)
POST   /users/:user_id/entitlements       (merchant:entitlements:write)
DELETE /users/:user_id/entitlements/:id   (merchant:entitlements:write)
POST   /users/:user_id/payments/off-channel (merchant:payments:write)
POST   /payments/:id/refund               (merchant:payments:write)
GET    /subscriptions                     (merchant:billing:read)
GET    /subscriptions/:id                 (merchant:billing:read)
POST   /subscriptions/:id/cancel          (merchant:subscriptions:write)
```

### Platform superadmin — `/v1/platform/*`

```
GET    /merchants                         (platform:superadmin)
GET    /merchants/:id                     (platform:superadmin)
GET    /search                            (platform:superadmin)
GET    /metrics                           (platform:superadmin)
```

### Merchant provisioning — `/v1/admin/merchants/*`

```
POST   /                                  (admin: provision new merchant)
GET    /:id
POST   /:id/suspend
POST   /:id/resume
POST   /:id/tier
POST   /:id/export
POST   /:id/delete
GET    /:id/credentials
PUT    /:id/credentials/*name
DELETE /:id/credentials/*name
POST   /:id/credentials/validate/*name
POST   /:id/credentials/test-stripe
```

### Webhooks

```
POST   /v1/webhooks/:provider                      (global: configured merchant)
POST   /v1/m/:merchant/webhooks/:provider          (per-merchant: slug resolved)
```

---

## openrails — Auth + Middleware Chain

```
Global (every request):
  1. gin.Recovery()
  2. gin.Logger()                       (skips health paths)
  3. ginmw.SecurityHeaders()            (CORS, CSP, X-Frame-Options)
  4. ginmw.CORS(configured origins ∪ merchant CORS)
  5. ginmw.BodyLimit(10MB)
  6. ginmw.ResolveMerchant(configuredMerchant)   ← SINGLE-TENANT LEGACY
  7. authProvider.Optional()            (best-effort AuthKit JWT validation)
  8. ginmw.RateLimitWithChallengeStore()

Per-surface middleware:
  User routes:     optional auth (no 401)
  Admin routes:    required auth → AdminPermissionRequired(openrails:admin live check) → MerchantDBConn(RLS)
  Merchant action: ServiceTokenRequired OR admin + PermCatalogWrite → MerchantDBConn(RLS)
  Service routes:  ServiceTokenRequired (merchant pinned from token) → MerchantDBConn(RLS) → RequireServiceTokenPermission(perm)
  Self routes:     DelegatedSelfRequired (merchant pinned from token) → MerchantDBConn(RLS) → RequireDelegatedPermission(perm)
  Webhook:         Signature verification after merchant resolution
  Merchant-admin:  DelegatedPrincipalRequired → MerchantDBConn(RLS) → RequireDelegatedPermission(perm)
  Platform:        Required auth → PlatformSuperadminRequired(platform org)
  Merchant provn:  Required auth → AdminPermissionRequired(openrails:admin)
```

---

## openrails — Key Request/Response Shapes

### `POST /v1/service/admit`
```json
// Request
{
  "customer_id": "uuid",
  "invoker": "user@example.com",
  "invoker_type": "user",
  "tier": "pro",
  "resource": "model:gpt-4",
  "currency": "USD",
  "estimated_amount": 50000,
  "request_id": "req_abc123",
  "expires_at": 1735689600,
  "roles": ["uuid1", "uuid2"]
}

// Response: 200 (allowed) | 402 (money) | 429 (abuse) | 403 (other block)
{
  "allowed": true,
  "blocked_by": null,
  ...
}
```

### `POST /v1/admin/merchants` (provision)
```json
// Request (merchants.ProvisionRequest)
{ "slug": "acme", "name": "Acme Corp", "owner_org_id": "...", "billing_tier": "...", "region": "us" }

// Response
{
  "id": "uuid", "slug": "acme", "name": "Acme Corp",
  "status": "active", "owner_org_id": "...", "billing_tier": "...",
  "region": "us", "webhook_host": "...", "webhook_path": "..."
}
```

### `POST /v1/self/checkout`
```json
// Request
{ "items": [{"price_id": "price_abc", "quantity": 1}] }
// Response: checkout session object
```

### Error responses (inconsistent)
```json
// Most routes:
{"error": "message string"}
// Some middleware abort paths:
"message string"           ← plain string, not a JSON object
// Platform routes:
{"error": "...", "count": N, "merchants": [...]}
```

---

## authkit — Full Route Inventory

Routes are prefix-neutral (`RouteSpec`) — the host mounts at any path. authkit exposes them as groups the host selects at mount time.

### Core group
```
POST   /token                  (public: issue access token or refresh)
POST   /token/org              (required: issue org-scoped token)
GET    /providers              (public: list configured auth providers)
POST   /sessions/current       (public: validate + return current session)
DELETE /logout                 (required)
GET    /me/permissions         (required: introspect caller's permissions)
```

### Password group
```
POST   /password/login
POST   /reauth/password        (required)
POST   /email/password/reset/request
POST   /email/password/reset/confirm
POST   /email/password/reset/confirm-link
POST   /phone/password/reset/request
POST   /phone/password/reset/confirm
```

### Register group
```
POST   /register
GET    /register/availability
POST   /register/resend-email
POST   /register/resend-phone
POST   /register/abandon
```

### Owners group
```
GET    /owners/{slug}          (public: namespace info)
```

### Email / Phone verification groups
```
POST   /email/verify/request
POST   /email/verify/confirm
POST   /email/verify/confirm-link
POST   /phone/verify/request
POST   /phone/verify/confirm
POST   /phone/verify/confirm-link
```

### User group
```
POST   /user/password          (required)
GET    /user/sessions          (required)
DELETE /user/sessions/{id}     (required)
DELETE /user/sessions          (required: revoke all)
GET    /user/me                (required)
GET    /user/bootstrap         (required)
PATCH  /user/username          (required)
PATCH  /user/preferred-locale  (required)
POST   /user/email/change/request (required)
POST   /user/email/change/confirm (required)
POST   /user/email/change/resend  (required)
POST   /user/email/change/cancel  (required)
POST   /user/phone/change/request (required)
POST   /user/phone/change/confirm (required)
POST   /user/phone/change/resend  (required)
POST   /user/phone/change/cancel  (required)
PATCH  /user/biography         (required)
DELETE /user                   (required: self-delete)
DELETE /user/providers/{provider} (required: unlink OIDC provider)
```

### Two-factor group
```
GET    /user/2fa               (required)
POST   /user/2fa/start-phone   (required)
POST   /user/2fa/enable        (required)
POST   /user/2fa/disable       (required)
POST   /user/2fa/regenerate-codes (required)
POST   /2fa/verify             (public)
```

### OIDC linking group
```
POST   /oidc/{provider}/link/start     (required)
POST   /oidc/{provider}/reauth/start   (required)
```

### Solana group
```
POST   /solana/challenge       (public)
POST   /solana/login           (public)
POST   /solana/link            (required)
```

### Orgs group
```
POST   /token/org              (required: org-scoped token)
GET    /orgs                   (required: list user's orgs)
POST   /orgs                   (required: create org)
GET    /orgs/{org}             (required: get org)
POST   /orgs/{org}/rename      (required)
GET    /orgs/{org}/members     (required)
POST   /orgs/{org}/members     (required: invite)
DELETE /orgs/{org}/members/{user_id} (required)
GET    /orgs/{org}/invites     (required)
POST   /orgs/{org}/invites     (required: send invite)
POST   /orgs/{org}/invites/{invite_id}/revoke (required)
GET    /me/invites             (required: my pending invites)
POST   /me/invites/{invite_id}/accept  (required)
POST   /me/invites/{invite_id}/decline (required)
GET    /orgs/{org}/roles       (required)
GET    /orgs/{org}/roles/{role} (required)
PUT    /orgs/{org}/roles/{role} (required: create/update role)
DELETE /orgs/{org}/roles/{role} (required)
POST   /orgs/{org}/service-tokens (required: mint service token)
GET    /orgs/{org}/service-tokens (required: list)
DELETE /orgs/{org}/service-tokens/{token_id} (required)
GET    /permissions            (required: permission catalog)
GET    /orgs/{org}/members/{user_id}/permissions (required: effective perms)
GET    /orgs/{org}/members/{user_id}/roles (required)
POST   /orgs/{org}/members/{user_id}/roles (required: assign role)
DELETE /orgs/{org}/members/{user_id}/roles (required: remove role)
GET    /orgs/{org}/me          (required: my membership in org)
POST   /orgs/{org}/permissions/check (required: permission check)
```

### Admin group
```
POST   /admin/roles/grant      (admin)
POST   /admin/roles/revoke     (admin)
GET    /admin/users            (admin)
GET    /admin/users/{user_id}  (admin)
POST   /admin/users/ban        (admin)
POST   /admin/users/unban      (admin)
POST   /admin/users/set-email  (admin)
POST   /admin/users/set-username (admin)
POST   /admin/users/set-password (admin)
POST   /admin/users/toggle-active (404 — intentionally disabled)
DELETE /admin/users/{user_id}  (admin)
POST   /admin/users/{user_id}/restore (admin)
GET    /admin/users/deleted    (admin)
GET    /admin/users/{user_id}/signins (admin)
POST   /admin/users/{user_id}/sessions/revoke (admin)
POST   /admin/users/{user_id}/password-reset (admin)
POST   /admin/accounts/restrict (admin)
POST   /admin/accounts/unrestrict (admin)
POST   /admin/account/park     (admin)
POST   /admin/account/claim    (admin)
POST   /admin/org/park         (404 — disabled)
POST   /admin/org/claim        (404 — disabled)
```

### Federation (RouteOrgIssuers) group
```
POST   /remote-applications                               (required)
DELETE /remote-applications                               (required)
GET    /remote-applications                               (admin)
POST   /remote-applications/{slug}/memberships            (required)
DELETE /remote-applications/{slug}/memberships            (required)
GET    /remote-applications/{slug}/permissions            (required)
POST   /remote-applications/{slug}/permissions            (required)
DELETE /remote-applications/{slug}/permissions            (required)
POST   /remote-applications/{slug}/attribute-defs         (required)
GET    /remote-applications/{slug}/attribute-defs         (required)
```

### OIDC browser group (browser redirects, separate mount)
```
GET    /{provider}/login
GET    /{provider}/callback
GET    /{provider}/reauth/callback
```

---

## Design Issues — By Severity

---

### CRITICAL

#### OR-API-C1: Single construction-time merchant — the foundational single-tenant assumption

**What this system does here:** At server boot, `config.Merchant` (a slug string) is resolved once into a `merchant.ID` via `db.ResolveMerchantSlug`, stored as `s.configuredMerchant`, and then pinned onto every request via `ginmw.ResolveMerchant(s.configuredMerchant)` as the first middleware. This means every request that doesn't carry a service token or delegated token operates against the same, boot-time-fixed merchant.

**What is broken:** This is the original single-tenant design. In a multi-merchant SaaS deployment:
- All user routes (`/v1/products`, `/v1/me/*`, `/v1/checkout/*`) resolve their merchant from the boot-time config — if you serve 10 merchants from one process, all user requests see Merchant 1's catalog, Merchant 1's subscriptions, etc.
- The admin routes (`/v1/admin/*`) also operate against the configured merchant. There's no URL segment identifying which merchant the admin is acting on.
- `StoreConfig` (name, logo, from-email, customer portal URL) is a single global config block — there's no per-merchant branding.

**Real-world impact:** If this codebase moves to a hosted-SaaS where the operator runs one openrails process serving many merchants, every unauthenticated or user-JWT-authenticated route will be broken — they'll all resolve to whichever merchant slug was in the config file at boot. You'd need one process per merchant, which defeats the SaaS model.

**Fix direction:** 
- User routes need a merchant-disambiguation mechanism: either a URL path prefix (`/v1/m/:merchant/*`), a request header (`X-Merchant: slug`), or per-merchant subdomain routing. The JWT approach used by service tokens and delegated tokens is clean — a user JWT could carry an `org` claim that maps to a merchant.
- Admin routes need the merchant in the URL: `/v1/admin/merchants/:merchant_id/subscriptions` etc. The current `/v1/admin/*` shape makes more sense as what `/v1/merchant-admin/*` is doing (browser-direct, delegated token carries the merchant).
- `StoreConfig` needs to move to a per-merchant database record, not global config.

---

#### OR-API-C2: Global webhook surface routes to configured merchant only

**What this system does here:** `POST /v1/webhooks/:provider` is a legacy single-merchant webhook receiver. It's documented as "the default merchant may still use the global surface." In practice, it hard-codes the configured merchant for all Stripe/NMI/CCBill events that arrive here.

**What is broken:** Stripe delivers webhooks to a registered endpoint. In a SaaS with multiple merchants, each merchant must have its own Stripe webhook endpoint pointing to `/v1/m/:merchant/webhooks/stripe`. If any merchant accidentally points their webhook to `/v1/webhooks/stripe`, events are processed for the wrong merchant. The global surface provides a false sense of security — it silently misattributes events.

**Real-world impact:** Stripe subscription created for Merchant B gets processed as if it belongs to Merchant A. Payments are credited to the wrong merchant's customers. This is not a security escalation (the event itself carries no customer identity) but produces silent data corruption in a multi-merchant context.

**Fix direction:** Deprecate and remove `POST /v1/webhooks/:provider`. Make `/v1/m/:merchant/webhooks/:provider` the canonical and only webhook surface. Add a boot-time warning when the global surface is hit if the system is in multi-merchant mode.

---

### HIGH

#### OR-API-H1: `/v1/admin/*` has no merchant scope in URL — silent context dependency

**What this system does here:** All operator admin routes (`GET /admin/subscriptions`, `GET /admin/users/:user_id`, etc.) operate against the merchant resolved by the global `ResolveMerchant` middleware. In a single-merchant deployment, this is correct. The admin is always acting on that one merchant.

**What is broken:** In a multi-merchant SaaS, an operator (platform admin) might need to view subscriptions for Merchant A, then for Merchant B. With the current URL shape there's no way to express which merchant you're querying — the URL has no merchant segment, and the context comes from the boot-time config or the caller's JWT tenant, neither of which is per-request merchant selection.

**Real-world impact:** Platform operators (who hold `openrails:admin`) cannot switch between merchants within a single API session. The workaround is the `/v1/platform/*` surface, but that is read-only. The mutation surfaces (refund, cancel, off-channel payment) can only target the configured merchant.

**Fix direction:** Add a merchant scope to admin URLs: `/v1/admin/merchants/:merchant_id/subscriptions`, or allow a `X-Merchant-ID` header that the admin middleware resolves instead of the boot-time configured merchant. The per-merchant surface already exists architecturally under `/v1/merchant-admin/*` but requires delegated browser tokens, not operator service tokens.

---

#### OR-API-H2: Duplicate and aliased routes in service API

**What is broken:** The service route table contains:
1. `POST /credits/holds/:id/capture` AND `POST /credits/hold/:id/capture` — singular and plural, both registered, both go to the same handler. Same for `/release`.
2. `GET /credits/invokers/:invoker` AND `GET /invokers/:invoker/credits` — two paths returning the same invoker credit data.

These are API surface leaks from migration/refactoring. They complicate client compatibility guarantees, make API documentation ambiguous, and increase attack surface (both paths must be tested, both paths must be permission-gated).

**Real-world impact:** Clients that discover the undocumented alias may rely on it, making it impossible to remove cleanly. Permission audits that miss one alias create security blind spots.

**Fix direction:** Remove the deprecated singular `/credits/hold/:id/*` routes (redirect is acceptable, but removal is cleaner). Consolidate the invoker credits endpoint to one canonical path.

---

#### OR-API-H3: `MerchantCORS` in static config file doesn't scale to SaaS

**What this system does here:** Per-merchant allowed CORS origins are configured in the YAML/env config file under `merchant_cors: {slug: {allowed_origins: [...]}}`. These are loaded at boot and baked into the CORS middleware allow-list.

**What is broken:** In a SaaS platform, merchants configure their own allowed origins via the API (e.g., when they add a new domain). With static config, every origin change requires an operator to edit the config file and redeploy the server. There's also no validation that an origin actually belongs to the merchant — an operator could misconfigure one merchant's origins into another merchant's allow-list.

**Fix direction:** Move per-merchant CORS origins to the `merchant_secrets` or a `merchant_settings` database table. The CORS middleware should resolve origins from the database (with a short TTL cache) rather than boot-time config. The `/v1/merchant-admin/secrets` surface or a new `PUT /v1/merchant-admin/settings/cors` endpoint gives merchants self-service CORS management.

---

#### AK-API-H1: `POST /remote-applications` not org-scoped in URL

**What this system does here:** Remote application registration (the federation/JWKS issuer registry) has a flat URL: `POST /remote-applications`. The org ownership is enforced inside the handler by checking the caller's JWT claims against the application's registered owner, but the URL does not express the org scope.

**What is broken:** The flat path means:
1. The URL is ambiguous — `GET /remote-applications` (admin-only list) and `POST /remote-applications` (register, any authenticated user) share the same path with different auth semantics. There's no way to read the URL and know "this is org-scoped."
2. It's inconsistent with every other org-scoped resource: `/orgs/{org}/service-tokens`, `/orgs/{org}/roles`, `/orgs/{org}/members` all follow the canonical `/{org}/` prefix pattern.

**Fix direction:** Move to `/orgs/{org}/remote-applications` with `{org}` validated against the caller's JWT, consistent with service tokens and roles. The admin list stays at `GET /remote-applications` (or `GET /admin/remote-applications`) as a cross-org view.

---

#### AK-API-H2: Two token endpoints (`/token` vs `/token/org`) — asymmetric flow

**What this system does here:** A user issues a global (no-org) token via `POST /token`, then issues an org-scoped token via `POST /token/org` (which requires the global token as auth). Getting to an org-scoped session requires two round-trips.

**What is broken:** Standard OAuth flow uses a single `/token` endpoint with `scope` or `resource` parameters to select the target context. The two-endpoint design means every client must know to call `/token` first and `/token/org` second, and must store two tokens — the global one just to be able to mint the org one.

**Real-world impact:** Every client implementation must implement the two-step dance. An SDK that only knows about `/token` will get a token with no org context and most protected endpoints will behave unexpectedly. The error message when you call an org-scoped route with a global token is not obvious without reading the source.

**Fix direction (long-term):** Unify into a single `POST /token` that accepts `org: "slug"` in the request body to produce a scoped token in one call. Keep the existing `/token/org` as a deprecated alias for backwards compatibility.

---

### MEDIUM

#### OR-API-M1: `StoreConfig` is a single global branding block

**What is broken:** `config.StoreConfig` (`name`, `logo_url`, `from_email`, `customer_portal_url`) applies to the entire OpenRails process. In a multi-merchant SaaS, each merchant has their own name, logo, email sender, and customer portal URL. These are used in Solana Pay QR codes and email receipts — if you have two merchants sharing one OpenRails process, both will have the same store name in all communications.

**Fix direction:** Merchants table or `merchant_settings` should carry these fields. The `merchants.Merchant` struct and the DB schema need `store_name`, `logo_url`, `from_email`, `customer_portal_url` columns. `StoreConfig` in `config.go` becomes the default/fallback only.

---

#### OR-API-M2: Error response body shape is inconsistent across routes

**What is broken:** Three different shapes appear across the codebase:
- `{"error": "string"}` — most handlers via `c.JSON(statusCode, gin.H{"error": err.Error()})`
- `"plain string"` — some middleware abort paths via `r.AbortJSON(statusCode, "unauthenticated")` 
- `{"error": "code", "count": N, ...}` — platform routes inline extra fields alongside `"error"`

A client library cannot write a single error-decoding path that works across all routes.

**Fix direction:** Standardize on `{"error": "machine_code", "message": "human string"}`. The `error` field should be a stable, parseable code (e.g., `service_token_expired`, `permission_required`); `message` carries the human-readable detail. The current `writeServiceCredentialError` in `routes.go` is close — it uses machine codes — but needs to be applied consistently across all surfaces.

---

#### OR-API-M3: `/v1/platform/*` is read-only; merchant provisioning lives under `/v1/admin/merchants`

**What is broken:** The platform superadmin surface (`/v1/platform/*`) is where you'd expect to provision and lifecycle-manage merchants — it has the separate platform-org authentication model. But actual provisioning (`POST /v1/admin/merchants`) sits under the per-merchant admin prefix, gated by the same `openrails:admin` permission the per-merchant admin uses.

Concretely: a platform operator uses `/v1/platform/*` to list merchants and view metrics, but must switch to `/v1/admin/merchants` to provision new ones. These are two different paths with two different permission models that both require platform-operator capabilities.

**Fix direction:** Move `POST /v1/admin/merchants` and the merchant lifecycle endpoints (`suspend`, `resume`, `tier`, `export`, `delete`) under `/v1/platform/merchants` behind `openrails:platform:superadmin`. The `/v1/admin/merchants` path can redirect or be retired. This makes the platform surface the canonical place for all cross-merchant operator operations.

---

#### OR-API-M4: `POST /:id/delete` should be `DELETE /:id`

**What is broken:** `POST /v1/admin/merchants/:id/delete` uses POST with a `/delete` suffix instead of `DELETE /v1/admin/merchants/:id`. The same body format and confirmation flow works with the HTTP DELETE method. The `POST /delete` pattern is a Rails-era workaround for HTML forms — not appropriate for a JSON API.

Similarly, `POST /v1/admin/merchants/:id/suspend` and `POST /:id/resume` are fine as POST (these are actions, not resource mutations) but `POST /:id/delete` is semantically DELETE.

**Fix direction:** Change to `DELETE /v1/admin/merchants/:id` with the confirmation body in the request. Update the client.

---

#### OR-API-M5: Merchant credential management is split across two surfaces

**What is broken:** Merchant credentials (Stripe keys, webhook signing secrets, etc.) can be managed from two different surfaces:
1. Operator: `PUT /v1/admin/merchants/:id/credentials/*name` — operator acts on any merchant's credentials
2. Merchant self-service: `PUT /v1/merchant-admin/secrets/*name` — merchant admin manages their own secrets via delegated token

The shapes are different: the operator surface uses `{"value": "..."}` in the body; the merchant-admin surface uses `PUT /secrets/*name` with the same body. Both ultimately call the same secret store. There's no clear owner: is credential management operator-driven or merchant-self-service? In a SaaS it should be merchant-self-service with an operator override, but the current split makes the intended model ambiguous.

**Fix direction:** Document which surface is canonical. Ideally merchant admins use `/v1/merchant-admin/secrets/*` for all day-to-day credential management, and the operator surface is a last-resort override (for support cases where the merchant's credentials are invalid). Add a `GET /v1/admin/merchants/:id/credentials/:name/status` (no value, just configured/not-configured) to the operator surface so operators can inspect without reading secrets.

---

#### AK-API-M1: Admin mutation routes use POST instead of REST-idiomatic methods

**What is broken:** These admin routes all use POST:
- `POST /admin/users/set-email` — should be `PATCH /admin/users/{user_id}`
- `POST /admin/users/set-username` — same
- `POST /admin/users/set-password` — same
- `POST /admin/users/ban` / `unban` — acceptable as POST (lifecycle actions)
- `POST /admin/accounts/restrict` / `unrestrict` — acceptable as POST

The `set-*` verbs are a symptom of building a JSON-RPC-style API on top of HTTP rather than resource-oriented REST. Each separate endpoint for setting one field is also a future maintenance burden — every new user attribute needs a new endpoint.

**Fix direction:** Consolidate to `PATCH /admin/users/{user_id}` with a partial update body: `{"email": "...", "username": "...", "password": "..."}`. Keep `POST /admin/users/{user_id}/ban` and `POST /admin/users/{user_id}/restore` as action endpoints (they're lifecycle state changes, not field mutations, so POST is correct).

---

#### AK-API-M2: `GET /register/availability` enables username/email enumeration

**What is broken:** `GET /register/availability` is a public endpoint that tells the caller whether a username or email is already taken. While this is common in consumer apps, in a security-sensitive multi-tenant platform it's a user enumeration oracle: an attacker can probe whether `admin@merchant.com` or any known email is registered, enabling targeted phishing and credential-stuffing campaigns.

**Real-world impact:** Attacker enumerates user identities across all merchants (authkit is shared). Then uses that list for targeted phishing or to probe weak-password accounts.

**Fix direction:** Rate-limit aggressively per IP (already present but may need tightening). Alternatively, return a consistent 200 with no useful signal, and rely on the registration flow itself to give the "already taken" error after a CAPTCHA pass. The most secure approach is to remove the endpoint entirely and let the registration flow be the only enumeration point.

---

### LOW / INFORMATIONAL

#### OR-API-L1: Embedded mode URL prefix (`/billing/v1/*`) not tested as primary surface

**What:** `EmbeddedV1Prefix = "/billing/v1"` and `StandaloneV1Prefix = "/v1"` are defined but the embedded handler mux (`NewHTTPHandler`) only mounts `user`, `admin`, and `webhook` route groups — it does not mount `service`, `self`, `merchant-admin`, or `platform` routes. Embedded hosts that want the full surface must use the standalone server.

**Risk:** An embedded host that assumes the full API surface is available via `NewHTTPHandler` will silently miss routes. The missing surfaces are not documented.

#### OR-API-L2: `GET /v1/solana/config` and `/v1/solana/tokens` are unauthenticated and not merchant-scoped

**What:** These public discovery endpoints return Solana processor config (supported tokens, addresses, network). In a multi-merchant context, different merchants may have different Solana configurations. The current endpoints return the global (configured-merchant) Solana config.

**Risk:** A user calling the wrong merchant's Solana config will see the wrong recipient wallet or wrong supported tokens. Low severity since the checkout flow validates at payment time, but the discovery endpoint gives misleading information.

#### AK-API-L1: `GET /owners/{slug}` — unclear ownership semantics

**What:** The `RouteOwners` group has one endpoint: `GET /owners/{slug}`. The handler name suggests it returns namespace/owner info for the given slug, but the intended use case and response shape are not documented. It's public (no auth). It may be an org or user namespace discovery endpoint, but the `owners` terminology doesn't map to any other concept in the authkit model (orgs are orgs, users are users).

**Risk:** If this endpoint leaks user identity or org membership info to unauthenticated callers it's an enumeration concern. Worth reviewing the handler implementation and either documenting or removing it.

#### AK-API-L2: `POST /admin/users/toggle-active` is intentionally a 404

**What:** The route is registered with `notFoundHandler` — it always returns 404. The route exists as a placeholder/tombstone. It should be removed from the route list entirely rather than registered as a permanent 404, since it adds confusion to any API exploration.

#### AK-API-L3: `POST /admin/org/park` and `/claim` are also intentional 404s

**What:** Same as above. These two routes are registered but always 404. Remove rather than leave as silent dead-ends.

---

## Summary Table

| ID | Surface | Severity | What |
|----|---------|----------|------|
| OR-API-C1 | openrails | Critical | Single configured-merchant at boot — all user/admin routes use boot-time merchant |
| OR-API-C2 | openrails | Critical | Global `/v1/webhooks/:provider` silently misattributes events in multi-merchant |
| OR-API-H1 | openrails | High | `/v1/admin/*` has no merchant scope in URL — no per-request merchant selection |
| OR-API-H2 | openrails | High | Duplicate service API routes (singular/plural holds, dual invoker credits path) |
| OR-API-H3 | openrails | High | Per-merchant CORS origins in static config, not database — can't self-serve |
| AK-API-H1 | authkit | High | `POST /remote-applications` not org-scoped in URL (inconsistent with all other org resources) |
| AK-API-H2 | authkit | High | Two-step token flow (`/token` then `/token/org`) — non-standard, client friction |
| OR-API-M1 | openrails | Medium | `StoreConfig` is global — per-merchant branding not supported |
| OR-API-M2 | openrails | Medium | Error response body shape inconsistent across routes |
| OR-API-M3 | openrails | Medium | Platform provisioning at `/v1/admin/merchants` not `/v1/platform/merchants` |
| OR-API-M4 | openrails | Medium | `POST /:id/delete` should be `DELETE /:id` |
| OR-API-M5 | openrails | Medium | Merchant credential management split across operator and merchant-admin surfaces |
| AK-API-M1 | authkit | Medium | `POST /admin/users/set-*` should be `PATCH /admin/users/{user_id}` |
| AK-API-M2 | authkit | Medium | `GET /register/availability` is a user enumeration oracle |
| OR-API-L1 | openrails | Low | Embedded mode handler missing service/self/merchant-admin routes |
| OR-API-L2 | openrails | Low | Solana config/tokens endpoints not merchant-scoped |
| AK-API-L1 | authkit | Low | `GET /owners/{slug}` — unclear semantics, review for enumeration risk |
| AK-API-L2 | authkit | Low | `POST /admin/users/toggle-active` permanently 404 — dead route |
| AK-API-L3 | authkit | Low | `POST /admin/org/park` and `/claim` permanently 404 — dead routes |

---

## Recommended Priorities

The two criticals (OR-API-C1, OR-API-C2) represent the core single-tenant design assumptions that will block any meaningful multi-merchant SaaS deployment. Everything else is fixable incrementally.

**Immediate (blocks multi-merchant):**
1. **OR-API-C1** — Design the merchant-disambiguation strategy for user routes. The cleanest path is a `POST /token` that emits merchant-context claims when the org maps to a merchant. User routes then resolve the merchant from the JWT.
2. **OR-API-C2** — Remove or hard-deprecate the global webhook surface.

**Near-term (API hygiene before more clients are built):**
3. **OR-API-M2** — Standardize the error response shape now, before more clients are built against the current inconsistent shape.
4. **AK-API-H1** — Move remote-application registration to `/orgs/{org}/remote-applications`.
5. **OR-API-H2** — Remove duplicate service routes.

**Longer-term (SaaS maturation):**
6. **OR-API-M1** + **OR-API-H3** — Move StoreConfig and CORS origins to per-merchant database records.
7. **OR-API-M3** — Consolidate platform operations under `/v1/platform/*`.
8. **AK-API-H2** — Unify token issuance to single `/token` endpoint.
