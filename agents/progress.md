<!-- openrails issue tracker — ACTIVE issues -->

> One `# #<id>: <name>` section per issue, separated by `---` lines; section anchor for
> tooling is a line starting with `# #`. IDs are stable for an issue's whole lifecycle and
> share ONE per-repo id space across progress.md / future.md / completed.md; new issues take `next_id` below and bump it.
> CONCURRENT EDITS: only ever edit/append your own issue's section with targeted string
> replacement — never rewrite the whole file.


next_id: 563

---

# #554: define final OpenRails permission catalog for public, org-treasury, and merchant routes

**Completed:** no
**Status:** planned 2026-06-20: define the complete OpenRails permission vocabulary from the planned route model. Cut permissions that do not bind to a planned route and collapse theoretical fine-grained splits into the smallest useful role boundaries. Rename merchant-resource permissions from `org:*` to `merchant:*`; they are still granted/evaluated in the AuthKit org scope that owns the merchant. This issue supersedes the rough "Initial OpenRails Platform/Org-Local Permissions" list inside #552.

## Model

- OpenRails core has four route buckets: public, personal customer self, org-customer treasury, and merchant.
- Public routes require no OpenRails RBAC permission.
- Individual customer self routes are authenticated self-access by subject; personal user balances are not delegable.
- Org customer treasury routes are explicit `/v1/orgs/:org_id/*` routes and AuthKit-org scoped. This covers "I am acting for this org customer/payer, and I want to read billing or share this org balance."
- Merchant routes are AuthKit-org scoped where the AuthKit org owns exactly one OpenRails merchant.
- OpenRails core should not define platform-admin permissions for the merchant/customer surface. OpenRails SaaS tracks platform operator permissions separately (`platform:merchants:read/delete/restore`).
- Permission names describe the OpenRails resource. Merchant-resource permissions use `merchant:<resource>:<action>` even though AuthKit stores/evaluates the grant in the owning org scope.

## Public Routes

No permissions.

## Personal Customer Self Permissions

None. Authenticated self-subject is enough for `/v1/me/*`.

## Org Customer Treasury Permissions

Only define permissions for the org-customer treasury routes that actually exist:

```text
org:spend-delegations:read       # read org balance-sharing policy
org:spend-delegations:update     # replace org balance-sharing policy
```

`org:spend-delegations:*` only applies to the org named in `/v1/orgs/:org_id/spend-delegations`. Do not allow an individual/personal customer to delegate their personal balance.

## Merchant Permissions

Scoped to the AuthKit org that owns the OpenRails merchant:

```text
merchant:settings:read             # GET /v1/merchant/settings
merchant:settings:update           # PUT /v1/merchant/settings

merchant:payment-providers:read    # GET /v1/merchant/payment-providers*
merchant:payment-providers:update  # PUT/DELETE payment provider config

merchant:catalog:read              # GET catalog products, prices, drift
merchant:catalog:update            # product/price writes, drift refresh, catalog publish

merchant:customers:read            # list/read customers, balances, transactions, support profile
merchant:customers:update          # customer support writes: grants, balance adjustments, credit limits, off-channel payments, payment-method removal

merchant:payments:read             # search/read payments and customer payment history
merchant:payments:refund           # refund a payment

merchant:subscriptions:read        # search/read subscriptions
merchant:subscriptions:update      # cancel/update a subscription

merchant:admissions:create         # create, capture, release admissions; report wasted spend
merchant:usage:read                # POST /v1/merchant/usage/*

merchant:repair-alerts:read        # GET /v1/merchant/repair-alerts
```

Merged permission mapping:

- `merchant:payment-providers:update` covers provider update and delete/disable.
- `merchant:catalog:update` covers product/price writes, drift refresh/repair,
  and `POST /v1/merchant/catalog/publish`.
- `merchant:customers:read` covers customer profile, balance, transaction,
  entitlement, product-access, payment, and subscription reads under a customer.
- `merchant:customers:update` covers manual entitlement/product-access grants,
  balance adjustments, credit-limit changes, off-channel payment recording, and
  support payment-method removal.
- `merchant:subscriptions:update` covers cancellation/update workflows.
- `merchant:admissions:create` covers the whole admission lifecycle: create,
  capture, release, and wasted-spend report.

## Tasks

- [ ] Replace #552's rough initial permission list with this catalog once implementation starts.
- [ ] Add these permission definitions to the OpenRails AuthKit permission catalog.
- [ ] Delete old planned permissions that have no route: org-customer `billing`, `checkout`, `payment-methods`, and `subscriptions` permissions.
- [ ] Rename merchant-resource permission constants/docs/tests from `org:*` to `merchant:*`, while keeping the AuthKit scope check tied to the owning org.
- [ ] Collapse old fine-grained merchant permissions into the smaller set above; do not keep separate `balances`, `credit-limits`, `entitlements`, `product-access`, `payment-methods`, `payment-providers:delete`, `catalog:repair`, `payments:create`, `subscriptions:cancel`, `admissions:capture`, `admissions:release`, or `wasted-spend:create` gates unless a real role split later requires them.
- [ ] Map every planned `/v1/me/*` route to authenticated personal self-access and every planned `/v1/orgs/:org_id/*` route to one of the org customer permissions above.
- [ ] Map every planned `/v1/merchant/*` route to one merchant permission above.
- [ ] Add tests proving individual self-access does not require org permissions but org-scoped treasury access does.
- [ ] Add tests proving personal/individual customers cannot use `spend-delegations`.
- [ ] Add tests proving merchant permissions are scoped to the merchant-owner org and do not apply to customer/payer orgs unless it is the same org.
- [ ] Add docs showing OpenRails SaaS platform operator permissions live outside this core catalog.

## Acceptance

- OpenRails has one concrete permission catalog for all non-public core routes.
- No core merchant/customer route depends on a fake platform org or platform-admin permission.
- Every defined OpenRails core permission binds to at least one planned route.
- Merchant-resource permission strings use `merchant:*`, not `org:*`; org scope is part of the AuthKit grant/check context.
- Merchant permissions are coarse enough to match real roles; fine-grained splits are only added when a concrete admin persona needs them.
- `org:spend-delegations:*` is org-customer-only and cannot delegate personal user balances.
- Route tests cover public/no-permission, personal self-access, org-scoped treasury access, and merchant org access.

---

# #553: rename payer tier to trust-tier

**Completed:** yes
**Status:** COMPLETE 2026-06-20: trust-tier wire/API rename is implemented for the live admission/service surfaces. Canonical graduated-tier route is `/v1/service/trust-tier`; `/v1/service/tier` and old `tier` request fields remain compatibility aliases because Tensorhub still uses the current Go client `Tier` fields. Validation: `go test . ./internal/http/handlers ./internal/http/routes/ginroutes ./pkg/service ./embed`; `go test ./internal/modules/admission/...`; `git diff --check`.

## Scope

- Rename API response fields from `tier` to `trust_tier` when the value is the customer's auto-maintained trust/spend level.
- Rename route/docs wording from "payer tier" or "customer tier" to "trust tier" where that is the actual meaning.
- Keep product/catalog terminology unchanged: product tiers, tier groups, prices, and subscription plans are not trust tiers.
- Keep internal database column renames optional unless the current names leak into generated APIs or make code misleading. Prefer API/docs clarity over churn.
- Preserve compatibility aliases only if downstreams still read `tier`; otherwise hard-cut before the new merchant route surface ships.

## Tasks

- [x] Audit OpenRails for payer/customer `tier` usages and classify each as trust-tier vs product/catalog tier.
- [x] Update merchant customer profile responses to return `trust_tier` instead of ambiguous `tier`. DONE 2026-06-20: no live merchant-customer profile handler exists yet; the live graduated-tier read response now returns `trust_tier`.
- [x] Update admission/settings policy docs so trust-tier language is explicit where it refers to spend/trust classification.
- [x] Update Go DTOs/client names if they expose payer/customer tier publicly.
- [x] Update tests and fixtures to use `trust_tier`.
- [x] Verify catalog/product tier terminology remains unchanged.

## Acceptance

- Merchant/customer APIs no longer expose ambiguous `tier` for payer trust classification.
- Product/catalog tier names are untouched.
- Tests cover the renamed customer profile/admission DTO fields.

---

# #552: merchant-api-surface-recut (/v1/service → /v1/merchant) [EPIC — children #554–#562]

**Completed:** no
**Status:** PLANNED 2026-06-20: this is a route + principal recut, NOT an auth-model change. The two RBAC planes already exist and are already separate: `platform:*` (control-plane platform layer, no org — and per #554 platform/operator permissions live in OpenRails SaaS, not core) and org-scoped OpenRails permissions (named `merchant:*` / `org:*`, evaluated in the owning AuthKit org). "Platform org" is dead legacy scaffolding, not a proposed model: `PlatformOrgSlug()` already returns "" and `/v1/platform/*` never mounts in core. So the real work is: (1) move merchant-owned `/v1/service/*` to resource-named `/v1/merchant/*`, (2) normalize every credential type into one merchant/org principal check, (3) recut the customer-self / org-treasury / Tensorhub surfaces, and (4) delete the dead platform-org wiring. The contracts below are the shared design source of truth; the work is sliced into the child issues under "## Child Issues".

HARD CUT — no backwards compatibility, no data migration, no aliases. `/v1/service/*` and `/v1/self/*` are deleted, not aliased. All consumers (Doujins, Tensorhub, Cozy Art) are first-party and bump in lockstep.

`/v1/service/*` describes the credential type, not the resource boundary. Merchant-owned operations move under `/v1/merchant/*` and accept any credential that resolves to the owning org with the needed permission: regular logged-in user, delegated JWT, self-service/browser JWT with org authority, or API key.

## Metadata

- Category: auth
- Status: planned
- Passes: false

## Decisions

- There is no OpenRails "platform org".
- Do not use AuthKit org membership or org roles to authorize OpenRails platform routes.
- Do not model platform admins as merchant admins on a special merchant.
- OpenRails owns the permission names and route gates; AuthKit owns the single RBAC model, role assignment, and effective-permission resolution.
- Platform permissions use `platform:<resource>:<action>` and are global to the OpenRails installation.
- Org-scoped OpenRails permissions use the AuthKit org RBAC plane but the permission string names the OpenRails resource, e.g. `merchant:payments:refund` for a merchant route scoped to the merchant-owner org.
- Merchant route authorization is tied to the AuthKit org that owns the OpenRails merchant.
- Route paths should describe the resource boundary, not the auth mechanism: use `/v1/merchant/*`, not `/v1/service/*`, for merchant-owned operations.
- Do not add `/admin` inside merchant routes. `/v1/merchant/*` already means "authorized actor operating inside the authenticated merchant/org"; admin-ness comes from the permission gate, not the path.
- API keys, delegated JWTs, self-service/browser JWTs with org authority, and regular logged-in users should all normalize into the same org/merchant principal check on merchant routes.
- Core merchant lookup APIs should be customer-forward: list a customer's entitlements, check one entitlement, list a customer's product access, check one product access, and read a customer's balance.
- Reverse lookups such as "which customers have entitlement X" are directory/filter APIs, not the common embedded host path.
- Drop `issuer` from merchant entitlement lookup request bodies once customer identity is `(merchant_id, subject)`; merchant/org comes from auth, subject comes from the request.
- Merchant HTTP routes should be classified by deployment need: standalone/remote needs HTTP APIs; embedded hosts should prefer the Go interface into OpenRails and direct DB access through OpenRails services.
- Embedded mode should mount browser-facing/customer/admin/webhook routes as needed, but should not require internal merchant lookup HTTP routes just to let the host call back into OpenRails.
- OpenRails routes should fall into four product buckets in the core product:
  public routes, personal customer self routes, org-customer treasury routes,
  and merchant routes. Platform/operator routes belong to OpenRails SaaS, not
  the core merchant/customer surface.
- `platform:*` must never imply any org-local OpenRails permission inside a merchant/org.
- Org-local OpenRails permissions must never imply any `platform:*` permission.
- Standalone mode uses OpenRails' bundled AuthKit control plane for RBAC.
- Embedded mode uses the host application's AuthKit RBAC/principal mapping; OpenRails still checks the same permission names.

## Child Issues

This epic is the design source of truth; the master checklist under "## Tasks" is decomposed into landable child issues. Each child carries its own detailed `- [ ]` task list and acceptance, and is a hard cut (no aliases).

- [ ] #554 — final OpenRails permission catalog (public / personal-self / org-treasury / merchant; `merchant:*` naming). *(created)*
- [ ] #555 — `/v1/service` → `/v1/merchant` rename + one merchant/org principal resolver; drop `issuer`; customer-forward vs directory split.
- [ ] #556 — embedded route-set presets (replace `IncludeUser` / `IncludeAdmin` / `IncludeWebhooks` booleans).
- [ ] #557 — customer self `/v1/me/*` + org-treasury `/v1/orgs/:org_id/*` (spend-delegations); delete `/v1/self/*`.
- [ ] #558 — Tensorhub client recut into Admission/PolicySync/AdminFunding Go interfaces + settings/policy split; batch-native admission.
- [ ] #559 — merchant payment-provider config API (replace flat secret-name CRUD; atomic validate-then-store).
- [ ] #560 — merchant catalog publish/drift over HTTP (HTTP form of `push-merchant-catalog`; plan-only default).
- [ ] #561 — merchant customer-support + payments/subscriptions admin surface (resource-named, grant-ledger audited).
- [ ] #562 — delete dead platform-org wiring from core (empty-slug mount switch + `PermPlatformSuperadmin` alias).

## Permission Catalog

Use #554 as the core OpenRails permission catalog. This issue should not keep a
second rough list. OpenRails core currently plans only:

- org-customer treasury permissions for `/v1/orgs/:org_id/spend-delegations`.
- `merchant:*` permissions for actual `/v1/merchant/*` routes, scoped to the
  AuthKit org that owns the merchant.

OpenRails SaaS tracks platform/operator permissions separately.

## Current Smells To Remove

- `controlPlane.PlatformOrgSlug()` as a mount switch for `/v1/platform/*`.
- `PermPlatformSuperadmin` as a single fake route gate instead of concrete OpenRails `platform:*` permissions.
- Route comments that imply platform administration is tied to an AuthKit org.
- Any test setup that grants platform route access by creating or referencing an org-like authority.
- Any route-specific merchant permission that still uses the misleading `org:*` prefix. Merchant-resource permissions should be `merchant:*` and scoped by the AuthKit org grant context.
- `/v1/service/*` as the primary path for merchant-owned APIs.
- `POST /v1/service/customers/by-external-subject/entitlements`: badly named, carries obsolete `issuer`, and mixes standalone remote identity lookup with the simpler merchant/customer model.
- Auth middleware split by credential type instead of normalizing credentials into one org/merchant principal.

## Proposed Merchant Lookup API

Core HTTP surface:

```text
GET  /v1/merchant/customers/:customer_id/entitlements
POST /v1/merchant/customers/entitlements:batch
GET  /v1/merchant/customers/:customer_id/entitlements/:name
GET  /v1/merchant/customers/:customer_id/products
GET  /v1/merchant/customers/:customer_id/products/:product_id/access
GET  /v1/merchant/customers/:customer_id/balance?currency=USD
```

Batch entitlements request after dropping issuer:

```json
{
  "subjects": ["user-uuid-1", "user-uuid-2"],
  "at": "2026-06-20T12:00:00Z"
}
```

Response remains keyed by subject, with unknown subjects returning `[]`.

Directory/filter API, if still needed:

```text
GET /v1/merchant/entitlements/:name/customers
```

Go library surface should stay as close as possible to HTTP:

```go
ListEntitlements(ctx, customerID)
HasEntitlement(ctx, customerID, name)
ListEntitlementsBatch(ctx, []customerID)
ListProductAccess(ctx, customerID)
HasProductAccess(ctx, customerID, productID)
GetBalance(ctx, customerID, currency)
ListCustomersWithEntitlement(ctx, name, page) // directory/filter only
```

## Route Mounting By Deployment Mode

Standalone/remote mode should mount merchant HTTP routes because the host calls OpenRails over the network:

```text
GET  /v1/merchant/customers/:customer_id/entitlements
POST /v1/merchant/customers/entitlements:batch
GET  /v1/merchant/customers/:customer_id/entitlements/:name
GET  /v1/merchant/customers/:customer_id/products
GET  /v1/merchant/customers/:customer_id/products/:product_id/access
GET  /v1/merchant/customers/:customer_id/balance?currency=USD
GET  /v1/merchant/entitlements/:name/customers
```

Embedded mode should prefer the Go interface for those same operations:

```go
ListEntitlements(...)
HasEntitlement(...)
ListEntitlementsBatch(...)
ListProductAccess(...)
HasProductAccess(...)
GetBalance(...)
ListCustomersWithEntitlement(...)
```

Embedded mode still mounts HTTP for browser/user flows, delegated merchant-admin UI flows, and webhooks. It should not need HTTP routes for host-internal lookups when the host can call OpenRails in-process.

## Embedded Route Group API

Host applications should not have to infer route groups from booleans like
`IncludeUser`, `IncludeAdmin`, and `IncludeWebhooks`. Expose boring named route
sets and presets instead:

```go
type RouteSet string

const (
	RouteSetPublicCatalog RouteSet = "public_catalog" // /products, /prices, solana config/pay helpers
	RouteSetCustomer      RouteSet = "customer"       // /me/* browser self-service
	RouteSetMerchantAdmin RouteSet = "merchant_admin" // /merchant/* delegated merchant UI/support
	RouteSetMerchantAPI   RouteSet = "merchant_api"   // /merchant/* host-internal or remote merchant API
	RouteSetWebhooks      RouteSet = "webhooks"       // processor callbacks
)

var EmbeddedDefaultRouteSets = []RouteSet{
	RouteSetPublicCatalog,
	RouteSetCustomer,
	RouteSetMerchantAdmin,
	RouteSetWebhooks,
}

var StandaloneDefaultRouteSets = []RouteSet{
	RouteSetPublicCatalog,
	RouteSetCustomer,
	RouteSetMerchantAdmin,
	RouteSetMerchantAPI,
	RouteSetWebhooks,
}
```

Lazy rule: embedded defaults exclude `RouteSetMerchantAPI` because the host can
call OpenRails' Go service directly. Hosts may opt in when they intentionally
want HTTP loopback parity or expose a remote-compatible API surface.

## Customer Self-Service Route Recut

Customer routes are browser/delegated self-service for the authenticated
customer/payer. They do not need RBAC permissions for ordinary own-account
operations; the auth subject is the customer.

Core customer self routes:

```text
GET    /v1/me/balance?currency=USD
GET    /v1/me/transactions?currency=USD&limit=&offset=
GET    /v1/me/settings
PUT    /v1/me/settings
GET    /v1/me/payment-methods
POST   /v1/me/payment-methods
DELETE /v1/me/payment-methods/:id
POST   /v1/me/checkout
GET    /v1/me/subscriptions
GET    /v1/me/subscriptions/:id
POST   /v1/me/subscriptions/:id/cancel
GET    /v1/me/payments
GET    /v1/me/invoices
GET    /v1/me/invoices/:id
GET    /v1/me/usage?currency=USD&from=&to=
```

Org-customer delegated spend sharing:

```text
GET    /v1/orgs/:org_id/spend-delegations
PUT    /v1/orgs/:org_id/spend-delegations
```

`spend-delegations` is the self-service shape for "I am acting as this org
customer, and I want selected users/roles to spend this org's balance, subject
to budget windows." It is org-customer policy only: personal/individual customer
balances cannot be delegated. `:org_id` is the payer/customer org whose balance
is being shared. The caller must have AuthKit authority in that org and hold the
org permission that allows reading or changing spend delegation. It is not a
merchant-admin support override. The request should be a full replacement document
with boring scopes. This is a view over the same delegated-spend limit records
OpenRails admission enforces; do not create a second delegation system:

```json
{
  "currency": "USD",
  "delegations": [
    {
      "scope": "invoker_tier",
      "scope_id": "tier_1",
      "windows": [
        {"key": "5h", "window_seconds": 18000, "amount": 5000000}
      ]
    },
    {
      "scope": "role",
      "scope_id": "role-uuid",
      "windows": [
        {"key": "1w", "window_seconds": 604800, "amount": 35000000}
      ]
    }
  ]
}
```

Scope semantics:

- `invoker_tier`: a per-invoker meter selected by the delegated user's tier.
- `role`: a shared meter selected by role UUID; every delegated user carrying
  the role shares the same window.
- `amount` is the currency-native integer amount. For USD, it is micro-dollars.
- No per-user custom override until a real UI/workflow needs it.

For embedded Tensorhub/Cozy Art, the host can keep using its Go/policy path:
Cozy Art authors role/tier budgets, Tensorhub checks AuthKit `billing:spend`
and passes payer/invoker/role/tier to OpenRails admission. These `/v1/me`
routes are only needed when OpenRails itself owns the customer-facing UI for
sharing a balance.

## Tensorhub Merchant API Recut

Tensorhub is the only real consumer of the broad `/v1/service/*` credit/admit
surface. Its production path does not need a generic service-account grab bag.
It needs three boring OpenRails client surfaces:

```go
type AdmissionClient interface {
	Admit(ctx, batchReq)
	Capture(ctx, requestID, capturedAmount, usage)
	Release(ctx, requestID)
	ReportWastedSpend(ctx, report)
	GetTier(ctx, customerID, currency)
}

type PolicySyncClient interface {
	SetTierSchedule(ctx, currency, schedule)
	SetTierSpendLimits(ctx, currency, tier, limits)
	SetDelegatedInvokerWastedSpendLimits(ctx, windows)
}

type AdminFundingClient interface {
	DepositCredits(ctx, request)
	SetCreditLimit(ctx, customerID, currency, limit)
	UsageRollup(ctx, customerID, currency, from, to, groupBy)
	ResourceRevenueDaily(ctx, resourceID, currency, from, to)
}
```

Tensorhub owns request pricing, endpoint/resource identity, endpoint access,
capacity/scheduler decisions, local platform-abuse event display, and the UI
meaning of its tiers/roles. OpenRails owns the money ledger, balances,
holds/captures/releases, arrears credit-limit enforcement, spend counters,
wasted-spend counters, tier graduation from paid spend, usage rollups from
captured ledger events, payments, subscriptions, and invoicing.

Remote/standalone Tensorhub should only need this merchant HTTP surface:

```text
POST /v1/merchant/admissions                    # always batch-shaped; one item is still an array
POST /v1/merchant/admissions/:request_id/capture
POST /v1/merchant/admissions/:request_id/release
POST /v1/merchant/wasted-spend
GET  /v1/merchant/settings
PUT  /v1/merchant/settings

POST /v1/merchant/customers/:customer_id/balance-adjustments
PUT  /v1/merchant/customers/:customer_id/credit-limit
POST /v1/merchant/customers/:customer_id/usage/rollup
POST /v1/merchant/usage/resource-revenue
```

Routes to cut from the Tensorhub-required surface unless another downstream
proves a real need:

```text
GET  /v1/service/budget
GET  /v1/service/merchant-configuration
GET  /v1/service/abuse-usage
GET  /v1/service/credit-limit
GET  /v1/service/credits/balance
POST /v1/service/credits/withdraw
GET  /v1/service/credits/transactions/lookup
PUT  /v1/service/credits/account-settings
GET  /v1/service/credits/account-settings
GET  /v1/service/credits/transactions
```

Reasoning:

- Tensorhub's hot path is `Admit` before work, `Capture` on success, `Release`
  on failure/cancel, and `ReportWastedSpend` for rejected or failed provider
  spend. Admit is always batch-shaped; a single request is just a one-item batch.
  Direct `withdraw` bypasses that lifecycle.
- Tensorhub reads local abuse/platform event tables for its abuse UI; it does
  not need OpenRails' `abuse-usage` endpoint.
- Tensorhub's previous admin proxy for OpenRails account settings and
  transaction history was already removed; customers/orgs should use the
  OpenRails customer/merchant surfaces directly.
- OpenRails invoicing remains internal to OpenRails. Tensorhub should not need
  routes to crank invoices, charge arrears, or inspect processor internals.
- Embedded Tensorhub should use the same Go interfaces directly and should not
  mount these merchant HTTP routes unless it wants remote parity.

## Merchant Admin Frontend Surface

There are two different admin surfaces and they should not blur together:

- Platform admin: cross-merchant OpenRails operations such as creating,
  disabling, exporting, or deleting merchants.
- Merchant admin: actions by an admin of exactly one merchant/org, usually from
  a host frontend or OpenRails standalone merchant dashboard.

Merchant admins should not manage `owner_org_id`, platform status, platform
exports, or hard deletes. Those stay under `platform:*`.

### Merchant Route Contracts

Keep merchant settings boring: display/support metadata, checkout/webhook URLs,
and merchant-owned billing policy. Payment-provider routing and credentials live
under `/v1/merchant/payment-providers/*`. Do not put platform lifecycle fields
here.

Settings and payment providers:

| Route | Purpose | Request | Response |
|---|---|---|---|
| `GET /v1/merchant/payment-providers` | List configured payment providers. | `?provider=&environment=&status=` | `{data:[payment_provider]}` |
| `GET /v1/merchant/payment-providers/:provider` | Read one payment provider's status/config. | optional `?environment=live` | `{payment_provider}` |
| `PUT /v1/merchant/payment-providers/:provider` | Create/update one provider config as a block. Supplied credentials are validated before storage. | `{environment,enabled,account_id?,public_config?,credentials?}` | `{payment_provider}` |
| `DELETE /v1/merchant/payment-providers/:provider` | Disable/remove one payment provider from future use. | optional `{environment,reason}` | `{payment_provider}` |

`payment_provider` responses expose metadata and configured-field status, never
secret plaintext:

```json
{
  "id": "uuid",
  "provider_type": "stripe",
  "environment": "live",
  "account_id": "acct_...",
  "role": "primary",
  "status": "enabled",
  "public_config": {
    "publishable_key": "pk_..."
  },
  "credentials": {
    "secret_key": {"configured": true, "updated_at": "..."},
    "webhook_signing_secret": {"configured": true, "updated_at": "..."}
  }
}
```

Current merchant credential/config fields by provider:

- Stripe: `secret_key`, `webhook_signing_secret`,
  `webhook_signing_secret_thin`, and optional public `publishable_key`.
- NMI/Mobius: `production_key`, `tokenization_key`, `tokenization_url`, and
  `webhook_signing_secret`. `tokenization_key`/URL are browser-facing config,
  but still belong on the provider account.
- CCBill: `account_config` for now, until it is split into typed fields.
- Solana: `private_key` exists but is OpenRails/platform-owned, not
  merchant-admin writable.

Credential updates are atomic: validate first, store only if validation passes.
There is no separate `/validate` route in the primary API. Direct
`/v1/merchant/secrets/*name` CRUD should be retired or compatibility-only.
The storage layer may still store values under provider-account-scoped secret
names like `provider_accounts/{provider}/{environment}/{account_id}/{key}`.

Catalog:

| Route | Purpose | Request | Response |
|---|---|---|---|
| `GET /v1/merchant/catalog/products` | List products for admin UI. | query filters/page | `{data:[product],page}` |
| `POST /v1/merchant/catalog/products` | Create product. | product fields | `{product}` |
| `GET /v1/merchant/catalog/products/:id` | Read product. | none | `{product}` |
| `PATCH /v1/merchant/catalog/products/:id` | Update product fields. | partial product fields | `{product}` |
| `POST /v1/merchant/catalog/products/:id/activate` | Make product sellable/visible. | optional `{reason}` | `{product}` |
| `POST /v1/merchant/catalog/products/:id/deactivate` | Stop new sales without deleting history. | optional `{reason}` | `{product}` |
| `GET /v1/merchant/catalog/prices` | List prices. | query filters/page | `{data:[price],page}` |
| `POST /v1/merchant/catalog/prices` | Create price. | price fields | `{price}` |
| `GET /v1/merchant/catalog/prices/:id` | Read price. | none | `{price}` |
| `PATCH /v1/merchant/catalog/prices/:id` | Update price fields. | partial price fields | `{price}` |
| `POST /v1/merchant/catalog/prices/:id/activate` | Make price available for new purchases. | optional `{reason}` | `{price}` |
| `POST /v1/merchant/catalog/prices/:id/deactivate` | Retire price for new purchases. | optional `{reason}` | `{price}` |
| `POST /v1/merchant/catalog/publish` | Plan/apply the catalog-as-code manifest, mirroring `openrails push-merchant-catalog`. | `{catalog:{version,default_providers?,tier_groups},insert?,overwrite?,prune?,plan_only?}` | `{plan,result?,extras?}` |
| `GET /v1/merchant/catalog/drift` | List provider/catalog drift findings, including provider-side orphans. | `?provider=&kind=&resource_type=&limit=&offset=` | `{data:[finding],page}` |
| `POST /v1/merchant/catalog/drift/refresh` | Run drift detection now. | optional `{provider}` | `{summary}` |

Product/price writes should enqueue provider sync automatically where a provider
mirror is needed. Do not require a merchant admin UI to call per-object
`reconcile` routes after saving. Keep drift reads as ops visibility; provider
orphans are just `GET /v1/merchant/catalog/drift?kind=orphan`, not a separate
route. If manual repair is needed later, add one boring bulk repair endpoint
rather than per-product/per-price buttons.

`POST /v1/merchant/catalog/publish` is the HTTP form of the existing
`openrails push-merchant-catalog` CLI. The route is merchant-scoped from auth, so
the body should contain one catalog entry, not the CLI file's multi-merchant
`catalogs[]` wrapper. The `catalog` object is the same desired-state shape used
inside `config/catalog.example.yaml`: `default_providers`, `tier_groups`,
products, prices, and `provider_links`. Default behavior should be plan-only;
mutation requires explicit `insert`, `overwrite`, or `prune`, matching the CLI.

Customer support:

| Route | Purpose | Request | Response |
|---|---|---|---|
| `GET /v1/merchant/customers` | Search/list merchant customers. | query filters/page | `{data:[customer],page}` |
| `GET /v1/merchant/customers/:customer_id` | Customer billing/support profile, including trust tier. | optional `?currency=USD` | `{customer,balance_summary,trust_tier,active_entitlements,subscriptions}` |
| `GET /v1/merchant/customers/:customer_id/balance` | Per-currency balance. | `?currency=USD` | `{currency,balance}` |
| `GET /v1/merchant/customers/:customer_id/transactions` | Customer ledger history. | `?currency=&limit=&offset=` | `{data:[transaction],page}` |
| `GET /v1/merchant/customers/:customer_id/entitlements` | List active/history entitlement grants. | query filters/page | `{data:[entitlement],page}` |
| `GET /v1/merchant/customers/:customer_id/entitlements/:name` | Check one entitlement. | optional `?at=` | `{active,entitlement?}` |
| `POST /v1/merchant/customers/:customer_id/entitlements` | Manual entitlement grant. | `{entitlement,starts_at?,ends_at?,reason}` | `{grant,entitlements}` |
| `DELETE /v1/merchant/customers/:customer_id/entitlements/:grant_id` | Revoke manual grant. | optional `{reason,refund?}` | `{grant,revoked:true}` |
| `GET /v1/merchant/customers/:customer_id/products` | List owned/product access. | query filters/page | `{data:[product_access],page}` |
| `GET /v1/merchant/customers/:customer_id/products/:product_id/access` | Check one product. | none | `{has_access,access?}` |
| `POST /v1/merchant/customers/:customer_id/product-access` | Manual product ownership grant. | `{product_id,starts_at?,ends_at?,reason}` | `{grant,product_access}` |
| `DELETE /v1/merchant/customers/:customer_id/product-access/:grant_id` | Revoke manual product grant. | optional `{reason}` | `{grant,revoked:true}` |
| `GET /v1/merchant/customers/:customer_id/payments` | Customer payment history. | query filters/page | `{data:[payment],page}` |
| `POST /v1/merchant/customers/:customer_id/payments/off-channel` | Record manual/off-channel payment for an existing price and run the normal purchase side effects. | `{price_id,transaction_id,amount?,currency?,purchased_at?,discount_code?,discount_reason?,discount_metadata?}` | `{payment_id,entitlements,delayed_start,eligibility}` |
| `DELETE /v1/merchant/customers/:customer_id/payment-methods/:id` | Remove saved payment method for support. | none | `{deleted:true}` |
| `GET /v1/merchant/customers/:customer_id/subscriptions` | Customer subscriptions, current and historical. | `?status=&limit=&offset=` | `{data:[subscription],page}` |
| `POST /v1/merchant/customers/:customer_id/balance-adjustments` | Append-only support adjustment to the customer's prepaid balance; for comp credits, corrections, or migrations, not normal purchases. | `{currency,amount,reason,idempotency_key?}` | `{transaction}` |
| `PUT /v1/merchant/customers/:customer_id/credit-limit` | Set platform/merchant-approved arrears exposure for this customer. | `{currency,limit,reason}` | `{currency,limit}` |

Merchant-wide payments and subscriptions:

| Route | Purpose | Request | Response |
|---|---|---|---|
| `GET /v1/merchant/payments` | Search merchant payments. | query filters/page | `{data:[payment],page}` |
| `GET /v1/merchant/payments/:id` | Read payment detail. | none | `{payment}` |
| `POST /v1/merchant/payments/:id/refunds` | Refund payment through provider/ledger. | `{amount?,reason,revoke_access?}` | `{refund,payment}` |
| `GET /v1/merchant/subscriptions` | Search merchant subscriptions. | query filters/page | `{data:[subscription],page}` |
| `GET /v1/merchant/subscriptions/:id` | Read subscription detail. | none | `{subscription}` |
| `POST /v1/merchant/subscriptions/:id/cancel` | Stop future rebilling. | `{reason?,revoke_access?,effective_at?}` | `{subscription}` |

Usage/admission and policy:

| Route | Purpose | Request | Response |
|---|---|---|---|
| `POST /v1/merchant/admissions` | Batch authorize work and create holds. | `{items:[{payer,invoker?,resource?,currency,estimated_amount,idempotency_key?}]}` | `{items:[{request_id,admitted,reason?,hold?}]}` |
| `POST /v1/merchant/admissions/:request_id/capture` | Capture an admitted hold after work succeeds. | `{amount,usage?}` | `{transaction}` |
| `POST /v1/merchant/admissions/:request_id/release` | Release an admitted hold after failure/cancel. | optional `{reason}` | `{released:true}` |
| `POST /v1/merchant/wasted-spend` | Report provider spend that produced no billable result. | `{payer,invoker?,currency,amount,reason,resource?}` | `{recorded:true}` |
| `POST /v1/merchant/usage/rollup` | Usage/spend rollup for analytics/support. | `{customer_id?,currency,from,to,group_by}` | `{data:[row]}` |
| `POST /v1/merchant/usage/resource-revenue` | Revenue rollup by resource. | `{currency,from,to,resource?}` | `{data:[row]}` |
| `GET /v1/merchant/settings` | Read merchant-owned settings, including admission policy. | none | `{settings}` |
| `PUT /v1/merchant/settings` | Update merchant-owned settings as one document. | `{display?,checkout?,admission_policy?}` | `{settings}` |

Policy split:

- `settings.admission_policy.tier_schedule` is merchant-wide. Tensorhub uses it
  to declare the cumulative paid-spend ladder once; OpenRails auto-graduates each
  payer.
- `settings.admission_policy.tier_spend_limits` is merchant-wide default policy
  for a resolved payer tier: in-flight/held spend caps, single-charge caps, and
  payer wasted-spend windows.
- `settings.admission_policy.delegated_invoker_wasted_spend_limits` is
  merchant-wide. OpenRails can provide defaults, but multi-merchant deployments
  need a per-merchant override.
- Delegated spend authority is host/org policy, not a merchant-admin override.
  Tensorhub-style org delegation should stay in the host app and reach
  OpenRails through the embedded Go API or a future host-internal sync path only
  if standalone deployment needs it.
- If OpenRails owns the customer-facing UI for this, expose it as
  `/v1/orgs/:org_id/spend-delegations`, because the route must explicitly name
  the payer org/customer whose spend-sharing policy is changing. Do not allow
  individual/personal customer balances to be delegated.
- OpenRails should still trivially accommodate Tensorhub org-delegated spend:
  admission already has the right generic shape — payer/customer id, invoker,
  invoker type, tier, role UUIDs, estimated amount, request id, and resource.
  Tensorhub decides whether the invoker may spend the org's balance through
  AuthKit + Tensorhub policy; OpenRails meters the resulting windows and money.
- Merchant-wide invoker safety policy lives in settings, but payer authorization
  still lives on the payer/customer. Do not add a merchant-level "invoker may
  spend anyone's balance" grant.

Admission policy shape:

```json
{
  "admission_policy": {
    "currency": "USD",
    "tier_schedule": [
      {"tier": "free", "min_cumulative_paid_amount": 0},
      {"tier": "pro", "min_cumulative_paid_amount": 100000000}
    ],
    "tier_spend_limits": [
      {
        "tier": "free",
        "windows": [],
        "wasted_spend_windows": []
      }
    ],
    "delegated_invoker_wasted_spend_limits": []
  }
}
```

Ops reads:

| Route | Purpose | Request | Response |
|---|---|---|---|
| `GET /v1/merchant/repair-alerts` | Ledger/provider repair alerts. | query filters/page | `{data:[alert],page}` |

Routes to skip until a real caller needs them:

```text
POST /v1/merchant/customers/entitlements:batch
GET  /v1/merchant/entitlements/:name/customers
```

The first can come back as `POST /v1/merchant/entitlements:batch` if JWT
enrichment over HTTP needs it. The second is a directory/filter query, not a
core customer support workflow.

Rules:

- Customer rows should still be created naturally by checkout/usage/auth flows;
  admin routes may upsert by subject only when recording an off-channel payment
  or manual grant.
- Manual entitlements and product access should be recorded through the grant
  ledger with `source_type=admin`, explicit `starts_at`, optional `ends_at`,
  reason, and acting admin for audit.
- Off-channel payments are for payments tied to an existing catalog price. They
  use the same purchase registration path as checkout/webhooks so entitlements,
  product access, and idempotency stay consistent. Arbitrary free access uses
  the manual entitlement/product-access grant routes instead.
- Customer subscriptions should include current and historical subscriptions plus
  lifecycle dates the admin UI needs: started, current period start/end, cancel
  time, next retry/renewal where available, status, price/product, and processor
  references.
- Customer profile reads include the customer's auto-maintained trust/spend tier;
  do not keep a separate `GET /v1/merchant/customers/:customer_id/tier` route.
  Customer balance checks use `GET /v1/merchant/customers/:customer_id/balance`.
  `balance-adjustments` is append-only support/migration/correction credit,
  not a purchase path. `credit-limit` is explicit arrears exposure, not a
  billing-mode toggle. Tier is read-only and auto-maintained from spend/trust
  rules; do not add a merchant-admin tier override unless a real support
  workflow proves it is needed.
- Usage rollups are reporting/analytics, not core customer support CRUD, so keep
  them under `/v1/merchant/usage/*`.
- Refunds should not automatically revoke access. Revoking access is a separate
  admin decision unless the refund workflow explicitly asks for `revoke_access`.
- Cancel subscription means "do not rebill"; whether existing access is revoked
  immediately or ends at period end must be an explicit request flag.
- Deleting a payment method is support cleanup only; it must be scoped to the
  same merchant/customer and must not delete historical payment records.
- Payment-provider credentials are managed as payment-provider fields, not as a
  flat secret-name CRUD API. Admin UI can list configured status, update supplied
  fields, and delete/disable accounts; it never reads plaintext. Supplied
  credentials are validated immediately and are not stored if validation fails.

## Tasks

- [ ] Define/register only the OpenRails core permissions from #554 during bootstrap/config sync.
- [ ] Rely on AuthKit's single RBAC model to reject invalid permission/scope combinations: non-`platform:` permissions in platform roles and `platform:` permissions in org roles.
- [ ] Replace `PlatformOrgSlug()` route mounting with a normal platform-principal permission gate; platform routes should not depend on any org slug existing.
- [ ] Replace `/v1/platform/*` and `/v1/admin/merchants*` gates with specific OpenRails `platform:*` permission checks instead of one fake superadmin permission.
- [ ] Rename or remove `PermPlatformSuperadmin`; prefer concrete permissions plus `platform:*` expansion.
- [ ] Ensure delegated/browser merchant admin tokens cannot satisfy platform gates, even if they carry `platform:*`.
- [ ] Ensure platform principals cannot satisfy org-local merchant gates without explicit org-scoped authority for that merchant/org.
- [ ] Rename merchant-local permission constants/routes/docs to the chosen org-local OpenRails permission names where needed.
- [ ] Ensure standalone mode checks the bundled AuthKit control plane for both platform and org-local OpenRails permissions.
- [ ] Ensure embedded mode checks the same permission names from the host/AuthKit principal mapping.
- [ ] Move merchant-owned `/v1/service/*` routes to `/v1/merchant/*`; keep only a temporary compatibility alias if downstreams still require it.
- [ ] Make `/v1/merchant/*` routes accept every supported credential type that resolves to the owning org/merchant and required permission: logged-in user, delegated JWT, self-service/browser JWT with org authority, and API key.
- [ ] Replace route-level "service credential required" assumptions with a shared principal resolver for merchant routes.
- [ ] Remove nested `/admin` naming from merchant-owned routes; use resource paths such as `/v1/merchant/subscriptions/:id`, with permissions deciding who may call them.
- [ ] Normalize personal customer self-service routes under `/v1/me/*` and add org-customer-owned `GET/PUT /v1/orgs/:org_id/spend-delegations` for org balance sharing; personal customer balances must not be delegable.
- [ ] Replace `POST /v1/service/customers/by-external-subject/entitlements` with `POST /v1/merchant/customers/entitlements:batch`; remove `issuer` from the request once customer identity is `(merchant_id, subject)`.
- [ ] Split customer-forward lookups from directory/filter reverse lookups in both HTTP and Go APIs.
- [ ] Keep the Go library API and HTTP API names/shapes aligned so embedded and standalone hosts use the same concepts.
- [ ] Separate merchant route registration by deployment mode: standalone mounts merchant HTTP lookup APIs; embedded exposes the same operations primarily through the Go interface and avoids unnecessary host-internal HTTP routes.
- [ ] Replace/augment embedded `IncludeUser`, `IncludeAdmin`, `IncludeWebhooks` booleans with explicit named route sets or presets so hosts can see what should be mounted.
- [ ] Make embedded defaults exclude merchant host-internal HTTP lookup routes; provide an opt-in route set for HTTP parity.
- [ ] Audit Doujins, Tensorhub, and Cozy Art usage before removing compatibility aliases; expected common needs are account balance, check/list entitlements for JWT/auth, check/list product access, and Tensorhub `Admit`.
- [ ] Recut Tensorhub's OpenRails dependency into explicit admission, policy-sync, and admin-funding/reporting Go interfaces; keep HTTP only for standalone/remote mode.
- [ ] Make admission batch-native: one `POST /v1/merchant/admissions` endpoint and one Go method that both accept an item array; no separate single-admit route.
- [ ] Replace Tensorhub's broad `/v1/service/*` expectations with the smaller `/v1/merchant/*` route set above.
- [ ] Remove or compatibility-gate Tensorhub-unused service routes: `budget`, merchant configuration read, `abuse-usage`, credit-limit read, direct credit withdraw, transaction lookup, account settings, and generic credit transactions.
- [ ] Keep OpenRails invoicing, payment processor state, and arrears charging internal to OpenRails; Tensorhub should only configure limits/policies and consume ledger/admission results.
- [ ] Fold the existing delegated `/v1/admin/*` merchant-admin routes into the resource-named `/v1/merchant/*` surface; keep compatibility aliases only if downstreams still need them.
- [ ] Keep platform merchant provisioning routes separate from merchant self-admin routes: platform can create/delete/export/disable merchants; merchant admins can only manage their own merchant settings, payment providers, catalog, and customers.
- [ ] Replace direct merchant secret-key CRUD with payment-provider configuration routes; store individual credentials as write-only fields under the provider account internally.
- [ ] Cut generic `/v1/merchant/configuration`; use `/v1/merchant/settings` for merchant-owned settings and `/v1/merchant/payment-providers/*` for provider configuration.
- [ ] Implement `/v1/merchant/payment-providers` list/read/update/delete using provider names in the path and `{environment}` in query/body; keep `provider_accounts` as internal storage only.
- [ ] Make payment-provider update atomic: validate supplied credentials against the provider first, then store config/credentials only on success.
- [ ] Return payment-provider credential status as redacted field metadata (`configured`, `updated_at`, `last_validated_at` if available), never plaintext.
- [ ] Support test/live provider environments explicitly; enforce one active config per `{merchant, provider, environment}` until multiple active accounts are actually needed.
- [ ] Expose catalog-as-code publish/apply over HTTP with the same plan/apply engine as `openrails push-merchant-catalog`; HTTP is single-merchant because the merchant comes from auth.
- [ ] Add `POST /v1/merchant/catalog/publish` that accepts the inner single-merchant catalog manifest shape from `config/catalog.example.yaml`, not the CLI `catalogs[]` wrapper.
- [ ] Keep catalog publish plan-only by default; require explicit `insert`, `overwrite`, or `prune` for mutation, matching `openrails push-merchant-catalog`.
- [ ] Remove product/price per-object reconcile routes from the planned primary surface; product/price writes and catalog publish should enqueue safe provider sync automatically.
- [ ] Fold catalog orphan listing into `GET /v1/merchant/catalog/drift?kind=orphan`; do not keep a separate `/orphans` route.
- [ ] Add or normalize customer-management routes for merchant admins: customer profile/read, manual entitlement grant/revoke, product-access grant/revoke, off-channel payment record, payment refund, payment-method removal, and subscription cancel.
- [ ] Keep product access write routes named `/product-access`, not `/products`, because the written resource is the customer's access grant, not a catalog product.
- [ ] Make off-channel payment recording require `price_id` and `transaction_id`, then route through the existing normal purchase registration path for entitlements/product access/idempotency.
- [ ] Ensure customer subscription list returns current and historical subscriptions with lifecycle dates, status, product/price, processor refs, renewal/retry data where available, and pagination/status filtering.
- [ ] Move usage rollup out of customer CRUD into `/v1/merchant/usage/rollup` with optional `customer_id`.
- [ ] Keep `balance-adjustments` and `credit-limit` as explicit money admin writes, not balance reads; customer balance stays `GET /v1/merchant/customers/:customer_id/balance`.
- [ ] Make manual grants go through the grant ledger with acting-admin audit fields; do not write raw entitlement/product-access rows from HTTP handlers.
- [ ] Make refund and subscription-cancel workflows explicit about access revocation instead of silently coupling money reversal to entitlement revocation.
- [ ] Replace separate merchant policy routes with `GET/PUT /v1/merchant/settings`, containing merchant-owned admission policy as one document.
- [ ] Rename/update Go library policy methods to prefer one merchant settings call, e.g. `GetMerchantSettings` and `SetMerchantSettings`; do not add customer delegated-spend methods unless a real standalone host sync path requires them.
- [ ] Keep broad merchant-level "invoker can spend" grants out of the model; invoker/role spend authority must be attached to the payer/customer whose balance is being spent.
- [ ] Preserve OpenRails admission support for Tensorhub org-delegated spend budgets with generic fields: payer/customer id, invoker, invoker type, tier, role UUIDs, estimated amount, request id, and resource.
- [ ] Do not expose Tensorhub org delegated-spend budgets as OpenRails merchant-admin routes; org-customer-owned sharing belongs under `/v1/orgs/:org_id/spend-delegations`, while embedded Tensorhub/Cozy Art can keep their host-owned policy sync path.
- [ ] Keep `ResourceRevenueDaily` / `/v1/merchant/usage/resource-revenue` as reporting-only endpoint analytics, not admission settlement.
- [ ] Cut planned `GET /v1/merchant/manual-rebill-attempts`; rebill attempts are dunning/provider-intent events, so surface failures needing attention through `repair-alerts` and defer aggregate dashboard counts/history drill-downs to the future admin dashboard work.
- [ ] Add integration coverage proving platform RBAC is independent from merchant RBAC: platform admin can list merchants, merchant admin cannot; platform admin cannot mutate a merchant customer/subscription through merchant routes unless separately authorized.
- [ ] Add integration coverage for payment-provider credential validation failure proving invalid credentials are not persisted.
- [ ] Add integration coverage for `POST /v1/merchant/catalog/publish` plan-only and mutating modes against the same catalog engine as the CLI.
- [ ] Add integration coverage for remote merchant admission batch shape, capture, release, and wasted-spend against live HTTP server + DB/Redis stack.
- [ ] Add integration coverage for customer manual grants, off-channel payment registration, refund with explicit revoke flag behavior, and subscription cancel with explicit access behavior.
- [ ] Add integration coverage for settings/policy split: Tensorhub-style merchant settings install tier schedule, tier spend limits, and delegated-invoker wasted-spend defaults without exposing customer delegated-spend overrides as merchant-admin routes.
- [ ] Update docs and comments to use "platform RBAC" and "org-local merchant RBAC", not "platform org".

## Acceptance

- No OpenRails route requires or references a "platform org".
- Platform admin routes are protected by OpenRails-defined AuthKit `platform:*` permissions.
- Merchant admin routes are protected by OpenRails-defined AuthKit org-local permissions.
- Merchant-owned routes are exposed under `/v1/merchant/*`; `/v1/service/*` is deleted (hard cut, no alias).
- The same merchant permission gate works for regular users, delegated JWTs, self-service/browser JWTs with org authority, and API keys.
- Batch entitlement lookup no longer accepts or requires `issuer`; merchant/org is resolved from auth.
- Customer-forward lookup APIs are simple and mirrored between HTTP and Go.
- Standalone/remote merchant lookup routes are available over HTTP; embedded hosts can use the Go API without routing through HTTP.
- Tensorhub's hot path uses only admission/capture/release/wasted-spend/tier reads, with policy sync and admin funding/reporting kept separate.
- Tensorhub/Cozy Art org-delegated spend budgets can be enforced by OpenRails admission without adding merchant-admin delegated-spend override routes; if OpenRails owns the UI, it is org-customer self-service via `/v1/orgs/:org_id/spend-delegations`.
- Admission has one batch-shaped API; a one-item admission request uses the same route and response shape.
- Routes removed from the Tensorhub-required surface either have no downstream caller or are replaced by the OpenRails Go interface/customer/merchant surface.
- Merchant admin routes are resource-named under `/v1/merchant/*`, not hidden under credential-shaped `/v1/admin/*` or `/v1/service/*` paths.
- Merchant admins cannot create, delete, export, disable, or reassign merchants; those stay platform-only.
- Merchant customer-management actions are audited with the acting admin and scoped to the authenticated merchant/org.
- Merchant provider credentials are configured through payment-provider routes with redacted field status; direct secret-name routes are deleted (hard cut).
- Catalog publish/apply is available through both CLI and HTTP, using one shared engine and the same plan-only-by-default semantics.
- A platform role cannot contain org-local permissions, bare `*`, or negated permissions.
- A merchant role/token cannot grant `platform:*`.
- Integration tests prove the platform and merchant permission planes are disjoint.

---

# #330: nmi-immediate-subscription-checkout-stuck-pending

**Completed:** no
**Status:** IN_PROGRESS 2026-06-08: immediate NMI checkout activation patch is implemented; package tests and focused mock-provider regression pass; configured-account NMI recurring test has been added and compiles/skips locally because `NMI_SANDBOX_SECURITY_KEY` is not set. Remaining work: run real NMI credential test and repair/replay Paul2 after fixed code is deployed.

Fix the NMI subscription checkout path where an immediately approved recurring checkout is persisted as pending, leaving host apps such as Doujins stuck on the pending-subscription screen even though NMI accepted the transaction.

## Metadata

- Category: bug
- Status: in_progress
- Passes: false

## Live findings from Doujins/Paul2 on 2026-06-08

- OpenRails received the checkout request and `POST /v1/self/checkout` returned HTTP 200.
- NMI accepted the card vault and transaction: payment vault `314825442`, provider transaction `12162933364`, provider subscription `12162933429`.
- Local checkout session `019ea96b-221b-7274-a341-8cdc85cb72d6` was marked `succeeded` with processor `mobius` and amount `2300 usd`.
- Local subscription `019ea96b-223b-75b1-926b-35956d10eba5` stayed `pending`, with no current period start/end timestamps.
- Local payments only had a pending attempt row keyed as `nmi_sub_attempt:sub_cb204b034d2c3ec46be93b0470ff44df`; there was no completed payment row keyed by the real NMI transaction id.
- No billing entitlement rows were created for the user, so Doujins correctly kept showing the pending-subscription state.

## Root cause

`processNMISubscription` called `AddRecurringSubscription` successfully, but `completeNMISubscriptionRegistration` intentionally created a local pending subscription and returned a pending checkout response. The initial NMI response was not used to synchronously activate an immediate subscription, set the current billing period, create a completed payment, or grant entitlements. Webhooks should not be required for the initial happy path when NMI immediately approves the transaction; delayed/future starts can remain pending.

## Desired behavior

For immediate NMI subscription approvals, OpenRails should finish the local checkout atomically from the direct provider response: mark the subscription active, set current period timestamps, record the completed payment against the real provider transaction id, grant entitlements, and return a succeeded checkout response. Delayed-start subscriptions and genuinely asynchronous provider states should remain pending and rely on follow-up provider events/reconciliation.

**Tasks:**
- [x] Capture live-stack evidence for the Paul2 failure: NMI accepted the transaction, checkout succeeded, subscription stayed pending, payment stayed as a pending attempt, and no entitlement was granted.
- [x] Identify root cause in the NMI checkout finalization path.
- [x] Patch immediate NMI subscription finalization to activate the subscription, set period dates, create the completed payment, and grant entitlements without waiting for a webhook.
- [x] Preserve pending behavior for delayed/future-start NMI subscriptions.
- [x] Fix stale integration-test helpers that still query billing tables by `user_id` instead of `tenant_subject_id`.
- [x] Add/validate focused integration coverage proving an immediate NMI subscription checkout grants the premium entitlement synchronously.
- [x] Add an actual NMI test-account integration path so the live provider contract is exercised, guarded by NMI test credentials.
- [ ] Run the actual NMI configured-account recurring-subscription integration with `NMI_SANDBOX_SECURITY_KEY` set.
- [x] Run focused OpenRails checkout/module tests and NMI regression tests.
- [ ] After deployment/restart, repair or replay the affected live pending subscription row for Paul2 if it still exists.

---

# #328: robinhood-coinbase-usdc-funding-sessions

**Completed:** no
**Status:** PARTIAL 2026-06-08: Implemented Solana-only USDC funding session APIs, persistence, config, Coinbase hosted-session adapter with CDP JWT auth, Coinbase Hook0-signed Onramp webhook/status ingestion, Robinhood launch-template handoff, provider eligibility gates, self-service routes, idempotency, structured insufficient-USDC funding context on checkout errors, backend Solana USDC balance verification, focused tests, and DB-backed self-service API tests for create/get, merchant/user isolation, idempotency, and unsupported provider/network rejection. Retained for future provider integration work; current Doujins UX uses manual Robinhood/Coinbase links plus connected-wallet balance checks instead of OpenRails provider sessions. Remaining: real Robinhood partner adapter/status docs and access.

Plan and implement OpenRails-owned USDC funding sessions for host apps that need users to buy or transfer USDC into their own live Solana wallet before completing a Solana wallet checkout.

## Metadata

- Category: feature
- Status: partial
- Passes: false

## Goal

- OpenRails should expose a provider-backed funding-session API for Robinhood and Coinbase only. Host apps such as Doujins can ask OpenRails for a funding URL, send the user to the provider in a new tab or popup, then resume checkout after OpenRails and/or the host app verifies that the user's live Solana wallet has enough USDC.

## Product Behavior

- The user already has or creates a Robinhood/Coinbase account on the provider site.
- OpenRails does not custody funds and does not collect provider KYC; the provider handles account login, payment method, buy/transfer, KYC, and compliance.
- Provider redirect/return means the provider flow ended; it is not proof that the wallet is funded.
- Completion must be based on provider status/webhooks when available plus Solana wallet-balance verification.
- Only offer a provider when it can fund USDC on Solana. Coinbase/Base and all EVM chains are out of scope.

## Scope

- Implement Robinhood and Coinbase integration surfaces only for Solana USDC.
- Do not implement Ramp, Transak, MoonPay, PayPal, Venmo, Base, Ethereum, Polygon, Arbitrum, Optimism, or bridge paths for this issue.
- Keep provider abstraction narrow but extensible enough that more providers could be added later without changing the host-app contract.

**Tasks:**
- DESIGN:
- [x] Define the OpenRails funding-session contract for browser self-service callers: provider preference, wallet address, asset, network, requested amount, checkout_session_id, return_url, and idempotency key.
- [x] Define provider statuses and normalize them into OpenRails statuses such as created, opened, pending_provider, pending_settlement, funded, failed, expired, and cancelled.
- [x] Define Solana-only compatibility rules for USDC funding.
- [x] Decide whether funding amount comes from the checkout-session shortfall, an explicit requested amount, or both with server-side validation. Implemented explicit requested amount with optional checkout_session_id context.
- [x] Decide how provider ranking is configured per merchant: Robinhood preferred, Coinbase fallback. Implemented default provider order with Robinhood first and Coinbase second.
-
- DATA MODEL:
- [x] Add a funding/onramp session table with tenant_id, user_id, checkout_session_id, provider, wallet_address, asset, network, requested_amount, provider_session_id, provider_url, status, return_url, idempotency key, timestamps, and provider metadata.
- [x] Add indexes for tenant/user lookup, checkout_session_id lookup, provider_session_id lookup, and idempotency.
- [x] Store provider secrets/config in OpenRails config, never in host apps.
-
- API:
- [x] Add `POST /v1/self/usdc-funding-sessions` to create a Robinhood/Coinbase funding session for the authenticated browser user.
- [x] Add `GET /v1/self/usdc-funding-sessions/:id` to return normalized funding status and provider URL/status details safe for frontend polling.
- [x] Add `GET /v1/self/usdc-funding-options` to list eligible Robinhood/Coinbase options for wallet, network, asset, amount, and optional checkout_session_id.
- [x] Add provider webhook/status callback endpoints where Coinbase supports them. Implemented signed Coinbase Onramp webhook ingestion on the existing provider webhook route; Robinhood remains blocked on partner docs/access.
- [x] Enforce self-service auth, merchant boundaries, and idempotency on funding-session create/read routes.
-
- PROVIDERS:
- [x] Implement a Coinbase provider adapter that creates a hosted onramp URL/session with destination wallet, network, asset, amount, return URL, and partner/user reference, including short-lived CDP JWT bearer generation from Coinbase secret API keys.
- [x] Implement Coinbase status/webhook handling and map provider lifecycle into OpenRails funding-session status. Coinbase success maps to pending_settlement; only live Solana wallet-balance verification can mark funded.
- [ ] Implement a Robinhood provider adapter after partner docs/access are available, supporting external handoff into Robinhood Connect and funding into the user's live wallet.
- [ ] Implement Robinhood status/webhook handling if exposed by partner API; otherwise rely on return handling plus on-chain wallet-balance verification.
- [x] Add provider availability checks so unsupported network/asset combinations are hidden rather than offered.
-
- WALLET VERIFICATION:
- [x] Reuse existing Solana USDC balance-checking code where possible to verify the funded wallet before marking a session funded for Solana checkout.
- [x] Do not add Base/EVM balance verification for this issue; Solana is the only supported chain.
- [x] Ensure returning from a provider only triggers polling/checking; it must not mark the session funded by itself.
-
- CHECKOUT INTEGRATION:
- [x] Allow a funding session to reference the checkout session that produced an insufficient-USDC state.
- [x] Ensure insufficient-USDC API errors expose enough structured amount/network/wallet context for host apps to create a funding session. Added `error.metadata.usdc_funding` with Solana network, USDC asset, wallet, decimal amount/balance/shortfall, and base-unit values.
- [x] Keep final subscription/payment creation in the existing checkout confirmation path after the wallet is funded.
-
- VERIFY:
- [x] Add unit tests for provider eligibility and network compatibility gates.
- [x] Add API tests for create/get funding session, merchant isolation, idempotency, and unsupported-provider/network rejection.
- [x] Add provider adapter tests with mocked Coinbase responses.
- [x] Add wallet-balance verification tests proving redirect alone is insufficient through status semantics and frontend polling contract.
- [x] Document the host-app integration contract for Doujins in config.example.yaml and the tracker issue.

---

# #111: admin-rate-limiting

**Completed:** no

Add rate limiting to admin endpoints to limit blast radius of compromised JWT

## Metadata

- Category: security
- Status: not_started
- Passes: false

**Tasks:**
- STEPS:
- [ ] PROBLEM: If admin JWT is leaked, attacker could cancel/refund thousands of users before detection
- [ ] Implement per-admin-user rate limiting using Redis (keyed by admin user ID from JWT)
- [ ] Define rate limits for destructive operations:
-     - Cancellations: 5/minute, 10/hour, 50/day
-     - Refunds: 5/minute, 10/hour, 50/day
-     - Entitlement revocations: 5/minute, 10/hour, 50/day
- [ ] Define rate limits for bulk/expensive operations:
-     - Extend operations: 3/minute
-     - Off-channel payments: 10/minute
-     - Admin grants: 10/minute
- [ ] On rate limit exceeded: lock out admin for extended period (e.g., 1 hour) and alert
- [ ] Add alerting/notification when rate limits are approached (e.g., 80% threshold)
- [ ] Create admin rate limit middleware that wraps destructive endpoints
- [ ] Allow super-admin or manual override to unlock rate-limited admin if legitimate
- [ ] Log all rate limit events for security audit
- BENEFIT: Limits blast radius - attacker can only affect ~5-10 users before getting locked out

---

# #126: test-architecture-improvements

**Completed:** no

Tests should use real structs and functions from production code, not invent test-specific abstractions

## Metadata

- Category: testing
- Status: not_started
- Passes: false

## Details

- current_problems: ["SubscriptionOptions helper struct uses different field names than models.Subscription (PeriodStart vs CurrentPeriodStartsAt)","Tests can pass while using wrong field names because helpers translate between them","When a model field is renamed/removed, tests may not break because helper abstracts it away","Developers get confused about which struct to use and what fields are available"]
- example_bad: {"code":"suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{PeriodStart: now})","problem":"SubscriptionOptions.PeriodStart doesn't exist on models.Subscription"}
- example_good: {"code":"sub := &models.Subscription{CurrentPeriodStartsAt: now, ...}; suite.CreateTestSubscription(sub)","benefit":"Uses real model, compiler catches field name errors"}
- philosophy: {"principle":"Tests should verify actual production code behavior, not test-specific wrappers","goal":"When production code changes, tests should break if they're testing the affected behavior","anti_pattern":"Test helper structs that diverge from production models hide API changes and create maintenance burden"}
- recommendations: ["Use models.Subscription directly in tests instead of SubscriptionOptions","Use api.ListResponse[T] for parsing API responses instead of map[string]interface{}","Import and use the actual request/response structs from handlers","If a helper is needed, it should take the real model struct as input, not a parallel struct","Test assertions should use strongly-typed response parsing"]

**Tasks:**
- REFACTORING:
- [ ] Audit all test helper structs (SubscriptionOptions, PaymentMethodOptions, etc.)
- [ ] Replace helper structs with direct model usage where possible
- [ ] Update CreateTestSubscriptionWithOptions to accept *models.Subscription
- [ ] Use json.Unmarshal with actual API response types instead of map[string]interface{}
- [ ] Add linter rule or CI check to discourage map[string]interface{} in tests

---

# #158: ccbill-dunning-grace-entitlements

**Completed:** no

Adjust CCBill subscription entitlement logic to model paid term + retries (dunning) + end-of-term expiration. Use CCBill webhook fields like nextRetryDate/nextRenewalDate to drive subscription status and finite entitlement windows, and cap any grace extensions to avoid unlimited free access.

**Tasks:**
- SPEC:
- [ ] Verify CCBill webhook sequence in sandbox: cancel mid-term -> Expiration at end-of-term (and timing)
- [ ] Decide grace policy during retries: disabled vs enabled; cap strategy (extend-to-nextRetryDate vs fixed extra days)
- [ ] Decide policy for CCBill `Cancellation.source=failedRB`: treat as terminal end (no more retries) vs still paid-through end-of-term
-
- DATA MODEL:
- [x] Ensure we can store current_period_ends_at (from nextRenewalDate) for CCBill subscriptions
- [x] Store next_retry_at (from nextRetryDate) and optional grace_ends_at (policy derived)
- [x] Decide whether to reuse existing `subscriptions.next_retry_at` fields for CCBill (recommended) vs add CCBill-specific columns
-
- WEBHOOK -> STATE MACHINE:
- [x] NewSaleSuccess: set paid-term end; grant entitlements for [now, paid_term_end)
- [x] RenewalSuccess: extend paid-term end; extend entitlement windows
- [x] RenewalFailure: set past_due/dunning; persist next_retry_at from webhook; optionally extend entitlements within grace cap
- [x] Cancellation: mark cancelled but keep access until paid_term_end (do NOT revoke immediately)
- [x] Expiration: mark ended/expired and end/revoke entitlements
-
- CODE CHANGES (expected touch points):
- [x] `internal/services/webhook_ccbill.go`: parse and apply `nextRenewalDate` on `NewSaleSuccess` and `RenewalSuccess` (don’t rely solely on `price.billing_cycle_days`)
- [x] `internal/services/webhook_ccbill.go`: on `RenewalFailure`, parse `nextRetryDate` and update `subscriptions.status=past_due` + `subscriptions.next_retry_at` (avoid calling `SubscriptionLifecycleService.FailMembership`, which schedules NMI-style retries)
- [x] `internal/services/webhook_ccbill.go`: on `Cancellation`, call `CancelMembership` with `RevokeAccess=false` (today it passes true)
- [x] `internal/services/lifecycle_service.go`: update `CreateMembership` entitlement grant behavior for CCBill to create finite windows ending at `current_period_ends_at` (instead of indefinite `end_at=NULL`)
- [x] `internal/services/lifecycle_service.go`: update `RenewMembership` for CCBill to extend entitlements to match the new `current_period_ends_at` (today renewal doesn’t extend entitlements at all unless a downgrade changes entitlement set)
- [x] `internal/db/repo/entitlement.go`: make `IsEntitled` / “active entitlement” queries explicitly exclude `deleted_at` (don’t rely on implicit soft-delete behavior)
- [x] `config/config.go`: add a feature flag / config for CCBill grace behavior (enable/disable + max cap)
-
- ANALYTICS:
- [x] Ensure ClickHouse subscription_events reflect dunning/cancelled/expired transitions for CCBill
-
- TESTS:
- [x] Add integration test: NewSaleSuccess -> RenewalFailure(nextRetryDate) keeps access until paid_term_end (+ optional grace) -> RenewalSuccess extends window
- [x] Add integration test: Cancellation mid-term does NOT revoke immediately; entitlement ends at paid_term_end; verify Expiration handling if CCBill sends it
- [x] Add unit test for `EntitlementRepo.IsEntitled` to ensure deleted rows are excluded (regression guard)

---

# #164: cloudflared-managed-dev-tunnel

**Completed:** no

Make `Config.Cloudflared` an actual supported dev feature: OpenRails can (optionally) run and manage a Cloudflare Tunnel for local/dev, so a full local stack (e.g., multiple host apps + billing) can be accessed from an external device (phone) over HTTPS.

## Metadata

- Category: devx
- Status: planned
- Passes: false

## Goal

- With `cloudflared.tunnel_token` configured, a developer can start OpenRails and get a stable public hostname that routes to the local billing instance, usable from a phone browser/app.

## Non-goals

- Do not require Cloudflared in production.
- Do not make Cloudflared a hard dependency for boot.

## Notes

- This is for local/dev convenience (public ingress), not webhook signature bypass.
- Prefer explicit opt-in (config flag or separate command) so production never spawns subprocesses unexpectedly.

**Tasks:**
- DESIGN:
- [ ] Decide opt-in mechanism: (A) standalone CLI subcommand `openrails cloudflared up` that supervises both OpenRails + cloudflared, or (B) OpenRails spawns cloudflared when `cloudflared.tunnel_token` is set and `env=dev`
- [ ] Decide what constitutes “success”: tunnel established + /health/ready reachable via public hostname
-
- IMPLEMENTATION (if spawning subprocess):
- [ ] Add a small `pkg/cloudflared` supervisor that starts `cloudflared tunnel run` with the configured token and captures stdout/stderr into structured logs
- [ ] Ensure clean shutdown: propagate SIGINT/SIGTERM, kill child process group, avoid zombies
- [ ] Add a readiness check that probes the configured `cloudflared.public_hostname` (or local route) and reports status
-
- CONFIG / DOCS:
- [ ] Clarify config semantics in config.example.yaml (token is secret; hostname is non-secret; tunnel name optional)
- [ ] Document a phone-access workflow in docs (prereqs: cloudflared installed or image available; set APIURL/CORS origins appropriately)
-
- SECURITY / SAFETY:
- [ ] Ensure the service API (port 8060) is not accidentally exposed publicly by default; only expose the public API unless explicitly configured
- [ ] Ensure `api_key` and auth verification are still required for protected routes
-
- VERIFY:
- [ ] Add a lightweight unit test for supervisor command construction (no subprocess exec)
- [ ] Manual dev verification: start stack + tunnel and hit /health/ready from an external device

---

# #288: processor-routing-and-fallback-policy

**Completed:** no

Add deterministic processor selection and fallback policy so checkout can choose the best available rail for a merchant/product/tier/user context before creating a one-processor checkout session.

## Metadata

- Category: feature
- Status: planned
- Passes: false

## Motivation

- Hyperswitch wins on payment orchestration. OpenRails should not chase broad smart-routing, but customers do need basic processor redundancy and product/merchant-aware routing, especially high-risk merchants with Stripe plus NMI/CCBill/Solana.
- Keep checkout sessions one-processor-at-a-time; route before session creation.

## Non-goals

- No ML routing.
- No 200-connector orchestration platform.
- No automatic retry to a second processor after a successful authorization attempt unless the processor contract makes that safe.

**Tasks:**
- DESIGN:
- [ ] Define routing inputs: tenant_id, product_id, price_id, tier_group, amount, currency, billing cycle, user country/state when known, processor availability, processor capability metadata (#291), and explicit client preference.
- [ ] Define routing outputs: selected processor, fallback candidates, reason, and policy version.
- [ ] Decide precedence: explicit price/provider config > merchant policy > product/tier_group policy > global default.
- [ ] Decide failure classes that can trigger fallback before checkout finalization: processor unavailable, unsupported capability, credential missing, sandbox/live mismatch, hard validation failure. Do not fallback after a successful charge.
-
- DATA MODEL / CONFIG:
- [ ] Add routing policy representation in DB or catalog manifest: allowed processors, preferred order, disabled processors, and optional per-tier overrides.
- [ ] Extend catalog-as-code manifest only if needed; prefer using existing provider lists as the first version.
- [ ] Record selected processor and routing reason on checkout_sessions for auditability.
-
- IMPLEMENTATION:
- [ ] Add a ProcessorRouter service used before checkout session creation and legacy checkout flows.
- [ ] Integrate with processor health/capability metadata so unsupported processors are filtered with explicit errors.
- [ ] Add dry-run endpoint/CLI to explain routing for a price/user/merchant without creating a checkout.
- [ ] Ensure idempotency keys bind to the resolved processor/policy so retries do not unexpectedly switch rails.
-
- VERIFY:
- [ ] Unit-test policy precedence and fallback filtering.
- [ ] Integration-test Stripe primary -> NMI fallback when Stripe is disabled/unavailable before charge.
- [ ] Integration-test product constrained to NMI does not route to Stripe even if NMI is configured.

---

# #290: provider-certification-matrix

**Completed:** no

Publish and maintain a provider certification matrix for Stripe, NMI, CCBill, and Solana that records exactly which customer-visible flows are supported, sandbox/devnet tested, live/test-mode tested, and known-limited.

## Metadata

- Category: product
- Status: planned
- Passes: false

## Motivation

- OpenRails' strongest differentiator is deep support for real non-Stripe rails, especially NMI-compatible high-risk gateways. Customers need confidence that the specific flows they care about actually work.
- This should become both documentation and an executable certification harness where practical.

**Tasks:**
- MATRIX DESIGN:
- [ ] Define provider capabilities and certification statuses: not_supported, manual_only, unit_tested, integration_tested, sandbox_certified, live_test_mode_certified, devnet_certified, production_certified.
- [ ] Define flows to track: catalog/product push, price/recurring-plan push, one-time checkout, recurring checkout, vault/tokenization, rebill, cancellation, deferred cancellation, refund, dispute/chargeback, webhook handling, subscription sync/backfill, catalog drift detection.
- [ ] Include processor-specific notes: NMI product/prices are local while recurring prices push as NMI recurring plans; CCBill catalog actions may be manual; Solana recurring requires on-chain readback/devnet certification.
-
- DOCS:
- [ ] Add docs/providers.md or equivalent with the current matrix and exact tested commands.
- [ ] Add NMI-compatible gateway guidance: required security_key, Collect.js/tokenization key if needed, direct/query endpoints, test_mode behavior, and how white-label NMI accounts map to the same interface.
- [ ] Add troubleshooting for common provider failures: bad endpoint, key belongs to different gateway user, sandbox URL not supported, recurring plan query mismatch, webhook signature failure.
-
- EXECUTABLE CERTIFICATION:
- [ ] Add or formalize focused integration tests for NMI sale, vault, recurring plan create/readback, and query API.
- [ ] Add Stripe test-mode catalog command + subscription sync certification steps.
- [ ] Add Solana devnet read-back certification for plan accounts and recurring lifecycle.
- [ ] Add CCBill manual-action verification path so unsupported remote catalog operations surface as pending_manual_actions rather than errors.
-
- PROCESS:
- [ ] Make certification matrix updates part of provider-related PRs.
- [ ] Record last verified date, environment, and command for each provider flow without exposing secrets.

---

# #291: processor-capability-metadata

**Completed:** no

Expose processor capability metadata in code, APIs, catalog planning, checkout validation, and admin/provider status so OpenRails can explain what each rail supports instead of relying on scattered processor-specific conditionals.

## Metadata

- Category: architecture
- Status: planned
- Passes: false

## Motivation

- Stripe, NMI, CCBill, and Solana have different lifecycle semantics. Customers need predictable errors and routing decisions. OpenRails also needs a shared capability source for routing/fallback, catalog-as-code, checkout validation, and the provider certification matrix.

**Tasks:**
- CAPABILITY MODEL:
- [ ] Define ProcessorCapabilities with booleans/enums for recurring, one_time, vault/tokenization, hosted checkout, redirect checkout, direct sale, catalog push, recurring plan push, refund, dispute, cancel immediate, cancel deferred, remote subscription listing, remote dedup check, webhooks, drift enumeration, and manual actions.
- [ ] Add capability details for current processors: stripe, NMI-backed (`mobius`), ccbill, solana.
- [ ] Distinguish processor class capabilities (NMI-backed) from named provider overrides (Mobius).
-
- INTEGRATION POINTS:
- [ ] Use capabilities in checkout validation instead of hard-coded processor switches where practical.
- [ ] Use capabilities in catalog planning/reconciliation to decide provider actions and pending_manual_actions.
- [ ] Use capabilities in routing/fallback policy (#288) so unsupported rails are filtered before checkout.
- [ ] Surface capabilities through admin/provider status endpoints and docs.
-
- ERRORS / UX:
- [ ] Return structured unsupported-capability errors with processor, capability, and suggested alternative when possible.
- [ ] Ensure user-facing errors are clean while admin/debug surfaces retain processor detail.
-
- VERIFY:
- [ ] Unit-test capability metadata for each supported processor.
- [ ] Regression-test known special cases: CCBill manual catalog actions, NMI recurring plan push, Stripe remote dedup check, Solana one-off/recurring distinction.

---

# #320: Add Hyperswitch payment vault support for cloud and self-hosted deployments

**Completed:** no

Add Hyperswitch as an optional OpenRails payment provider and payment-method vault integration, covering both Hyperswitch Cloud and self-hosted Hyperswitch. This is payment vaulting/tokenization, not HashiCorp Vault merchant-secret storage. OpenRails should store only opaque Hyperswitch customer/payment-method identifiers plus non-sensitive metadata, while PAN/card collection stays in Hyperswitch-hosted/client-side tokenization flows or equivalent PCI-scoped Hyperswitch surfaces.

This issue must reconcile with future issue #297 (`deplatforming-resilient-card-vault`): Hyperswitch should not be positioned as the default adult/high-risk deplatforming-resilient vault until its contractual/export/compliance posture is explicitly certified. Hyperswitch Cloud may still be useful for lower-risk deployments or as an optional provider, and self-hosted Hyperswitch may be a break-glass/advanced deployment path if the operator accepts and satisfies the PCI requirements.

This should integrate with the provider capability and routing work in #288, #290, and #291: Hyperswitch can be selected as a processor/vault provider when the merchant/provider config says it is available, and OpenRails can route checkout/setup flows through it without treating it as a generic secret vault.

**Tasks:**
- [ ] Research the current Hyperswitch Cloud and self-hosted API surfaces needed for customers, payment methods, payment/setup intents, saved payment methods, refunds/voids, webhooks, connector routing, and vault/tokenization modes; record any version assumptions in docs.
- [ ] Reconcile the implementation plan with future issue #297: document whether Hyperswitch is an optional provider, a lower-risk deployment choice, or a PCI-heavy break-glass/self-hosted vault path; do not make it the default portable adult/high-risk vault without explicit certification.
- [ ] Define the OpenRails provider config shape for `hyperswitch`: cloud vs self-hosted mode, API base URL, optional vault/base URL split if Hyperswitch requires it, merchant/profile/account identifiers, API key secret reference, webhook secret reference, return/callback URLs, and test/live mode.
- [ ] Store Hyperswitch credentials in the existing merchant secret store path; do not store processor API keys in bootstrap YAML, database rows, logs, or generated frontend config.
- [ ] Extend provider capability metadata (#291) so Hyperswitch can advertise supported flows: payment-method vault/tokenization, one-time checkout, recurring/setup/mandate behavior if supported, refunds, webhooks, processor-side routing, remote payment-method listing, and manual certification status.
- [ ] Add a provider adapter/client abstraction that lets NMI customer-vault IDs and Hyperswitch payment-method IDs fit the same OpenRails payment-method model without hard-coding NMI semantics into checkout/subscription flows.
- [ ] Implement Hyperswitch customer/payment-method setup flow using an opaque token or hosted/client-side Hyperswitch collection result; persist only customer, provider, external customer ID, external payment method ID, brand/last4/expiry metadata, and status.
- [ ] Implement checkout/subscription charge paths that can use a saved Hyperswitch payment method and reconcile the resulting payment/subscription state back into OpenRails records.
- [ ] Implement webhook verification and event handling for successful payment, failed payment, refund/void, payment-method updates/deletes, and any recurring/mandate lifecycle events OpenRails relies on.
- [ ] Add self-hosted operational docs: required Hyperswitch services, base URL/TLS requirements, webhook reachability, secret injection, health checks, and local compose/dev smoke path if practical.
- [ ] Add cloud operational docs: required Hyperswitch Cloud credentials, webhook setup, provider certification checklist entry (#290), and merchant bootstrap/provider-link examples.
- [ ] Add tests with a mocked Hyperswitch server/client for tokenization/setup, saved-method charge, failure mapping, webhook signature verification, idempotency, merchant isolation, and no-sensitive-card-data persistence/logging.
- [ ] Validate with focused Go tests for provider/checkout/vault/webhook code, compile-only full package coverage, `task build`, and an optional live sandbox/self-hosted smoke test when credentials or local Hyperswitch are available.

---

# #331: Tune card-abuse rate-limit windows (#371): 15-min 3->captcha/6->block, 24h 10->block per user+ip, keep global 100->attack-mode

**Completed:** no

The #371 CardAbuseGuard (internal/modules/abuse/card_abuse.go), middleware subject-key plumbing, checkout hook, and integration tests already landed on master. This refines the policy to the agreed windows. Card-testing is defined by volume over time, so pair a short BURST window with a rolling DAILY cap, both keyed per user AND per IP. Final thresholds (user-specified): a 15-MINUTE rolling window per (user+ip) where 3 failed card charges -> future attempts require captcha, and 6 failed charges -> cut off (blocked) for the remainder of that 15-min window; PLUS a 24-HOUR rolling cap of 10 failed charges per (user+ip) -> cut off for the day. Keep the existing site-wide GLOBAL cap of 100 failed charges / 24h -> attack mode (captcha for every request from everyone).

**Tasks:**
- [ ] DefaultCardAbuseConfig: short window 15 min, CaptchaAfter=3, BlockAfter=6 (block lasts the remainder of the window)
- [ ] Add a second per-subject rolling 24h window with a cap of 10 failed charges -> block; the Limiter already returns multiple windows, so configure the daily window + add the second-window check in RecordChargeFailure
- [ ] Keep both subject keys (user: and ip:) so a logged-in attacker rotating cards and a bot rotating accounts behind one IP are both caught
- [ ] Keep the global 24h/100 attack-mode (captcha for everyone) unchanged
- [ ] Extend the abuse integration tests (real Redis) to cover the 15-min 3/6 thresholds and the 24h/10 daily cap; go build ./... + go vet ./... clean

---

# #333: admin-e2e-suite-needs-control-plane-wiring

**Completed:** no
**Status:** OPEN 2026-06-10 (Claude): diagnosed during the #332 failure review; not started. All other e2e failures from that review are fixed (see completed #332 + commits 9fe72d4 openrails, 68cb6a1 tensorhub, b72f61e8 cozy-art).

The admin e2e tests (tests/admin_payments_test.go, admin_metrics_test.go, admin_entitlements_source_test.go, admin_offchannel_payments_test.go) fail with 500 {'message':'authorization unavailable'} — the nil-admin-checker fail-closed path. DIAGNOSIS (2026-06-10): pre-existing #312 bit-rot, documented in the suite itself (testcontainer_suite.go Auth config comment: 'Admin-route integration tests therefore require the embedded control plane wired with the test admin granted openrails:admin'). The #312 hard-cut moved admin authority from JWT claims (tests still mint operatorAdminClaims() tokens via test_helpers.go) to the LIVE control-plane permission check (routes_admin.go only sets AdminPermissionChecker when s.controlPlane != nil; the suite never enables Auth.ControlPlane -> checker nil -> admin_neutral.go/ginmw admin gates 500 fail-closed).

WHAT THE FIX NEEDS:
1. suite.Config.Auth.ControlPlane = &config.ControlPlaneConfig{Enabled: true} (ginboot embcp.Attach then builds it; authkit profiles schema is already applied by migrate.RunPostgres).
2. The admin test users must EXIST in authkit profiles.users with ids equal to the JWT subs the suite mints, then be made members + granted the admin role (controlplane Bootstrap/AddMember/AssignRole or authkit core APIs — NOT raw SQL into roles). authkit core.CreateUser(email, username) generates its own id, so either (a) resolve the verifier's user mapping (issuer+sub -> user) and mint tokens for created users' real ids, or (b) add a test-only authkit user-provisioning hook.
3. operatorAdminClaims()/CreateAdminToken in tests/test_helpers.go are then vestigial — admin authority comes from the role grant, not claims.

NOTE: the admin-METRICS tests' 'Table test_analytics.daily_metrics does not exist' failures were a STALE TEST ENV artifact (migratekit records the ClickHouse ledger in postgres; reusing a postgres DB across runs against fresh CH containers skips re-apply). On a fresh env CH applies fine and those tests fail at the same admin-auth gate instead. Squashed CH baseline (cb9200b) is NOT at fault (daily_metrics present; migrations/clickhouse schema_test passes).

**Tasks:**
- [ ] Enable Auth.ControlPlane in testcontainer_suite config and verify embcp.Attach builds it in ginboot.
- [ ] Provision admin test users in authkit profiles.users matching the JWT subs (or mint tokens from created users' ids); AddMember + AssignRole openrails:admin via control plane / authkit core APIs.
- [ ] Re-run tests/admin_*.go on a fresh env; remove vestigial operatorAdminClaims if green.
- [ ] (env hygiene) consider failing loudly or re-validating when the migratekit CH ledger says applied but the CH database lacks the tables (stale-ledger detection).

---

# #340: e2e: 5 tier/lifecycle tests fail on master (pre-existing, surfaced by #334 verification)

**Completed:** no
**Status:** open (filed 2026-06-10 during #334 verification)

TestTierGroupDetection, TestEntitlementChangesOnTierChange, TestScheduledDowngrade, TestRenewMembershipDuplicateTransactionIsNoOp, TestLifecycleServiceUsesMockClock fail identically on origin/master (bun era) and on the sqlc-migration branch — verified via worktree runs on 2026-06-10 during #334 final verification, so they are NOT migration regressions. Observed modes: duplicate key on uq_subscriptions_tenant_subject_tier_group_active across subtests (CleanupSubscriptionsForUser resolves non-UUID test user ids to uuid.Nil via identity.TenantSubjectIDFromString, so per-user cleanup deletes nothing), and mock-clock/lifecycle assertion failures. Distinct from the admin_* family (#333). Suspect shared-suite state + the broken per-user cleanup; fix the cleanup to resolve the tenant subject through billing.tenant_subjects (issuer 'openrails:legacy-user') instead of pure-parsing.
