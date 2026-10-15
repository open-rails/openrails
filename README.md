# OpenRails Billing Engine

OpenRails is embedded into your Golang-webserver, or run as a stand-alone self-hosted application, and it owns your consumer's full billing state:

- The operator (you) creates a catalog of products + prices, and what they grant when owned (access to digital content usually).
- Create an order for each purchase
- Tokenize the user's card (browser -> vault, so that your webserver never sees sensitive card details)
- Charge the vaulted-card initially
- Charge again on rebill date
- If charges fail with a recoverable error, perform manual-dunning to retry again

OpenRails replaces most of Stripe and every other payment processor. OpenRails is not a PSP (payment service provider) you still need a PSP to charge the actual card (such as Stripe); OpenRails merely replaces all of the PSP-specific logic (catalog, billing cycles, manual dunning) with a single standardized system that you own and fully control. This means you can vault cards yourself, and switch between PSPs or use multiple PSPs easily without a new custom-integration project for each PSP.

OpenRails acts as the source of truth for your user's purchases (what items have they bought, what membership tier do they hold?) so your application doesn't need to track that.

OpenRails can also do usage-based metering and rate-limiting; for example, allocate premium-tier users to only 100-AI-gen credits per week, and OpenRails handles the state + rate limiting. As another example, you can meter a user's API-usage for the month, produce an invoice, and then charge their stored payment method on file.

### Example Apps:

- SaaS products: Define your tier-list of plans customers can buy, and then bill them per seat. Your customers can upgrade/downgrade their plan with proration.
- Adult sites: Your users buy recurring subscriptions; we handle the subscription-lifecycle (prevent duplicate subscriptions, cancellation, manual-dunning when rebill fails, etc.), and we act as the source of truth of which users have access to what (entitlements). Build your own OnlyFans, Pornhub, Patreon, etc.
- Digital Storefronts: Your users buy videos / courses / downloads individually. Define your own products + prices; we manage ownership-records. Build your own Shopify, Gumroad, etc.
- API Platform or Cloud: Your users top-up their API balance ahead of time, and then you drawdown that balance as requests arrive. Or you enable arrears-billing for your users, and keep a ledger of their usage and compile them into regularly monthly invoices.

OpenRails integrates with several payment-processors:

- Any PSP using an NMI-payment gateway. Examples: PaymentCloud, PayKings, SoarPay, Zen Payments, etc.
- Stripe
- CCBill
- Solana with USDC (crypto)

---

### How to Install (Embedded)

 We'll embed OpenRails the library into your Go webserver (you can alternatively run OpenRails as its own standalone server separately--see below). In this example, we'll have a simple site where users register / login with [AuthKit](https://github.com/open-rails/authkit) and buy CSS courses individually or as a bundle, and buy a recurring monthly or annual membership.

First install:

```sh
go get github.com/open-rails/openrails
```

Next, declare your catalog of products + prices as a YAML config file:

```yaml
# catalog.yaml
schema_version: 1 # the file format
products:
  course-101:
    display_name: Course 101 — Intro to CSS
    entitlements: ["course:101"]
    prices:
      purchase:
        amount: 4.99 USD
        access_duration: forever # permanent access
      rent:
        amount: 1.99 USD
        access_duration: 3 days # access expires 3 days after purchase

  course-102:
    display_name: Course 102 — Intro to Tailwind
    entitlements: ["course:102"]
    prices:
      purchase:
        amount: 4.99 USD
        access_duration: forever
      rent:
        amount: 1.99 USD
        access_duration: 3 days

  course-bundle:
    display_name: CSS courses 101 and 102
    entitlements: ["course:101", "course:102"] # the same keys as individual sales
    prices:
      purchase:
        amount: 8.99 USD # both courses at a discounted price
        access_duration: forever

  channel-membership:
    display_name: Channel membership
    entitlements: ["channel:membership"]
    prices:
      monthly:
        amount: 10 USD
        access_duration: 30 days
        billing_interval: 30 days
      yearly:
        amount: 99 USD
        access_duration: 365 days
        billing_interval: 365 days
```

Products and their prices are maps keyed by a stable key you choose. The entitlements are arbitrary strings you choose; OpenRails tracks these, but it's up to your application to give them meaning. Above, we created strings for our membership and for individual products.

A product may also list `required_entitlements`: keys the customer must already hold to order it. A members-only course with `required_entitlements: ["channel:membership"]` sells only to members; anyone else's order, and a purchase paid after their membership ended, is refused `403 entitlement_required`, with the keys they lack in `metadata.missing`.

#### Build the billing client

Next, configure your merchant. This contains secrets that allow OpenRails to talk to your PSP (payment service provider).

```yaml
# merchant.yaml
slug: onlydemo # your merchant's name: lowercase letters, digits and hyphens
display_name: OnlyDemo
psps:
  paykings: # your name for this account; any key you like
    rail: nmi # the gateway kind: nmi, stripe, ccbill or solana
    environment: sandbox # live (real money; the default), test (an NMI account in test mode) or sandbox (a sandbox.nmi.com account)
    gateway_id: "000000" # the NMI dashboard's Gateway ID
    tokenization_key: your-public-tokenization-key # the browser's card fields use it
    secrets:
      security_key: your-private-security-key
      webhook_signing_secret: your-webhook-signing-key
```

```go
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/authkit"
	authkitgin "github.com/open-rails/authkit/adapters/gin"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/authkit/verify"
	"github.com/open-rails/openrails"
	openrailsgin "github.com/open-rails/openrails/adapters/gin"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
)

// issuer is your AuthKit's: the tokens it issues act at your merchant.
const issuer = "https://shop.example"

func newBilling(ctx context.Context, db *pgxpool.Pool, ak *authkit.Client) (*openrails.Engine, *openrails.Client, error) {
	// Load your PSP-configs from above
	merchant, err := openrails.ReadMerchantFile("merchant.yaml")
	if err != nil {
		return nil, nil, err
	}

	merchant.Issuer = issuer // your users, signed in at your AuthKit, are this merchant's customers and staff
	cfg := openrails.Config{
		Database:          openrails.DatabaseConfig{Schema: "billing"}, // the Postgres schema OpenRails' tables go in
		ProviderWriteMode: openrails.ProviderWritesFull,                // Set to ProviderWritesReadOnly to prevent any billing
		Merchants:         map[string]openrails.MerchantDeclaration{merchant.Slug: merchant},
	}

	// This command will automatically apply its migrations and return the billing engine
	bill, err := openrails.New(ctx, cfg, openrails.Deps{
		Postgres: db, // the same pool your app uses
		UserInfo: ak.UserInfo(), // AuthKit's user directory; provides customer contact details and staff search
	})
	if err != nil {
		return nil, nil, err
	}
	// Your merchant's handle: the Go client your own code calls, in process.
	shop, err := bill.MerchantBySlug(merchant.Slug)
	if err != nil {
		_ = bill.Close(context.WithoutCancel(ctx))
		return nil, nil, err
	}
	return bill, shop, nil
}
```

#### Apply your catalog

Starting the server never changes what you sell. Give your app a catalog
command (here `myapp catalog apply catalog.yaml`) and run it once to load
`catalog.yaml`, then again whenever the file changes, like a deploy:

```go
// applyCatalog applies a catalog file and prints what changed. Applying the
// same file again changes nothing.
func applyCatalog(ctx context.Context, shop *openrails.Client, path string) error {
	products, err := catalog.ReadFile(path)
	if err != nil {
		return err
	}
	receipt, err := shop.ApplyCatalog(ctx, products, billing.ApplyCatalogParams{})
	if err != nil {
		return err
	}
	return openrails.WriteCatalogReceipt(os.Stdout, receipt)
}
```

A field someone changed in the console since is not overwritten: the receipt
lists it ([Catalog files and edits together](docs/catalog-ownership.md)).

#### Guard its routes with your auth

OpenRails has no auth of its own. When you mount its routes you pass in your auth, which only says who a request is; OpenRails decides what each route admits and answers refusals itself. AuthKit provides this as `ak.Authenticator()`.

The permissions are OpenRails' own, exported as constants (`billing.Permissions()` lists them), so your roles cannot drift from the names its routes check. First let's declare them in AuthKit and define two staff roles; staff are just regular users given a role. Support can see and help customers: read their billing history and make changes such as cancelling a membership or issuing a refund. Operators can do all of that and also edit the catalog, configure billing, and see the business metrics (revenue, sales) that support cannot.

```go
rbac := authkit.NewRoles()

// OpenRails' permissions, by its own names. Held on root, they reach every
// merchant you serve.
perm := func(p billing.Permission) iam.Perm { return rbac.Root.Permission(p.Resource(), p.Action()) }
billingRead, billingManage := perm(billing.RootCustomerRead), perm(billing.RootCustomerManage)
catalogManage, configManage, metricsRead := perm(billing.RootCatalogManage), perm(billing.RootConfigManage), perm(billing.RootMetricsRead)

rbac.Root.Role("support", billingRead, billingManage)
rbac.Root.Role("operator", billingRead, billingManage, catalogManage, configManage, metricsRead)
```

Then build AuthKit with those roles:

```go
// Persist this deployment's account directory UUID; never generate it at boot.
tenant, err := iam.ParseTenantID(os.Getenv("AUTH_DEFAULT_TENANT_ID"))
if err != nil {
	return err
}
ak, err := authkit.New(ctx, authkit.Config{
	DefaultTenantID: tenant,
	Database: authkit.DatabaseConfig{Schema: "profiles"}, // AuthKit's tables, beside OpenRails' "billing"
	Token:    authkit.TokenConfig{Issuer: issuer, IssuedAudiences: []string{"myapp"}},
	Keys:     authkit.KeysConfig{Path: "/vault/auth"}, // its signing keys
	Roles:    rbac,
	HTTP:     &authkit.HTTPConfig{RefreshCookie: true}, // the browser stays signed in across reloads
}, authkit.Deps{Postgres: db}) // the same pool OpenRails uses
if err != nil {
	return err
}
```

Finally, mount OpenRails' routes with AuthKit guarding them:

```go
err = openrailsgin.Mount(r, bill, openrails.Routes{
	Auth:   ak.Authenticator(), // says who a request is; required. Public, customer (/v1/me) and webhook routes are always on
	Prefix: "/billing",         // openrails routes will be mounted at mydomain.com/billing/v1/...

	// What optional routes to mount
	RouteGroups: openrails.RouteGroups{
		Admin:          true,  // customer support: customers, their subscriptions, orders, payments, refunds and cancellation
		Catalog:        false, // the catalog: update products and prices
		MerchantConfig: false, // merchant config: update PSPs and their credentials, findings, settings, billing import and export
		Metrics:        true,  // business metrics, read-only: ARR, the dashboard's charts
		Programmatic:   false, // /v1/app: your backend's own calls over HTTP, each route with its own permission; embedded, your Go code calls the Client instead
	},

	// No permissions to pass: each route checks OpenRails' own pair (billing.Permissions()),
	// its merchant: name in the merchant's group or its root: name in your root group.

})
```

#### Routes

These are OpenRails' routes that you just mounted. In this example, the full routes will look like `mydomain.com/billing/v1/config`:

**Public** (always mounted; requires no user auth)

| Route | What it does |
|---|---|
| `GET /v1/config` | how this deployment is set up: which route groups and features are on, the currency and rail registries, each PSP's public browser config, Solana tokens, and the captcha to solve when asked (billing-ui reads it once) |
| `GET /v1/orders/{id}/solana-pay` | Called by the customer's wallet app (from the QR code), not the browser; no sign-in. Returns the merchant's name and icon. Not mounted if Solana is not configured. |
| `POST /v1/orders/{id}/solana-pay` | The wallet sends its address and gets back the transaction to sign; it signs and submits it to Solana itself, and OpenRails watches the chain for the payment, then completes the order. No sign-in; not mounted if Solana is not configured. |

**Customer** (always mounted; requires user auth for this to work)

| Route | What it does |
|---|---|
| `GET /v1/me/cart` | their cart, priced now with the ways it can be paid |
| `DELETE /v1/me/cart` | empty their cart |
| `PUT /v1/me/cart/lines/{price_id}` | set a line's quantity (0 removes it) |
| `POST /v1/me/orders/preview` | the total and the ways it can be paid; changes nothing |
| `POST /v1/me/orders` | buy lines or the cart, paying in the same call with a saved or new card, or creating the order to pay next, with its embedded pay URL on the checkout origin when asked (`pay_url`: framed by the merchant's registered origins, as long as the order is open); asked again for the same lines, it answers the customer's open order |
| `GET /v1/me/orders` | their orders: purchases, subscription renewals and usage periods |
| `GET /v1/me/orders/{id}` | one order |
| `POST /v1/me/orders/{id}/pay` | pay an open order (a failed renewal or a usage period included), with a saved or new card |
| `POST /v1/me/orders/{id}/confirm` | finish it after the processor's step (3-D Secure, a redirect, a wallet) |
| `POST /v1/me/orders/{id}/cancel` | cancel an unpaid order |

| `GET /v1/me` | their balance per currency and the card that pays each currency |
| `GET /v1/me/entitlements` | everything they currently have access to |
| `GET /v1/me/subscriptions` | their subscriptions |
| `GET /v1/me/subscriptions/{id}` | one subscription, with any change waiting for renewal |
| `POST /v1/me/subscriptions/{id}/cancel` | cancel at period end (with a reason) |
| `POST /v1/me/subscriptions/{id}/resume` | undo a cancellation before the period ends |
| `POST /v1/me/subscriptions/{id}/change/preview` | what it would cost; changes nothing |
| `POST /v1/me/subscriptions/{id}/change` | change tier, or seats on a per-seat price: upgrades charge the difference now, downgrades wait for renewal, and changing back to what they have cancels a waiting downgrade |
| `PUT /v1/me/subscriptions/{id}/payment-method` | pay a subscription with another saved card, or with their default again |
| `GET /v1/me/payment-methods` | their saved cards, newest first, with the agreements each carries |
| `POST /v1/me/payment-methods` | add a card without buying: the PSP's own card fields turn it into a token, and this saves it (`replaces`: put it in an old card's place everywhere) |
| `POST /v1/me/payment-methods/{id}/confirm` | finish saving a card after the bank's 3-D Secure step |
| `PATCH /v1/me/payment-methods/{id}` | change a card's expiry, billing details or reuse |
| `DELETE /v1/me/payment-methods/{id}` | remove a card |
| `PUT /v1/me/default-payment-methods/{currency}` | choose the card that pays one currency: its renewals and usage, and every subscription without its own card |

| `GET /v1/me/payments` | card payments and refunds |
| `GET /v1/me/balance/transactions` | balance movements: top-ups, usage spent, expiries |
| `GET /v1/me/usage` | metered usage this period |
| `POST /v1/me/stripe/billing-portal-sessions` | open Stripe's own billing portal, for subscriptions Stripe runs (mounted only when a Stripe PSP is declared) |

**Admin: customer support** (with `RouteGroups.Admin`; your staff's ability to view and fix customer's accounts)

Each route needs `merchant:customer:read` or `merchant:customer:manage` (or its `root:` twin), as its last column says. Admin actions do not require a recent sign-in.

| Route | What it does | Needs |
|---|---|---|
| `GET /v1/admin/customers` | customers (`?ids=`; `?search=`: by email, username or name) | read |
| `GET /v1/admin/customers/{customer_id}` | one customer: email, username and name (from your directory), settings, balance per currency, and the card that pays each currency | read |
| `PATCH /v1/admin/customers/{customer_id}` | change a customer's credit limit, trust level, billing policy and invoice profile | manage |
| `GET /v1/admin/customers/{customer_id}/payment-methods` | a customer's saved cards, each with the agreements (mandates) it carries, ended ones included: evidence for disputes | read |
| `GET /v1/admin/customers/{customer_id}/balance` | prepaid credit balance | read |
| `GET /v1/admin/customers/{customer_id}/balance/transactions` | balance movements | read |
| `GET /v1/admin/subscriptions` | subscriptions (`?dunning=true`: renewals we are still retrying or waiting on a new card for, each with its `dunning`: retries made and left, the next and the final retry) | read |
| `GET /v1/admin/subscriptions/{id}` | one subscription, with its `scheduled_change` | read |
| `POST /v1/admin/subscriptions/{id}/cancel` | cancel | manage |
| `POST /v1/admin/subscriptions/{id}/resume` | undo a cancellation | manage |
| `POST /v1/admin/subscriptions/{id}/change/preview` | what it would cost; changes nothing | read |
| `POST /v1/admin/subscriptions/{id}/change` | change tier or seats when the customer asks support: upgrades charge the difference now, downgrades wait for renewal, changing back to the current plan cancels a waiting change; needs a `reason`, and emits a billing fact for application-owned delivery | manage |
| `PUT /v1/admin/subscriptions/{id}/payment-method` | move a subscription to another of the customer's saved cards, or back to their default | manage |
| `GET /v1/admin/orders` | orders: purchases, renewals and usage periods, and where an unpaid one stopped (`?customer_id=`, `?price_id=`, `?status=`; `?overdue=true`: unpaid past their due date, each saying whether its grace has run out and new usage is refused; they stay until paid, voided or written off) | read |
| `GET /v1/admin/orders/{id}` | one order | read |
| `GET /v1/admin/payments` | payments and refunds (`?customer_id=`, `?subscription_id=`, `?order_id=`, `?status=`) | read |
| `GET /v1/admin/payments/{id}` | one payment or refund | read |
| `POST /v1/admin/payments` | record money received outside OpenRails for an order | manage |
| `POST /v1/admin/payments/{id}/refunds` | refund | manage |
| `GET /v1/admin/payment-attempts` | every attempt to charge, declines included | read |
| `GET /v1/admin/payment-attempts/{id}` | one attempt | read |
| `GET /v1/admin/renewals` | renewal cycles and their charges | read |
| `GET /v1/admin/renewals/{id}` | one renewal cycle | read |
| `POST /v1/admin/orders/{id}/mark-uncollectible` | write off an unpaid renewal or usage order | manage |
| `GET /v1/admin/entitlements` | who has which entitlement, up to 100 customers at once (`?customer_id=`, `?entitlement=`, `?prefix=`) | read |
| `GET /v1/admin/product-access` | products customers hold, bought or granted (`?customer_id=`, `?product_id=`) | read |
| `POST /v1/admin/product-access` | grant a product without payment | manage |
| `POST /v1/admin/product-access/{id}/revoke` | take a grant back | manage |
| `GET /v1/admin/credit-grants` | credits granted (`?customer_id=`) | read |
| `GET /v1/admin/credit-grants/{id}` | one credit grant | read |
| `POST /v1/admin/credit-grants` | grant credits | manage |
| `POST /v1/admin/credit-grants/{id}/revoke` | revoke a grant | manage |

**Catalog management** (with `RouteGroups.Catalog`; used to update the catalog. Off in this example, where the catalog command applies `catalog.yaml`)

Catalog reads need `merchant:catalog:read`; changes need `merchant:catalog:manage` (or their `root:` twins). These routes also accept backend application credentials with the corresponding permission. They do not require a recent sign-in. They work alongside a catalog file: see [Catalog files and edits together](docs/catalog-ownership.md).

Prices are refernce by a stable key (ex: `pro-monthly`). Prices can change; the old versions stay available for existing subscribers and subscribers are migrated to the new price.

| Route | What it does |
|---|---|
| `GET /v1/catalog/products` | products (`?keys=`, `?entitlement=`) |
| `GET /v1/catalog/products/{id}` | one product |
| `GET /v1/catalog/prices` | prices (`?product_key=`, `?key=`) |
| `GET /v1/catalog/prices/{id}` | one price |
| `GET /v1/catalog/prices/{id}/history` | which price this price's key has pointed to, and from when |
| `GET /v1/catalog/meters` | meters and their rates |
| `GET /v1/catalog/meters/{key}` | one meter and its rates |
| `POST /v1/console/catalog/ask` | ask the standalone console’s AI assistant about the catalog or to draft a change (with an AI provider configured) |
| `POST /v1/catalog/apply` | apply a whole catalog document (renaming an entitlement across products included) |
| `POST /v1/catalog/products` | create a product |
| `PATCH /v1/catalog/products/{id}` | change or archive a product |
| `DELETE /v1/catalog/products/{id}` | delete a product no one has ever bought; once purchased, a product stays (archive it instead) |
| `POST /v1/catalog/prices` | create a price |
| `PATCH /v1/catalog/prices/{id}` | change or archive a price |
| `PUT /v1/catalog/meters/{key}` | create or change a meter and the rates that price its usage |
| `POST /v1/catalog/price-migrations/preview` | who it affects; changes nothing |
| `POST /v1/catalog/price-migrations` | move subscribers from one price, or every old version of a price, to another, at renewal or on a date |
| `GET /v1/catalog/price-migrations` | those migrations |
| `GET /v1/catalog/price-migrations/{id}` | one migration |
| `POST /v1/catalog/price-migrations/{id}/cancel` | cancel one |

**Merchant operations** (with `RouteGroups.MerchantConfig`; off in this example)

Every route needs `merchant:config:manage` (or `root:config:manage`). These operator routes do not require a recent sign-in.

| Route | What it does |
|---|---|
| `GET /v1/merchant/psps` | your PSPs |
| `POST /v1/merchant/psps` | add a PSP |
| `GET /v1/merchant/psps/{id}` | one PSP |
| `PATCH /v1/merchant/psps/{id}` | change a PSP's settings or credentials, or retire it |
| `POST /v1/merchant/psps/routing-preview` | which PSP an order for a price would use |
| `POST /v1/merchant/psps/refresh` | re-read every PSP now: renewals, declines, cancellations, card changes and catalog drift |
| `GET /v1/merchant/findings` | problems OpenRails found that need an operator, catalog drift included (`?type=`) |
| `GET /v1/merchant/findings/{id}` | one finding |
| `POST /v1/merchant/findings/{id}/resolve` | resolve one |
| `GET /v1/merchant/configuration` | your name, settings, their revision and your API host |
| `PATCH /v1/merchant/configuration` | change your name or settings |
| `GET /v1/merchant/alert-webhooks` | where operator alerts are sent |
| `POST /v1/merchant/alert-webhooks` | add an alert webhook |
| `PATCH /v1/merchant/alert-webhooks/{id}` | change an alert webhook |
| `DELETE /v1/merchant/alert-webhooks/{id}` | remove an alert webhook |
| `GET /v1/merchant/provisioning-tokens` | tokens for a directory like Okta or Entra ID to push users with; they open only the SCIM routes |
| `POST /v1/merchant/provisioning-tokens` | create one (its secret is shown once) |
| `DELETE /v1/merchant/provisioning-tokens/{id}` | revoke one |
| `GET /v1/merchant/billing-archive` | export your billing |
| `POST /v1/merchant/billing-archive` | restore an export into an empty book |
| `POST /v1/merchant/billing-import` | import billing from another system |
| `PUT /v1/merchant/dashboard` | save the metrics dashboard's layout (the console offers it to staff who also hold `Metrics`) |

**Business metrics** (with `RouteGroups.Metrics`; read-only)

Every route needs `merchant:metrics:read` (or `root:metrics:read`). Each only reads; a metrics query is a `POST` for its body.

| Route | What it does |
|---|---|
| `POST /v1/metrics/query` | run a metrics query (revenue, usage, open findings and more) |
| `GET /v1/metrics/schema` | what a query may use |
| `GET /v1/metrics/dashboard` | read the saved dashboard layout and widget definitions |

Every route, with its permission, request and response, is in the [route table](docs/api/routes.md); each admin and merchant-config route has one method on the Go `Client`, and the conventions are in the [API guide](docs/api/endpoints.md).

**Programmatic** (with `RouteGroups.Programmatic`; your backend server, never a person)

When running in embedded mode (as in this example) you wouldn't usually mount these routes; you would just use the Go client instead. These routes are necessary for when (1) you are running embedded, but need some external service to access these commands, or (2) OpenRails is running in standalone mode, or it's hosted by us (more on that below), in which case the Go client is operating over HTTP.

| Route | What it does | Needs |
|---|---|---|
| `POST /v1/app/orders/preview` | its total and how it can be paid; changes nothing | Orders |
| `POST /v1/app/orders` | an unpaid order for one of your users, which they pay on your site; it never charges | Orders |
| `POST /v1/app/orders/{id}/cancel` | withdraw an unpaid order you created | Orders |
| `POST /v1/app/entitlements/check` | whether a customer holds entitlements, to gate your content | Entitlements |
| `POST /v1/app/usage-events` | record metered usage, failed work included | Usage |
| `POST /v1/app/admissions` | admit requests against a customer's budget, holding funds | Usage |
| `POST /v1/app/admissions/capture` | settle requests at what the work used, priced by the rate cards | Usage |
| `POST /v1/app/admissions/release` | free holds | Usage |
| `POST /v1/app/admissions/extend` | move holds' deadlines | Usage |
| `POST /v1/app/running-charges` | charge a customer at a rate per hour until you stop it | Usage |
| `POST /v1/app/running-charges/{reference}/stop` | stop it and post what it charged | Usage |
| `GET /v1/app/host-events` | events for your backend to process, oldest first | Events |
| `POST /v1/app/host-events/acknowledge` | mark them processed | Events |
| `POST /v1/app/notification-consumers/{name}` | register an independent billing notification consumer | Events |
| `GET /v1/app/notification-consumers/{name}/events` | pending billing facts for this consumer | Events |
| `POST /v1/app/notification-consumers/{name}/acknowledge` | acknowledge facts after the application persists them | Events |
| `DELETE /v1/app/notification-consumers/{name}` | retire a consumer and release its retention claim | Events |
| `POST /v1/app/scim/v2/Bulk` | your directory's batch of user changes (AuthKit sends one every 5 minutes) | — |
| `POST /v1/app/scim/v2/Users` | create a user (for directories that don't batch: Okta, Entra ID) | ? |
| `GET /v1/app/scim/v2/Users` | list or filter users | ? |
| `GET /v1/app/scim/v2/Users/{id}` | one user | ? |
| `PUT /v1/app/scim/v2/Users/{id}` | replace a user | ? |
| `PATCH /v1/app/scim/v2/Users/{id}` | change a user | ? |
| `DELETE /v1/app/scim/v2/Users/{id}` | remove a user | ? |
| `GET /v1/app/scim/v2/ServiceProviderConfig` | what OpenRails accepts, including the batch limits | — |
| `GET /v1/app/scim/v2/ResourceTypes` | the resource types it serves | — |
| `GET /v1/app/scim/v2/Schemas` | the schemas of those resources | — |



**Webhooks** (always mounted; payment processors, verified by signature)

| Route | What it does |
|---|---|
| `POST /v1/webhooks/{rail}/{account_id}` | processor notifications (Stripe, NMI, CCBill), verified per account |

---

#### Sell Your Content

With `bill`, your OpenRails client, built and its routes mounted (above), next we'll start its background work (rebills, retries of failed payments, invoices) for the life of your server:

```go
if err := bill.Start(ctx); err != nil {
	return err
}
defer bill.Close(context.WithoutCancel(ctx))
```

Your server owns your content; OpenRails owns prices and who holds what. Your
API asks OpenRails two things: what each course costs, and what the user holds.
AuthKit's token rides `fetch`, not page loads or `<video src>`, so your pages
fetch this API with `authFetch` and play a short-lived signed URL. (In a real
app, content and previews come from ContentKit, whose paywall names the
entitlement to sell; your server finds the products with `ListProducts`, puts
them in its answer, and `<ProductList products>` sells them.)

**Your courses**, each with the keys that unlock it (its own, or the channel
membership; a members-only video only the membership), and the one question
every route asks OpenRails: which of those keys the user holds, in one read:

```go
// course is the host's own: what it shows, the catalog product that sells
// it and the entitlement that product grants, and its video under media/. A
// members-only video has no product: only the membership unlocks it.
type course struct {
	Slug, Title, Product, Entitlement, Video string
	MembersOnly                              bool
}

var courses = []course{
	{Slug: "css-101", Title: "Intro to CSS", Product: "course-101", Entitlement: "course:101", Video: "courses/css-101.mp4"},
	{Slug: "tailwind-102", Title: "Intro to Tailwind", Product: "course-102", Entitlement: "course:102", Video: "courses/tailwind-102.mp4"},
	{Slug: "live-qa", Title: "Live Q&A", Video: "courses/live-qa.mp4", MembersOnly: true},
}

// The channel membership unlocks every video, members-only ones included.
const membership, membershipProduct = "channel:membership", "channel-membership"

// unlock lists the entitlements that unlock c: any one will do.
func (c course) unlock() []string {
	if c.MembersOnly {
		return []string{membership}
	}
	return []string{c.Entitlement, membership}
}

// product is the catalog product the store sells c with.
func (c course) product() string {
	if c.MembersOnly {
		return membershipProduct
	}
	return c.Product
}

// held asks OpenRails which of entitlements the signed-in user holds, in one
// read. Each AuthKit user is their own customer. A signed-out visitor holds
// nothing and costs no call.
func held(c *gin.Context, bill *openrails.Client, entitlements ...string) (map[string]bool, error) {
	claims, signedIn := verify.ClaimsFromContext(c.Request.Context()) // set by authkitgin.Optional
	if !signedIn {
		return map[string]bool{}, nil
	}
	customer, err := billing.ParseCustomerID(claims.UserID)
	if err != nil {
		return nil, err
	}
	check, err := bill.CheckEntitlements(c, billing.CheckEntitlementsParams{CustomerID: customer, Entitlements: entitlements})
	if err != nil {
		return nil, err
	}
	return check.Entitlements, nil
}

// anyHeld reports whether owned holds any of keys.
func anyHeld(owned map[string]bool, keys []string) bool {
	return slices.ContainsFunc(keys, func(k string) bool { return owned[k] })
}
```

**List courses**, with prices and what the user owns. A page costs one product
read (`ListProducts`, none while the client holds those products in memory) and
one entitlement read, never one per course. The browser never reads the
catalog: the page carries each price as a buyer sees it (`billing.BuyerPrices`),
and billing-ui sells it:

```go
// GET /api/courses?cursor=&limit= pages over the host's courses. Each page
// costs one product read and one entitlement read, never one per course.
r.GET("/api/courses", authkitgin.Optional(ak), func(c *gin.Context) {
	from, err1 := strconv.Atoi(c.DefaultQuery("cursor", "0"))
	limit, err2 := strconv.Atoi(c.DefaultQuery("limit", "20"))
	if err1 != nil || err2 != nil || from < 0 || limit < 1 || limit > 50 {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}
	page := courses[min(from, len(courses)):min(from+limit, len(courses))]
	keys, entitlements := []string{}, []string{membership} // the membership unlocks every course
	for _, course := range page {
		keys = append(keys, course.product())
		if !course.MembersOnly {
			entitlements = append(entitlements, course.Entitlement)
		}
	}
	offers, err := bill.ListProducts(c, billing.ProductListParams{Keys: keys, Archived: new(false), ForSale: new(true), PageRequest: billing.PageRequest{Limit: len(keys)}})
	if err != nil {
		c.AbortWithStatus(http.StatusServiceUnavailable)
		return
	}
	owned, err := held(c, bill, entitlements...)
	if err != nil {
		c.AbortWithStatus(http.StatusServiceUnavailable)
		return
	}
	// Each product's prices as a buyer sees them, for the page's BuyButtons.
	prices := map[string][]billing.Price{} // by product key
	for _, product := range offers.Items {
		prices[product.Key] = billing.BuyerPrices(product.Prices)
	}
	data := []gin.H{}
	for _, course := range page {
		data = append(data, gin.H{"slug": course.Slug, "title": course.Title, "members_only": course.MembersOnly,
			"owned": anyHeld(owned, course.unlock()), "product_key": course.product(), "prices": prices[course.product()]})
	}
	var next any // null on the last page
	if from+limit < len(courses) {
		next = strconv.Itoa(from + limit)
	}
	c.JSON(http.StatusOK, gin.H{"data": data, "next_cursor": next})
})
```

**Gate a video**: whoever holds any key that unlocks it gets its signed URL;
anyone else, signed out included, gets `402` with the products on sale that
unlock it and where to buy one. The membership unlocks every video, members-only ones included; the
bundle grants both courses' keys; a rental passes for its 3 days.

```go
// A video: its signed URL to whoever holds any key that unlocks it, 402
// with the products on sale that unlock it and where to buy one to anyone
// else, signed out included.
r.GET("/api/courses/:course", authkitgin.Optional(ak), func(c *gin.Context) {
	i := slices.IndexFunc(courses, func(course course) bool { return course.Slug == c.Param("course") })
	if i < 0 {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	course := courses[i]
	owned, err := held(c, bill, course.unlock()...)
	if err != nil {
		c.AbortWithStatus(http.StatusServiceUnavailable)
		return
	}
	if anyHeld(owned, course.unlock()) {
		c.JSON(http.StatusOK, gin.H{"video_url": media.url(course.Video)})
		return
	}
	// Everything on sale granting a key that unlocks it: the course, the
	// bundle, the membership, with their prices as a buyer sees them. The
	// buy page sells them.
	offers, err := bill.ListProducts(c, billing.ProductListParams{Entitlements: course.unlock(), Archived: new(false), ForSale: new(true), PageRequest: billing.PageRequest{Limit: billing.MaxBatchItems}})
	if err != nil {
		c.AbortWithStatus(http.StatusServiceUnavailable)
		return
	}
	products := offers.Items
	for i := range products {
		products[i].Prices = billing.BuyerPrices(products[i].Prices)
	}
	slices.SortFunc(products, func(a, b billing.Product) int { return strings.Compare(a.Key, b.Key) })
	c.JSON(http.StatusPaymentRequired, gin.H{"error": "access_required", "products": products, "buy": "/courses/" + course.Slug + "/buy"})
})
```

**Serve media** to a signed, unexpired URL:

```go
// GET /media/*path serves a file under media/ to a signed, unexpired URL;
// ServeContent answers Range requests, so the player can seek.
r.GET("/media/*path", func(c *gin.Context) {
	path, expires := strings.TrimPrefix(c.Param("path"), "/"), c.Query("expires")
	at, err := strconv.ParseInt(expires, 10, 64)
	if err != nil || time.Now().Unix() > at || !hmac.Equal([]byte(c.Query("sig")), []byte(media.sign(path, expires))) {
		c.AbortWithStatus(http.StatusForbidden)
		return
	}
	f, err := os.Open(filepath.Join("media", filepath.FromSlash(path)))
	if err != nil {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	http.ServeContent(c.Writer, c.Request, stat.Name(), stat.ModTime(), f)
})

// mediaKey signs media URLs for 15 minutes, so a <video> plays them without
// a token. In production use an S3/R2 presigned GET, a CloudFront signed URL
// or ContentKit's media tokens.
type mediaKey []byte

func (k mediaKey) url(path string) string {
	expires := strconv.FormatInt(time.Now().Add(15*time.Minute).Unix(), 10)
	return "/media/" + path + "?" + url.Values{"expires": {expires}, "sig": {k.sign(path, expires)}}.Encode()
}

func (k mediaKey) sign(path, expires string) string {
	mac := hmac.New(sha256.New, k)
	mac.Write([]byte(path + "\n" + expires))
	return hex.EncodeToString(mac.Sum(nil))
}
```

**The pages.** Install the browser packages:

```sh
pnpm add @openrails/billing-ui @openrails/auth-ui
```

Wrap your app once in `<BillingProvider client={billing} appearance={{ theme: "auto" }}>`.
`BillingProvider` reads `GET /v1/config` and `GET /v1/me/config` once; a page
your server renders can carry both instead (`config`, from
`bill.PublicConfig(routes)` with the `Routes` you mounted, and `customerConfig`,
from `bill.CustomerConfig(routes, r)` with your own signed-in request), and
billing-ui then makes no request for them to load. This example also uses auth-ui's `<AuthProvider client={auth}>` and `<AuthUiProvider>`.
billing-ui doesn't depend on auth-ui: the billing client's only link to your auth is
the `fetch` you give it (here `auth.authFetch`, which attaches the AuthKit token; with
other auth, any `fetch` that sends your credentials), and `<BuyButton>`/`<ProductList>`
ask your app to sign someone in through `signedIn` and `onSignInRequired`. The store
lists the courses a page at a time: Watch for those the user owns, a
`<BuyButton price>` per price the server answered for the rest. A course page plays
the signed URL or goes to its buy page. A buy page sells the products the gate's
`402` carried (the course, the bundle, the membership) with `<ProductList products>`;
once paid, the gate lets the buyer in. The pages never handle a purchase themselves:
`<BuyButton>` and `<ProductList>` create the order, take the card (on the page, or in
the embedded payment page on a site the merchant registered to frame it) and report
`onPaid`:

```tsx
// pages.tsx
import { useEffect, useState } from "react"
import { Link, useNavigate, useParams } from "react-router-dom"
import { SignInDialog } from "@openrails/auth-ui"
import { createAuthClient } from "@openrails/auth-ui/client"
import { useAuth } from "@openrails/auth-ui/react"
import { BuyButton, createBillingClient, ProductList } from "@openrails/billing-ui"
import type { Price, Product } from "@openrails/billing-ui/client"

export const auth = createAuthClient() // AuthKit's browser client, at /api/v1: authFetch attaches the signed-in user's token
export const billing = createBillingClient({ baseUrl: "/billing/v1", fetch: auth.authFetch })

// A course as the app's API lists it: the host's own, priced by OpenRails
// (the server reads the catalog; the browser never does). A members-only one
// is sold with the membership's prices.
type Course = {
  slug: string
  title: string
  members_only: boolean
  owned: boolean
  product_key: string
  prices: Price[]
}

// StorePage lists the courses a page at a time: Watch for those the user
// owns, a BuyButton per price for the rest (the membership's, for a
// members-only video).
export function StorePage() {
  const navigate = useNavigate()
  const { signedIn } = useAuth()
  const [signingIn, setSigningIn] = useState(false)
  const [cursor, setCursor] = useState("0")
  const [pages, setPages] = useState<{ data: Course[]; next_cursor: string | null }[]>([])
  useEffect(() => {
    let current = true
    auth
      .authFetch(`/api/courses?cursor=${cursor}`)
      .then((res) => res.json())
      .then((page) => current && setPages((pages) => [...pages, page]))
    return () => {
      current = false
    }
  }, [cursor])
  const next = pages.at(-1)?.next_cursor
  return (
    <>
      {pages.flatMap((page) => page.data).map((course) => (
        <section key={course.slug}>
          <h2>{course.title}</h2>
          {course.members_only && <p>Members only</p>}
          {course.owned ? (
            <Link to={`/courses/${course.slug}`}>Watch</Link>
          ) : (
            course.prices.map((price) => (
              <BuyButton
                key={price.id}
                price={price}
                signedIn={signedIn}
                onSignInRequired={() => setSigningIn(true)}
                onPaid={() => navigate(`/courses/${course.slug}`)}
              />
            ))
          )}
        </section>
      ))}
      {next && <button onClick={() => setCursor(next)}>Load more</button>}
      <SignInDialog open={signingIn} onOpenChange={setSigningIn} />
    </>
  )
}

// CoursePage plays the course, if it's theirs.
export function CoursePage() {
  const { course = "" } = useParams()
  const navigate = useNavigate()
  const [videoURL, setVideoURL] = useState<string>()
  const [failed, setFailed] = useState(false)
  useEffect(() => {
    auth.authFetch(`/api/courses/${course}`).then(async (res) => {
      switch (res.status) {
        case 200: // theirs: the server answers a short-lived video URL
          setVideoURL((await res.json()).video_url)
          break
        case 402: // not theirs yet: the server says where to buy it
          navigate((await res.json()).buy, { replace: true })
          break
        default: // unknown course, or the server is unavailable
          setFailed(true)
      }
    })
  }, [course, navigate])
  if (failed) return <p>Couldn't load this course.</p>
  if (!videoURL) return <p>Loading…</p>
  return <video src={videoURL} controls />
}

// CourseBuyPage asks the server which products on sale unlock the course (the
// course, the bundle, the membership); the answer carries them, and the page
// offers them. Paid, the buyer goes back to the course, which now lets them in.
export function CourseBuyPage() {
  const { course = "" } = useParams()
  const navigate = useNavigate()
  const { signedIn } = useAuth()
  const [signingIn, setSigningIn] = useState(false)
  const [products, setProducts] = useState<Product[]>()
  const [failed, setFailed] = useState(false)
  useEffect(() => {
    auth.authFetch(`/api/courses/${course}`).then(async (res) => {
      switch (res.status) {
        case 200: // already theirs: back to it
          navigate(`/courses/${course}`, { replace: true })
          break
        case 402: // not theirs yet: the server answers the products that unlock it
          setProducts((await res.json()).products)
          break
        default: // unknown course, or the server is unavailable
          setFailed(true)
      }
    })
  }, [course, navigate])
  if (failed) return <p>Couldn't load this course.</p>
  if (!products) return <p>Loading…</p>
  return (
    <>
      <ProductList
        products={products}
        signedIn={signedIn}
        onSignInRequired={() => setSigningIn(true)}
        onPaid={() => navigate(`/courses/${course}`)}
      />
      <SignInDialog open={signingIn} onOpenChange={setSigningIn} />
    </>
  )
}
```

You can read the full example here [`examples/embedded`](examples/embedded)

---

#### Replacing Authkit

In the above example, OpenRails used Authkit for (1) saying who each request is, and (2) a lookup for user account info. Authkit and OpenRails were both built by us, and built to work together, but OpenRails only requires AuthKit’s standard-library-only `auth` and `userinfo` contracts. AuthKit’s runtime is optional; your own authentication system can implement those interfaces.

First, tell OpenRails who a request is. You write no middleware: OpenRails checks
each route's permission itself and answers refusals the same way for every
host. Admin and operator routes require no step-up; sensitive network
operations retain their recent-sign-in checks:

```go
// shopAuth says who a request is and what they hold; OpenRails decides what to admit.
type shopAuth struct{ sessions *Sessions }

var root = auth.Scope{Authority: "https://shop.example", ID: "root"} // your staff of every merchant

func (a shopAuth) Authenticate(r *http.Request) (auth.Verified, error) {
	s, err := a.sessions.Lookup(r) // the session the bearer token names, read live; OpenRails passes no cookies
	switch {
	case errors.Is(err, ErrNoSession):
		return nil, auth.ErrUnauthenticated
	case err != nil:
		return nil, errors.Join(auth.ErrUnavailable, err)
	}
	return session{s}, nil
}

func (shopAuth) RootScope(context.Context) (auth.Scope, error) { return root, nil }

func (shopAuth) KnownPermission(p string) bool {
	return slices.Contains(billing.Permissions(), billing.Permission(p))
}

type session struct{ s *Session }

func (v session) Identity() auth.Identity {
	return auth.Identity{
		Issuer: root.Authority, Subject: v.s.UserID, SubjectKind: auth.SubjectUser, // a UUID
		Invoker:    auth.Invoker{Issuer: root.Authority, ID: v.s.UserID}, // acting itself
		Credential: auth.Credential{Kind: auth.CredentialSession, ID: v.s.ID},
	}
}

// BoundScope: the merchant this session acts at, which your own records say
// (the store it signed in at, the merchant staff switched to). Zero for none.
func (v session) BoundScope() auth.Scope {
	if v.s.MerchantID == "" {
		return auth.Scope{}
	}
	return auth.Scope{Authority: root.Authority, ID: v.s.MerchantID}
}

// Can: exactly this permission in exactly this scope. OpenRails asks a route's
// merchant: name (merchant:customer:read) in {root.Authority, the merchant's id}
// and its root: name (root:customer:read) in root.
func (v session) Can(ctx context.Context, scope auth.Scope, permission string) (bool, error) {
	if scope.Authority != root.Authority || (scope != root && v.s.MerchantID != "" && scope.ID != v.s.MerchantID) {
		return false, nil // another authority's scope, or another merchant's while bound to one
	}
	return v.s.Holds(ctx, scope.ID, permission) // the user's roles there, read live
}

// CheckRecentSignIn: signed in within 15 minutes, for sensitive network operations.
func (v session) CheckRecentSignIn(context.Context) error {
	if time.Since(v.s.SignedInAt) > 15*time.Minute {
		return &auth.Challenge{Err: auth.ErrStepUpRequired, MaxAge: 15 * time.Minute}
	}
	return nil
}
```

The types are `github.com/open-rails/authkit/auth`. Mount with
`Routes{Auth: shopAuth{sessions}, ...}`, and check your
adapter in CI with `openrailstest.CheckAuth(t, routes, authtest.Cases{...})`
(`github.com/open-rails/authkit/auth/authtest`). For one merchant, set
`MerchantDeclaration.Issuer` to your issuer and return a zero `BoundScope`.
[`examples/own-auth`](examples/own-auth) demonstrates a host’s own users, sessions and roles.

Then you'd write your user-info lookup as well, which provides customer contact details and user-search on your OpenRails admin-dashboard. The interface is `github.com/open-rails/authkit/userinfo.Lookup`:

```go
type Lookup interface {
	// Get returns the users of ids, keyed by ID; an id you don't hold is absent.
	Get(ctx context.Context, ids []string) (map[string]userinfo.User, error)
	// Search returns users whose email, username or name contains query,
	// ignoring case, up to limit. query is literal text, never a pattern.
	Search(ctx context.Context, query string, limit int) ([]userinfo.User, error)
}

type User struct {
	ID       string // the Subject your auth's Identity reports
	Email    string
	Name     string
	Username string
}
```

And this is an example implementation:

```go
type appUsers struct{ db *pgxpool.Pool }

func (u appUsers) Get(ctx context.Context, ids []string) (map[string]userinfo.User, error) {
	rows, _ := u.db.Query(ctx, `SELECT id, email, name, username FROM users WHERE id = ANY($1)`, ids)
	users, err := pgx.CollectRows(rows, pgx.RowToStructByPos[userinfo.User])
	if err != nil {
		return nil, err
	}
	byID := make(map[string]userinfo.User, len(users))
	for _, user := range users {
		byID[user.ID] = user
	}
	return byID, nil
}

func (u appUsers) Search(ctx context.Context, query string, limit int) ([]userinfo.User, error) {
	if query == "" || limit < 1 {
		return nil, nil
	}
	rows, _ := u.db.Query(ctx, `SELECT id, email, name, username FROM users
		WHERE strpos(lower(email || ' ' || username || ' ' || name), lower($1)) > 0
		ORDER BY email LIMIT $2`, query, limit)
	return pgx.CollectRows(rows, pgx.RowToStructByPos[userinfo.User])
}
```

You'd pass this into your OpenRails client construct like: `openrails.Deps{Postgres: db, UserInfo: appUsers{db}}`.

---

#### Manage merchant config at runtime (Vault)

Merchant config (your name, settings, PSPs and their credentials) lives in one of two places:

- **Your file** (`merchant.yaml` above): read into memory at start and kept there. Changing merchant-config requires modifying your YAML file and reloading OpenRails with your new configuration. Theis configuration can be seein in the admin dashboard, but cannot be changed because it's immutable.

- **HashiCorp Vault**: staff add and rotate PSPs and change settings over HTTP / in the admin dashboard (`RouteGroups.MerchantConfig`), and OpenRails writes them to Vault: one document for the merchant and one per PSP. Configuration looks like this:

```go
cfg := openrails.Config{
	Database:          openrails.DatabaseConfig{Schema: "billing"},
	ProviderWriteMode: openrails.ProviderWritesFull,
	Merchant:          openrails.MerchantDeclaration{Slug: "onlydemo"}, // no PSPs here: staff manage them in Vault
	Vault: &openrails.VaultConfig{
		Address:    "https://vault.internal:8200",
		AuthMethod: "kubernetes", // or "approle", "token"
		K8sRole:    "onlydemo-billing",
		KVMount:    "onlydemo/kv", // the KV-v2 mount the merchant's documents are written under
	},
}
```

If your app already has an authenticated Vault client, pass it as `Deps.Vault` instead of `Config.Vault`.

---

#### Admin dashboard

The standalone server's console is at `/admin`, with Billing, Access and
Account tabs. Configure app sign-in with `admin_console.issuers`, or offer
local accounts with `local_sign_in`. Set `admin_console.disabled: true` for an
API-only server; see the [standalone console](docs/billing-admin.md).

The browser entry and JSON API have separate URLs. For a server hosted at
`https://billing.doujins.com`, open `https://billing.doujins.com/admin`.
`GET /v1/metrics/dashboard` returns the saved JSON layout and widget definitions;
`POST /v1/metrics/query` returns chart data. A host embedding `BillingAdmin`
chooses its own `basePath`.

An embedded engine serves the billing API. The host owns its admin pages
and authenticates their calls through `Routes.Auth`. `GET /v1/admin/access`
reports the caller's permitted areas and the assistants enabled there;
customer support is read-only without `root:customer:manage`.

#### Share one River fleet

`bill.Start(ctx)` runs OpenRails' own River workers in
`Config.Database.RiverSchema` (`billing_river` by default), whose tables `New`
creates. If you already run a River fleet, add OpenRails' jobs to it instead;
the fleet is yours, so you start and stop it. It must use
`Config.Database.RiverSchema`:

```go
fleet, err := riverhelpers.New(ctx, db, &river.Config{Schema: "billing_river"}, auth.RiverJobs(), bill.RiverJobs())
if err != nil {
	return err
}
if err := fleet.Start(ctx); err != nil {
	return err
}
defer fleet.StopAndCancel(context.WithoutCancel(ctx))
if err := bill.Start(ctx, openrails.WithRiverClient(fleet)); err != nil { // enqueues through your fleet; never starts or stops it
	return err
}
```

AuthKit joins the same fleet when its `Database.RiverSchema` is
`"billing_river"` and you start it with `auth.Start(ctx, authkit.WithRiverClient(fleet))`.

#### Running several instances

Run as many instances as you like against one PostgreSQL database and one
Redis: replicas of your app that each call `openrails.New` and `Start`, or
several `openrails run-server` processes. Nothing is configured per instance,
and no session is sticky.

- **Shared**: PostgreSQL (one writable primary) holds everything, River's queue
  and leader election included. Redis holds rate limits, admin lockouts,
  captcha challenges and spent DPoP proofs, once for the whole fleet. Without
  Redis these are kept in memory, so run a single instance. A configured Redis
  that stops answering falls back to memory per instance until it returns.
- **Identical on every instance**: the configuration (merchant declaration,
  catalog, rate limits, trusted proxies), the mounted `Routes`,
  `encryption.master_key`, and for the standalone server its signing and TOTP
  keys and `resource_server.dpop_nonce_key`. `auth.allow_ephemeral_signing_key`
  and `auth.allow_memory` are for a single instance.
- **Background work**: every instance that calls `Start` works River jobs, each
  job on one instance. River's leader schedules periodic work on its period's
  clock boundaries, once per period for the fleet; when the leader stops,
  another takes over.
- **Caches**: the catalog, settings and PSPs are read from PostgreSQL per
  request, so an edit on one instance shows on every other at once.
- **Safe to scale**: HTTP, webhooks (one effect per provider event), idempotent
  requests and workers. Applications consume notification facts idempotently and own their delivery.
- **Upgrades**: `New` migrates under an advisory lock (one instance migrates,
  the others wait). Migrations are additive, so old and new builds run side by
  side during a rolling deploy.

See [Running several instances](docs/operations.md#running-several-instances).

---

### Evolving the catalog

The following files form a catalog migration: update products in place, create
new price revisions, retire prices, and restore an earlier revision. All changes
are YAML applications. Apply each file with your app's catalog command shown
above (`client.ApplyCatalog`), or with `openrails catalog apply FILE.yaml --merchant onlydemo`.
There are no caller-managed application IDs or version numbers. These examples
build on the initial `catalog.yaml` above.

| YAML change | What happens |
|---|---|
| Edit fields under an existing product key | Update that product in place; its revision counter advances. |
| Change financial terms under an existing product/price key | Create a new immutable price revision and archive the previous live revision. |
| Add a different price key under a product | Create a separate price; the other prices stay unchanged. |
| Set `archived: true` on a price | Stop new sales at that price; the product and its other prices stay available. |
| Set `archived: true` on a product | Stop new sales of the product through all its prices. Their individual archive flags stay unchanged. |
| Omit a product, price, or field | Preserve its stored state. Omission never means deletion. |

**Change a product and raise its price.** Keep the keys to identify the same
product and price:

```yaml
# catalog-update.yaml
schema_version: 1
products:
  channel-membership:
    display_name: Channel Plus membership
    prices:
      monthly:
        amount: 12.99 USD
```

The `amount` field sets both the amount and currency. Other omitted fields
preserve their existing values. If no money fields are supplied, both the amount
and currency are preserved.

For the initial catalog, this creates `channel-membership.monthly.v1` at
$12.99 and archives `channel-membership.monthly.v0` at $10. The old price
record is retained unchanged. New subscribers buy v1; existing subscribers keep their exact accepted price and
entitlements indefinitely. Product descriptions and `entitlements` can also
be edited in place; changed entitlements apply to new purchases, not retroactively
to existing grants. Neither a YAML price change nor archival schedules a
subscription migration.

**Change what a product grants.** Add a named benefit for new memberships on
the membership product. The supplied list replaces the product's entire entitlement
list, so retain the original membership key:

```yaml
# add-membership-benefit.yaml
schema_version: 1
products:
  channel-membership:
    entitlements: ["channel:membership", "channel:downloads"]
```

Your application can check the new downloads key for its download feature. Prices
are omitted and stay unchanged. Existing subscribers keep their accepted benefit
snapshot; this file does not grant or remove benefits retroactively. The same
rule applies to editing a bundle's granted keys: earlier buyers retain the
keys they bought. Moving existing customers to new terms is a separate operation
from changing the prices available to new customers.

**Retire the monthly price.** Stop new 30-day memberships while keeping the
yearly price on sale:

```yaml
# retire-monthly.yaml
schema_version: 1
products:
  channel-membership:
    prices:
      monthly:
        archived: true
```

The omitted `yearly` price under `channel-membership` stays unchanged. Monthly subscribers
keep their accepted price and billing schedule. Archiving a price does not move
them to another product, cancel them, revoke access, or delete history.

**Replace a rental price.** Retire the three-day rental and add a seven-day
rental under the same course product:

```yaml
# replace-rental.yaml
schema_version: 1
products:
  course-101:
    prices:
      rent:
        archived: true
      rental-7-days:
        amount: 2.99 USD
        access_duration: 7 days
        billing_interval: null
```

The permanent `course-101.purchase` price and the bundle remain available.
Existing three-day rentals retain their original expiry; a new price never
extends them automatically. A price cannot move to another product.

**Retire an entire product.** There is no need to repeat its prices:

```yaml
# retire-bundle.yaml
schema_version: 1
products:
  course-bundle:
    archived: true
```

The bundle stops selling, while existing buyers keep access to both courses. The
individual course products and the membership product are omitted and stay
unchanged. Archiving one product does not retire other products granting the same
keys. To make the bundle available again, apply a new batch:

```yaml
# restore-bundle.yaml
schema_version: 1
products:
  course-bundle:
    archived: false
```

Restoring a product makes its unarchived prices available again. It does not
restore any prices that were individually archived.

**Restore an earlier price.** After both monthly revisions have been archived,
give the complete financial terms to select the intended historical revision:

```yaml
# restore-original-monthly.yaml
schema_version: 1
products:
  channel-membership:
    prices:
      monthly:
        amount: 10 USD # the original $10 terms
        access_duration: 30 days
        billing_interval: 30 days
        trial_unit_amount: null
        trial_duration_hours: null
        archived: false
```

This restores the original monthly price ID and v0; it does not rewrite history
or create another copy of the same terms. If a different monthly revision is
currently live, it is archived. The yearly price remains available
because it is omitted. Complete terms, including explicit null trial fields, avoid ambiguity
when a key has several archived revisions; a known immutable price `id` can also
select a particular revision.

### Backing up and restoring your catalog

Use a catalog snapshot when the original IDs and archived history must survive:

```sh
# Source database: capture every product and price revision, including archived rows.
openrails catalog export --merchant MERCHANT_UUID --out catalog.snapshot.yaml

# Destination database, selected by config: restore those exact records and IDs.
openrails catalog import --config destination.yaml --merchant MERCHANT_UUID --in catalog.snapshot.yaml
```

The snapshot preserves products, all price revisions, price-key history, provider
bindings, meters and rate cards, catalog application receipts, and catalog
revision state. A purchase's original product and price IDs continue to identify
the same records after restoration. It is a versioned YAML snapshot, distinct
from the editable `products:` application files above.

Restore into an empty catalog for the same merchant UUID. Provision referenced
PSP accounts and customers with their original identities first; the snapshot
contains references, not payment credentials. Import restores local state
atomically and makes no provider calls. It does not overwrite an existing
catalog or reprice subscriptions. Repeating the same successful restore is a
no-op, even after later catalog edits.

Payments, subscriptions, grants and balances are not included in a catalog
snapshot. The [merchant archive](internal/merchantarchive/README.md) covers
financial state and its referenced catalog; review its documented exclusions
before a whole billing migration.

For an editable declaration containing only current, unarchived prices, use the
lighter dump/apply workflow instead:

```sh
openrails dump-merchant-catalog --slug onlydemo > catalog.yaml
openrails catalog apply catalog.yaml --merchant onlydemo
```

That document can recreate equivalent prices, using the destination's product and
price identities. It does not preserve archived history or original IDs. Provider
references belong to their configured PSP accounts and must be reviewed before
copying between merchants. Reapplying an already applied document is a permanent
hash replay, so this workflow does not roll back later edits.

---

### Prepaid API-balance catalog

An API service can apply this separate catalog with the same catalog command. These products top
up a currency balance that metered API usage consumes; they are independent of
the creator-site catalog above.

```yaml
# api-catalog.yaml
schema_version: 1
products:
  api-credit-10:
    display_name: $10 prepaid API balance
    credit_grant:
      currency: USD
      amount: 10000000 # $10 of spendable API balance
      # expires_after_days defaults to 365, starting when payment succeeds
    prices:
      purchase:
        amount: 10 USD
        billing_interval: null

  api-credit-100:
    display_name: $100 prepaid API balance
    credit_grant:
      currency: USD
      amount: 100000000
      expires_after_days: 365
    prices:
      purchase:
        amount: 80 USD # grants 100 USD of balance: a bulk discount
        billing_interval: null

  api-deposit:
    display_name: Top up your prepaid API balance
    credit_grant:
      currency: USD
      from_payment: true # grant exactly the amount paid
      expires_after_days: 365
    prices:
      deposit:
        amount: 0 USD # customer_amount requires an explicit purchase amount; this is not free
        customer_amount:
          min_amount: 1000000 # at least $1
          max_amount: 1000000000 # at most $1,000
        billing_interval: null
```

The readable `amount` field above belongs to each price. Credit-grant amounts
and customer-selected minimums and maximums keep their numeric native units
(USD micros); they are separate policy fields.

Customers pay ahead of time; metered API usage draws down
the funded balance. Each top-up uses the existing currency balance and usage ledger.
The two packs above give different value per dollar. Each successful purchase
automatically and idempotently creates a credit lot; another purchase creates
another lot. Credit-only products do not create permanent ownership that would
prevent repeat purchases. The amount and expiry policy are frozen at purchase;
expiry starts when payment succeeds. Omitted expiry means 365 days. Later catalog
changes affect new purchases, preserving existing lots.

The deposit is also a product: it names what is bought, its currency, limits, and
expiry policy. The amount is selected for each purchase without creating a new
price revision. For a $100 deposit, the customer's browser orders it with that
amount on `POST /billing/v1/me/orders`:
`{"lines": [{"price_id": "<deposit price>", "amount": "100000000"}]}`, paying in
the same call or next. From your backend, the Go client creates the same order
for a customer, unpaid, for them to pay on your site:

```go
amount := int64(100_000_000) // $100 in USD micros
order, err := client.CreateOrder(ctx, billing.CreateOrderParams{
    CustomerID: customerID,
    Lines:      []billing.OrderLineParams{{PriceID: depositPriceID, Amount: &amount}},
})
```

The customer pays it with `POST /billing/v1/me/orders/{id}/pay`. Its amount cannot
change once ordered, and fixed-price packs reject an amount. Customer-selected deposits
currently support Stripe and NMI purchase; recurring credit benefits are not yet
supported. These are monetary API balances, consumed by priced usage, rather than
a separate request-count or token-count wallet. A voluntary refund requires the
corresponding credits to remain unused and unheld; forced reversals use the source
lot and preserve the existing rules for already-authorized usage.

#### Updating prepaid prices

Credit benefits evolve through the same partial updates. For example, this changes
only the expiry offered by new $100-pack purchases. Existing credit lots keep the
expiry promised when they were bought:

```yaml
schema_version: 1
products:
  api-credit-100:
    credit_grant: # replaces the complete credit policy; omitted credit_grant preserves it
      currency: USD
      amount: 100000000
      expires_after_days: 180
```

Archiving the pack or deposit stops new sales. It does not expire, revoke, or
otherwise change credits already granted.

---

### Running in Standalone mode

Standalone, OpenRails is its own service, the `openrails` server, and your app calls it over HTTP. The app code is the code of [Sell Your Content](#sell-your-content); this section shows only what changes. [`examples/standalone`](examples/standalone) is that course site run this way, with every file below.

#### Run the server

The server is an image, `docker.io/openrails/openrails:vX.Y.Z` (amd64 and arm64), and the `openrails` binary on each GitHub release (linux, darwin, windows). It needs PostgreSQL 18; an SMTP sender supports its native account mail and operator alerts. Applications own customer billing email. Redis (or Garnet) holds rate limits, lockouts, captcha challenges and spent DPoP proofs for every replica; without it they are kept in memory, so run one instance. A configured Redis that stops answering falls back to memory and degrades readiness.

The example's `compose.yaml` runs PostgreSQL, Redis, Mailpit, the server, a one-shot catalog job and the app:

```sh
cp openrails/merchant.example.yaml openrails/merchant.yaml   # your NMI sandbox gateway's IDs and keys
OPENRAILS_CLIENT_SECRET=$(openssl rand -hex 32) docker compose up -d --build
```

The server's service is the image with your two files mounted:

```yaml
openrails:
  image: docker.io/openrails/openrails:v0.235.0
  command: [run-server, --config, /etc/openrails/config.yaml, --merchant-manifest, /etc/openrails/merchant.yaml]
  volumes:
    - ./openrails:/etc/openrails:ro
    - openrails-keys:/vault/auth # the server's signing key (auth.keys_path)
```

Without Docker, download the binary and run the same files:

```sh
curl -fsSL https://github.com/open-rails/openrails/releases/download/v0.235.0/openrails_0.235.0_linux_amd64.tar.gz | tar xz openrails
DB_URL=postgres://… EMAIL_SMTP_HOST=… AUTH_KEYS_PATH=… ./openrails run-server --config config.yaml --merchant-manifest merchant.yaml
```

#### Configure it

`config.yaml` holds the process: its database, its own AuthKit, the issuer it trusts, its route groups and SMTP. Generate a stable `auth.default_tenant_id` once for the server's platform/operator account directory; it is independent of its merchants' tenants. Keep it across replicas and restarts. `openrails migrate up` prepares both billing and this explicit AuthKit directory. See [account directory adoption](docs/auth.md#standalone-account-directory). This is the example's, whole:

```yaml
provider_write_mode: full # full | limited | readonly: how much it may do at the PSPs

host: 0.0.0.0
port: 3053
public_billing_base_url: http://localhost:3053 # where browsers and PSPs reach this server

db:
  url: postgres://openrails:openrails@postgres:5432/openrails?sslmode=disable
redis: # rate limits and abuse state shared by replicas; without it, in memory (one instance)
  addr: redis:6379

# The server's own AuthKit: its tables (schema profiles) and signing key. Its
# sign-in stays off: people sign in at the app, the trusted issuer below.
auth:
  default_tenant_id: 51336692-ef57-4cdb-a189-a8d388465c11
  issuer: http://localhost:3053
  request_origin: http://localhost:3053 # the origin DPoP proofs name
  keys_path: /vault/auth
  direct_peer_ip: true                  # clients connect directly; behind a proxy, set trusted_proxies
  allow_loopback_http: true             # local only: http://localhost as issuer and origin
  allow_ephemeral_signing_key: true     # local only: generates its key under keys_path
  allow_missing_senders: true           # it sends no sign-in mail of its own

# OpenRails is an OAuth resource server: it accepts the app's AuthKit tokens.
resource_server:
  identifier: http://localhost:3053   # the tokens' aud
  dpop_nonce_key: local-dpop-nonce-key-at-least-32-bytes # the same on every replica
  trusted_issuers:
    - name: courses
      issuer: http://localhost:8080   # the app's AuthKit; keys from its /.well-known/jwks.json
      merchants: [onlydemo]           # its tokens act for this merchant only
      permissions: ["merchant:customer:read", "merchant:entitlements:read", "merchant:catalog:read"] # the most its tokens may grant

# Route groups beyond the public, customer (/v1/me) and webhook routes.
route_groups:
  admin: true        # customer support; the app's backend reads customers with merchant:customer:read
  programmatic: true # /v1/app: the backend's entitlement checks (merchant:entitlements:read) and offers (merchant:catalog:read)

# Native account mail and operator alerts; customer billing delivery belongs to the application.
email_smtp:
  host: mailpit
  port: 1025
  from: "OnlyDemo Billing <billing@onlydemo.example>"
```

Every key also reads from the environment, its path upper-cased and joined by `_` (`db.url` is `DB_URL`), or from a file of that name under `/vault/secrets` (`VAULT_SECRETS_PATH`); flags beat the environment, which beats the file. In production drop the `allow_*` lines; use https for `auth.issuer`, `auth.request_origin`, `resource_server.identifier` and the trusted issuer; mount a real signing key at `keys_path`; take `dpop_nonce_key` (`RESOURCE_SERVER_DPOP_NONCE_KEY`) and database credentials from secrets; and behind a load balancer replace `direct_peer_ip` with `trusted_proxies` (`cloudflare_proxies` behind Cloudflare). Redis takes `redis.url` (`redis://` or `rediss://`), `redis.username`, `redis.tls` and `redis.ca_cert` for a managed instance. The server applies its migrations at boot (`openrails migrate up` runs them ahead of a rollout); see [config.example.yaml](config.example.yaml) for every key.

#### Declare the merchant

The merchant file is the merchant's configuration: the server reads it at every boot (the first boot creates the merchant) and holds it in memory, read-only. This file holds the NMI keys, so keep it out of version control:

```yaml
version: 1
merchants:
  onlydemo: # your merchant's name: lowercase letters, digits and hyphens
    display_name: OnlyDemo
    api_host: localhost # the Host its public routes (the catalog, purchase) answer on
    profile:
      from_email: billing@onlydemo.example # merchant sender identity
    psps:
      mobius: # your name for this account; any key you like
        rail: nmi
        gateway_id: "000000" # the NMI dashboard's Gateway ID
        tokenization_key: your-public-tokenization-key # the browser's card fields use it
        secrets:
          security_key: your-private-security-key
          webhook_signing_secret: your-webhook-signing-key
```

The public routes find their merchant by the request's Host, so every merchant needs an `api_host`, a lone one included. In production keep `secrets` out of the file: render them (Vault Agent, a Kubernetes Secret) as a YAML file of the same shape and list it in `merchant_manifest_overlays`. To add and rotate PSPs at runtime instead, name a Vault KV mount (`vault.kv_mount`) and turn on `route_groups.merchant_config`: Vault then holds the configuration and the file names only the merchant, its slug and `api_host` ([Vault](docs/vault.md)).

#### Apply the catalog

Your `catalog.yaml` is the one from [How to Install](#how-to-install-embedded). Apply it once the server is up, and again whenever it changes; an unchanged document is a no-op. The command prints what changed and exits non-zero when it is refused or skips an object an edit changed:

```sh
openrails catalog apply catalog.yaml --config config.yaml --merchant-manifest merchant.yaml --merchant onlydemo
```

#### Trust your app's AuthKit

The server keeps no accounts: your AuthKit mints the tokens it accepts. Add the server as a resource and two OAuth clients, one for your users' browsers and one for your backend:

```go
AuthorizationServer: authkit.AuthorizationServerConfig{
	Resources: []authkit.ResourceServerConfig{{
		ID:            s.OpenRailsURL,                                   // the tokens' aud
		Scopes:        []string{"openrails:self", "openrails:merchant"}, // a customer's own billing; the merchant API
		Permissions:   []string{"merchant:customer:read", "merchant:entitlements:read", "merchant:catalog:read"}, // the most a token may carry
		ContactClaims: true,                                             // verified contact claims for a brand-new buyer
	}},
	Clients: []authkit.OAuthClientConfig{{
		ID:         "courses-web", // public: the browser proves a DPoP key instead of a secret
		Origins:    []string{s.PublicURL},
		Resources:  []string{s.OpenRailsURL},
		GrantTypes: []authkit.OAuthGrantType{authkit.GrantTokenExchange},
	}, {
		ID:           "courses-backend", // confidential: this server and the SCIM pushes
		SecretSHA256: hex.EncodeToString(secret[:]),
		Resources:    []string{s.OpenRailsURL},
		Permissions:  []string{"merchant:customer:read", "merchant:entitlements:read", "merchant:catalog:read"}, // customer support's reads; the content gate and its offers
		GrantTypes:   []authkit.OAuthGrantType{authkit.GrantClientCredentials},
	}},
},
```

The server's `resource_server` block (above) names this issuer, the merchant its tokens act for, and their permission ceiling.

#### Build the billing client

The same `*openrails.Client` as embedded, over HTTP, with your backend's client-credentials token:

```go
func newBilling(ctx context.Context, s settings) (*openrails.Client, error) {
	cc := backendCredentials(s) // courses-backend at your AuthKit's /oauth2/token, resource s.OpenRailsURL
	tokens := (&clientcredentials.Config{
		ClientID: cc.ClientID, ClientSecret: cc.ClientSecret, TokenURL: cc.TokenURL, Scopes: cc.Scopes,
		EndpointParams: url.Values{"resource": {cc.Resource}},
	}).TokenSource(context.WithoutCancel(ctx)) // caches each token until it expires
	// The token is bound to your merchant, so the client acts for that merchant only.
	return openrails.NewRemoteMerchant(s.OpenRailsURL, openrails.RemoteCredential{
		Token: func(context.Context) (string, error) {
			t, err := tokens.Token()
			if err != nil {
				return "", err
			}
			return t.AccessToken, nil
		},
	})
}
```

`ListProducts` and `CheckEntitlements` in your course routes are unchanged; they now go to the server.

#### Call it from the browser

The browser calls the server directly with a DPoP-bound `openrails:self` token that auth-ui gets by token exchange. Only the two clients change; the pages are the same:

```ts
// clients.ts
export const auth = createAuthClient({ resourceTokens: { clientId: "courses-web" } })

// The OpenRails server, which the app's server names (/api/billing): its
// API, and the resource identifier its tokens are minted for.
const server: { base_url: string; resource: string } = await fetch("/api/billing").then((res) => res.json())

export const billing = createBillingClient({
  baseUrl: server.base_url,
  fetch: (input, init) =>
    auth.getAccessToken()
      ? auth.resourceFetch(input, { ...init, resource: server.resource, scope: "openrails:self" })
      : fetch(input, init),
})
```

```go
r.GET("/api/billing", func(c *gin.Context) { // where the browser's billing client calls
	c.JSON(http.StatusOK, gin.H{"base_url": s.OpenRailsURL + "/v1", "resource": s.OpenRailsURL})
})
```

#### Customer contacts

The server learns who a customer is from your AuthKit, which pushes every user to its SCIM routes with your backend's client credentials:

```go
Provisioning: authkit.ProvisioningConfig{
	Interval: 30 * time.Second,
	Targets: []authkit.ProvisioningTarget{{
		Name:              "openrails",
		URL:               s.OpenRailsURL + "/v1/app/scim/v2",
		ClientCredentials: backendCredentials(s),
	}},
},
```

AuthKit shares only an email the user proved, so require it (`Registration.Verification: iam.RegistrationVerificationRequired`) and give AuthKit a sender (`authkit/adapters/smtp`) for the codes.

#### Receipts

OpenRails records customer billing facts; your application renders and sends receipts and payment notices. Consume them through `Client.NotificationEvents` or the application notification-consumer routes, persisting each event idempotently before acknowledging it. See [customer notification events](docs/customer-notifications.md).

#### PSP webhooks

Point each PSP's webhooks at the server: `https://<your server>/v1/webhooks/{rail}/{account_id}`, here `/v1/webhooks/nmi/000000`, signed with the PSP's `webhook_signing_secret`. Locally, reach it through a tunnel ([local webhooks](docs/dev/local-webhooks.md)).

#### Route groups and permissions

`route_groups` turns on what `RouteGroups` does embedded: `admin`, `catalog`, `merchant_config`, `metrics` and `programmatic` (`/v1/app`, SCIM included). The staff groups check the server's permissions, `merchant:customer:read|manage`, `merchant:catalog:manage`, `merchant:config:manage` and `merchant:metrics:read`; `/v1/app` routes check `merchant:orders:manage`, `merchant:entitlements:read`, `merchant:catalog:read`, `merchant:usage:manage` and `merchant:events:read`. A token carries them from your AuthKit roles within the issuer's ceiling. The backend client above may read customers, check entitlements and read offers, nothing more. Your staff sign in to the standalone console at `/admin`; configure `admin_console.issuers` for app sign-in or `local_sign_in` for local accounts. See the [standalone console](docs/billing-admin.md).

#### Operate it

- **TLS:** terminate it in front of the server; set `trusted_proxies` to the proxy's CIDRs so limits and logs see the client.
- **Health:** `/health/live` and `/health/ready`; `private_port` serves `/metrics` apart from the public port.
- **Scaling:** run replicas behind the load balancer, all sharing PostgreSQL. `run-server` also runs the background workers; to split them out, run `openrails run-worker` with the same files and `run-server --no-workers`. See [Running several instances](#running-several-instances).
- **Upgrades:** each release applies its migrations as it boots, and replicas take turns. From v1 the API and schema only grow ([compatibility](docs/compatibility.md)).
- **Backups:** PostgreSQL point-in-time recovery ([backup and recovery](docs/backup-and-recovery.md)); `openrails billing export` moves one merchant's billing to another deployment.

#### On Kubernetes

Install the Helm chart (chart `X.Y.Z` runs image `vX.Y.Z`):

```bash
helm install openrails oci://ghcr.io/open-rails/charts/openrails --version X.Y.Z \
  --namespace openrails --create-namespace -f values.yaml
```

Your `values.yaml` `config` becomes the server's `config.yaml`; secrets come only from Secrets you already have (or Vault); Postgres 18+ (for example CloudNativePG) and Redis are yours; more than one replica needs Redis. Ingress, a ServiceMonitor for the private `/metrics` listener and a PodDisruptionBudget are optional. See [docs/kubernetes.md](docs/kubernetes.md).

The [standalone guide](docs/standalone-integration.md) and [operations manual](docs/operations.md) go deeper.

### Using our Hosted OpenRails Platform

[openrails.dev](https://openrails.dev) runs OpenRails for you, for many
merchants:

- **Sign up** and create a merchant account. It gets its own API host, shown in
  the dashboard; your staff join it as owners, support or viewers.
- **In the dashboard,** add your PSPs (credentials go to Vault), manage the
  catalog, and copy the webhook URL for each processor.
- **Your backend** creates a service token in the dashboard and uses the same Go
  `Client`:

  ```go
  bill, err := openrails.NewRemoteMerchant("https://<your API host>", openrails.RemoteCredential{
      APIKey: os.Getenv("OPENRAILS_SERVICE_TOKEN"), // shown once in the dashboard
  })
  ```

- **Your users** sign in with your own auth: register your signing application
  in the dashboard, and the platform accepts its access tokens for your
  customers' `/v1/me` calls from billing-ui.

---

### Features

- **Orders** — one purchase model across every rail: tokenized card entry
  (NMI Collect.js, Stripe Elements), a provider-hosted redirect, and Solana Pay.
- **Catalog as source of truth** — declare your products, prices, rate cards, and tiers
  once; OpenRails pushes them to providers (find-or-create) and watches for drift.
- **Entitlements & product ownership** — your webserver asks one question ("does user X
  have Y right now?") against your own database, never a provider API.
- **Tiered plans with proration** — users upgrade/downgrade between tiers; preview the
  proration before committing.
- **Dunning: capture lost revenue** — failed rebills are retried on a schedule derived
  from the billing cycle, with a staleness window that guarantees a months-old failure is
  canceled, never surprise-charged.
- **Payment-lifecycle facts** — payment failures, card updates, renewals and
  cancellations produce durable events. Each application consumes them independently
  and owns its inbox, templates, email preferences and delivery.
- **Higher approval rates, measurably** — network tokens, automatic account-updater
  refresh of expired/reissued cards before rebills, and correct stored-credential
  (MIT/CIT) framing on every recurring charge. Then prove the uplift in your own
  dashboard: approval rate is sliceable by token type, rail, and decline reason.
- **Metered billing & spend control** — pre-authorize with holds (admit → capture/release),
  per-user budgets, spend caps, trust levels, prepaid credits or arrears invoicing.
- **Solana, for real** — one-off USDC payments *and* on-chain recurring subscriptions via
  the official delegation program, devnet-tested like any other sandbox.
- **Browser-direct self-service** — end-users hit `/v1/me/*` (balance, orders,
  subscriptions, payment methods, cancellation) straight from your frontend; no proxy
  routes to write or maintain.
- **Batch import of legacy data** — bring existing subscribers, payments, and vault
  references in from a previous billing system and let reconciliation converge them.
- **Operational notifications** — review reconciliation findings in the console,
  with immediate notifications and encrypted outbound webhook destinations.
- **Team & scoped API keys** — invite your staff with role-scoped permissions, or
  define least-privilege roles of your own (a storefront key that only checks
  entitlements); machine access through API keys that carry explicit grants and can
  never cross merchants.
- **Merchant console** — an optional admin UI for your team: customers, subscriptions,
  refunds, catalog, API keys, and a metrics dashboard you can customize with your own
  key-metric widgets.
- **Hosted & embedded payment page** — send a customer to a pay URL on OpenRails' own
  domain, or frame it on your site: cards, 3-D Secure, Apple Pay and Google Pay.
- **One account across merchants (openrails.dev)** — shoppers link their openrails.dev
  account to each merchant's customer, proven from both sides, and see, cancel and pay
  for everything in one place.

---

### Who Uses OpenRails

Archetypes — the site you're building, and what OpenRails does for it:

- **Building an OnlyFans, Fansly, or Pornhub Premium** — recurring memberships on
  processors that will actually board adult content (NMI ISOs like PayKings/PaymentCloud,
  CCBill). You get duplicate-subscription prevention, dunning that chases failed rebills,
  and entitlements that survive lost webhooks — the subscription-lifecycle plumbing every
  paysite rebuilds badly.
- **Building a Patreon or Substack** — creator/membership tiers with upgrades,
  downgrades, and proration. Your app asks "is this person a patron at tier 2 right now?"
  against its own database, not a provider API.
- **Building a Gumroad, itch.io, or Teachable** — videos, courses, games, downloads sold
  individually. Purchases can grant permanent ownership or time-limited rental access; your
  server checks the stored access grant. There is one purchase model whether the buyer pays by card or USDC.
- **Building a Linear, Notion, or Figma** — classic SaaS seat billing: define the tier
  list once, customers self-serve upgrades with proration previews, Stripe rail today
  with an exit ramp built in.
- **Building an OpenAI-platform or Replicate** — the credits console: users pre-pay a
  balance (or run arrears with monthly invoices), you draw it down per request. The
  admission API pre-authorizes with holds so a runaway job stops *before* the balance
  goes negative — a real ledger, not a usage counter bolted to Stripe.
- **Building a Midjourney or Cursor** — subscription + metered hybrid: a base plan plus
  GPU/LLM usage, per-user and even per-agent spend budgets with trust levels. Built for
  AI products where the marginal cost of a request is real money.
- **Building a Helius-style paid service for crypto users** — your customers live
  on-chain; take USDC straight to a wallet you control, including recurring on-chain
  subscriptions. No processor exists in the loop to drop you.
- **Running an Aylo-style network of sites** — several brands, one self-hosted billing
  deployment, each site an isolated merchant with its own rails on shared
  infrastructure, isolation enforced by the database.
- **Anyone who watched the 2021 OnlyFans near-ban** — if you're in a category processors
  purge (content, crypto, CBD, gaming), the question isn't *if* you get a termination
  email, it's *when*. Integrate rail-agnostic now and that email becomes a config change
  — swap to a high-risk gateway over a weekend, keep your subscribers, keep your
  entitlements, lose nothing.

---

### Failed Payments Are a Revenue Line

Industry numbers: 5–14% of subscription payments fail, involuntary churn is 20–40% of all
churn (ProfitWell), and good recovery tooling wins back 30–60% of failed payments. For a
site doing $100K/mo, that's roughly $2–5K/mo of revenue that leaks or doesn't, depending
entirely on plumbing. Merchants pay $79–500/mo for standalone dunning tools — or 10–25%
of recovered revenue to success-fee vendors — to capture it.

OpenRails ships the whole recovery stack built in:

- **Fewer failures in the first place** — network tokens, account-updater card refresh
  before rebills, and correct MIT/CIT stored-credential framing lift issuer approval
  rates on the charges that matter most: the renewals.
- **Smart retries on the failures that remain** — hard declines (stolen card,
  do-not-honor) go terminal immediately instead of burning retries; soft declines get a
  cycle-derived schedule, and a months-stale failure is canceled, never surprise-charged.
- **Events for the customer conversation** — payment-failure and card-expiry facts
  let your application deliver notices and recheck whether an action is still needed.
- **Proof it's working** — approval rate by rail, token type, and decline reason on your
  customizable dashboard.

---

### Technical Guarantees

The engineering underneath, and the invariants it enforces — this is what you'd otherwise
build yourself, badly, under deadline:

- **The Convergence Engine.** Billing systems rot by drifting from provider truth: a lost
  webhook here, a manual refund there. OpenRails treats the provider as the source of
  truth for money state and *continuously* re-converges against it — inline after every
  mutation, on a background sweep, and via watermarked provider reads. Every divergence
  becomes a classified finding on one of four planes (provider-observed truth, derived
  effects, lifecycle clocks, internal consistency); safe repairs apply automatically,
  judgment calls queue for a human and never auto-fire.
- **Rebilling, both ways.** New subscriptions are collected by OpenRails itself:
  stored-credential charges where *we* decide the amount and timing (scheduled reprices,
  vaulted-card rails), and the on-chain crank for Solana. Subscriptions a provider
  already schedules (an imported NMI, Stripe or CCBill book) stay with the provider,
  observed and converged. Same subscription model either way.
- **Double-entry money, twice over.** Every money movement is a balanced double-entry
  ledger transaction — value is never created or destroyed, only moved. A separate grant
  ledger tracks credit lots (who was granted what, from which source), and the
  Convergence Engine cross-checks the two continuously: duplicates, broken source
  references, and unbalanced effects surface as findings instead of festering.
- **Audited entitlements.** No entitlement exists without provenance: every access window
  records its source event, grant, and lifecycle (granted → revoked, with reason), so
  "why does this user have premium?" is a query, not an investigation. Excess grants
  (e.g. access surviving a refund) are detected and flagged automatically.
- **Effectively-once provider writes.** Every outbound mutation is a durable intent
  before it's an HTTP call: idempotency-keyed, origin-tagged, account-fingerprinted, and
  drained by an executor that resolves ambiguous outcomes by *reading* the provider
  before any retry. A charge is never blind-retried; a credential swap can never fire a
  stale queue against the wrong account.
- **Environments that can't lie.** Every PSP declares whether it moves real money
  (its rail's `environment`: `live | test | sandbox`, Solana's `mainnet | devnet | testnet`), and OpenRails proves it with the provider
  before the PSP is used: a Stripe key's prefix must agree, every NMI account is asked
  whether it is in test mode (a sandbox.nmi.com account is probed with a card only a
  simulator approves), and a Solana PSP pays on the network its environment names. A
  merchant mixes live and test PSPs freely.
- **Your users never pay for our malfunction.** Access is closed by *evidence* — a
  confirmed cancellation, a terminal decline, exhausted dunning — never by the clock
  alone. A lost webhook, a dead pipe, or stale data parks the subscription for
  verification with access intact. Terminal cancellation is the last resort, not the
  default.
- **Explicit merchant isolation.** The credential binds the merchant; scoped SQL
  predicates and composite relationships keep its records separate. Isolation
  works with the application's owning database connection. Platform discovery
  and cross-merchant operations have explicit entrypoints.
- **Time is injected, and the build enforces it.** Every billing decision runs on an
  injectable clock, guarded by a lint that fails the build on naked `time.Now()` in
  business logic. That's why a year of renewals, dunning, and expiry can be
  fast-forwarded in an integration test — the billing engine is deterministic under
  time travel.
- **One client, structurally incapable of drift.** Embedded and standalone modes share
  one client implementation over one handler surface — the in-process transport
  dispatches into the same routes HTTP does. Parity between modes isn't tested into
  existence; there's nothing to diverge.
- **Work scales with activity, not accounts.** No routine job ever sweeps
  everything-on-file: due work is indexed, provider reads are watermarked, and
  per-merchant jobs fan out only for merchants with something to do. Ten thousand idle
  merchants cost roughly nothing.

---

### Manifesto

OpenRails ultimate goal is to break the parasitic monopoly Visa + MasterCard currently hold upon American's financial lives. This pain is especially acute for 'high risk' businesses (crypto, porn, gambling, CBD, etc.) who are banned from most payment providers. OpenRails makes it much easier to integrate with 'high risk payment gateways', which have really shitty APIs usually, and lack all of the dev-ex nicities you get with Stripe.

---

### License

Our license is very permissive; you can do anything you like with OpenRails and its sourcecode, except create a multi-tenant hosted service that competes with our own hosted-SaaS platform. This is the only restriction we impose, since that is our primary business model.

---

### Required Services

Standalone Mode:
- Postgres 18+ (can be shared with your webserver)
- A redis-compatible service (we recommend Garnet) (Optional for rate-limiting)

Embedded Mode:
- Same as above, plus your webserver has to be written in Go, since this is a Go-library after all.

---

## Collecting Credit Card Info + PCI-Compliance

OpenRails does not receive or store your user's credit card details; only your payment-process should. There are two payment flows that achieve this:

- **Redirect flow**: Browser -> payment provider's payment page (ex. Stripe) -> user enters credit card details -> payment provider redirects back to your frontend. Behind the scenes Stripe webhook -> Your OpenRails server -> updates entitlements in your database.
- **Tokenized-vault flow**: Browser -> sends credit card details directly to your payment provider -> browser receives a token in response -> browser sends the token to OpenRails -> OpenRails sends the charge + token to the payment provider -> payment provider charges the card and returns the result to OpenRails -> OpenRails updates entitlements in your database.

Your webserver + OpenRails only need PCI-compliance SAQ-A, which is a self-assessment + annual questionnaire; if you're handling or storing credit-card information that would make you SAQ-D, which requires a lot of work.

---

### When NOT to Use OpenRails

We'd rather you find this out here than three weeks in. Skip OpenRails if:

- **You're a simple, low-risk SaaS that fully trusts Stripe.** A handful of subscription
  tiers, no metered credits, no risk of being classified "high-risk"? Direct Stripe
  (Checkout + Billing + a few webhook handlers) is genuinely less work than running us.
  OpenRails earns its keep when you need what Stripe doesn't give you: a local source of
  truth for access (entitlement checks against your own Postgres — no Stripe call in the
  hot path, keeps working through a Stripe outage), pre-authorization/spend-control for
  metered workloads, or the ability to switch rails without a rewrite when a provider
  drops you.
- **You need polished invoicing, tax, and revenue recognition today.** That's Stripe
  Billing / Lago / Kill Bill territory; our invoicing is functional, not an accounting
  suite.
- **You can't run Postgres.** OpenRails is self-hosted infrastructure with a database as
  its source of truth. If you want zero ops, wait for our hosted version.

---

### OpenRails vs Alternatives

Three open-source projects get compared to us most often. They're good products that sit
at different layers — here's an honest map:

| | **OpenRails** | **OpenMeter** | **Lago** | **Kill Bill** |
|---|---|---|---|---|
| Core job | Full billing engine: payments, subscriptions, entitlements, ledger | Usage **metering** + entitlement pipeline | Usage-based **rating + invoicing** | Subscription billing **framework** |
| Moves money itself | ✅ four rails, incl. high-risk + crypto | ❌ delegates to Stripe | ❌ delegates to PSP integrations | via payment plugins you run |
| High-risk gateways (NMI ISOs, CCBill) | ✅ first-class | ❌ | ❌ | write your own Java plugin |
| Crypto (Solana/USDC, incl. on-chain recurring) | ✅ | ❌ | ❌ | ❌ |
| Entitlements as the access source of truth | ✅ timeline your app reads | ✅-ish (feature access checks) | ❌ | subscription-state only |
| Pre-auth spend control (hold → capture, budgets, trust levels) | ✅ | ❌ meters after the fact | ❌ prepaid credits, no holds | ❌ |
| Invoicing / coupons / tax maturity | functional, not an accounting suite | ❌ (Stripe's problem) | ✅ its core strength | ✅ mature |
| Dunning / failed-payment recovery | ✅ built-in doctrine | ❌ | retry logic | ✅ overdue system |
| Embeds as a library in your app | ✅ Go, in-process | ❌ | ❌ | ❌ |
| Stack you operate | one Go binary + Postgres (+ optional Redis) | Go service + event pipeline | Rails app + Postgres + Redis + workers | JVM + MySQL + plugin runtime |

**vs OpenMeter.** OpenMeter is a usage-metering pipeline: it ingests high-volume event
streams, aggregates them into meters in real time, does feature/entitlement checks, and
hands the results to Stripe for actual billing. If your problem is *"billions of usage
events, Stripe is fine as the money layer,"* OpenMeter is the stronger metering engine —
its ingestion architecture is built for volumes we don't target. But it never touches
money: no payments, no dunning, no ledger, no rails. And metering is after-the-fact by
design — it can tell you what a customer used, not stop them before an expensive request
exceeds their balance. OpenRails' admission API (hold → capture/release, spend caps,
per-invoker budgets) exists precisely for that pre-authorization gap, and our
double-entry ledger — not a Stripe mirror — is the money record.

**vs Lago.** Lago is the open-source Stripe-Billing/Chargebee alternative: plans,
metering, rating, coupons, prepaid credits, and genuinely polished invoicing, with
charging delegated to PSP integrations (Stripe, Adyen, GoCardless, …). If your business
is invoice-centric B2B billing on mainstream processors, Lago is further along than we
are on the invoicing/accounting surface, and we say so above. What it doesn't give you:
rail independence (its PSP list won't board high-risk businesses), any crypto rail, an
entitlement timeline your app can gate features on, pre-auth spend control, or an
embedded mode — it's a Rails service you deploy and call. OpenRails is the payments +
access-control engine; Lago is the invoicing engine.

**vs Kill Bill.** Kill Bill is the closest functional overlap: a mature, battle-tested
subscription-billing framework with real dunning, catalog versioning, and a plugin
architecture that can, in principle, reach any gateway. Its cost is operational and
developmental weight: a JVM/MySQL stack, XML catalogs, and gateway support that means
*writing and maintaining a Java plugin* — for a high-risk ISO or a crypto rail, you're
building the integration yourself on their SPI. Kill Bill's entitlement API tracks
subscription state; it isn't a per-feature access timeline. OpenRails trades fifteen
years of framework generality for a sharper shape: one Go binary (or an import into your
own), rails that board high-risk merchants working out of the box, entitlements as the
thing your app reads, and a reconciliation engine that continuously converges your
database against provider truth.

---

### Integrate with an AI Agent

Using Claude Code, Codex, or another coding agent? Paste this prompt to have it do the
integration for you:

```text
Integrate OpenRails (github.com/open-rails/openrails) — a self-hostable
billing/payments engine — into this application.

First, fetch and read the agent integration guide; it has the decision tree,
the milestone plan, and links to every doc you need:
https://raw.githubusercontent.com/open-rails/openrails/master/docs/agent-integration.md

Before writing any code, confirm with me:
1. Mode — embedded (this app is Go; the engine runs in-process) or
   standalone (separate service; any language can call it).
2. Payment rails — NMI-backed gateway / Stripe / CCBill / Solana, and
   whether I already have sandbox + live credentials.
3. What we sell — subscriptions, one-time purchases, metered usage/credits.

Then follow the guide's milestone plan in order, verifying each step before
the next. All development runs on non-live PSPs (each PSP's `environment:
test` or `sandbox`); prove the full purchase → webhook → entitlement flow
end-to-end before ever touching live credentials.
```

The agent-facing guide itself lives at [docs/agent-integration.md](docs/agent-integration.md).

---

### Documentation

**Integrate** — for developers wiring OpenRails into an application:

- [Embedded integration (Go library)](docs/embedded-integration.md) — run the engine in-process: boot, migrations, declaring your merchant, mounting the billing routes on your server, calling the in-process client.
- [Standalone integration (service)](docs/standalone-integration.md) — deploy OpenRails as its own service: production config, first-run provisioning, API keys, the Go SDK and plain-HTTP integration.
- [Frontend integration](docs/frontend-integration.md) — the browser side: self-service routes, orders, payment methods, tokens, and error handling.
- [`@openrails/billing-ui`](sdk/billing-ui/README.md) — the embeddable purchase and account-billing React UI; each release attaches `openrails-billing-ui-X.Y.Z.tgz`.
- [The auth model](docs/auth.md) — one credential per trust domain: why embedded uses your session credential and standalone uses trusted issuers' access tokens.
- [Products and prices through the Client](docs/catalog-client.md).
- [Payment page](docs/payment-page.md) — a pay URL on OpenRails' own domain, or embedded on your site.
- [Customer network](docs/customer-network.md) — openrails.dev accounts linked to each merchant's customers, and the portal's API.
- [PSP import](docs/psp-import.md) — onboarding a merchant's subscribers straight from its PSPs: a read-only pull, rules and mappings that link them to customers, and landing.
- [Batch import / legacy migration](docs/batch-import.md) — moving an existing subscriber base onto OpenRails: the import surface, the phased playbook, and the limited-mode cutover.

**API** — the contract:

- [API guide](docs/api/endpoints.md) — conventions and behavior; [every route](docs/api/routes.md); [every error code](docs/api/error-codes.md); [`api/openapi.json`](api/openapi.json).
- [Purchase](docs/api/commerce.md), [errors](docs/api/errors.md) and [money, ids and times on the wire](docs/money-wire.md).
- [Compatibility](docs/compatibility.md) — what v1 freezes and what counts as additive.
- [Migrating to v1](docs/migrating-to-v1.md) — everything a v0 host changes.

**Payment rails** — per-rail setup: credentials, the manifest entry, webhooks, sandbox testing:

- [Rail matrix](docs/rails/certification-matrix.md) — what each rail does, and what evidence backs a verification claim
- [NMI](docs/rails/nmi.md) (PayKings, PaymentCloud, and other NMI-backed ISOs)
- [Stripe](docs/rails/stripe.md)
- [CCBill](docs/rails/ccbill.md)
- [Solana](docs/rails/solana.md) (USDC, self-custody, on-chain recurring subscriptions)
- [Payment-method custody](docs/payment-method-custody.md) — who *holds* a stored card vs who *charges* it
- [PSP environments](docs/psp-environments.md) — how each PSP declares whether it moves real money, and how OpenRails proves it

**Run your business** — for the merchant defining plans and managing customers:

- [Merchant guide](docs/merchant-guide.md) — authoring the catalog (products, prices, rate cards, entitlements), applying it, and day-to-day customer management.
- [Admin console](docs/billing-admin.md) — the merchant portal: turning it on/off, building the assets, and using it.
- [Entitlements](docs/entitlements_timeline.md) — the access-timeline model your app reads.
- Usage billing — [request admission](docs/admission-operations.md), [billing policies](docs/billing-policies.md), [arrears and delinquency](docs/arrears-delinquency.md), [usage orders](docs/usage-orders.md), [running charges](docs/running-charges.md), [balance notices](docs/balance-notices.md).

**Operate the deployment** — for whoever keeps it running:

- [Operator guide](docs/operator-guide.md) — infrastructure requirements (Postgres, Redis/Garnet, Vault), what runs by itself, and the drift toolbox.
- [Operations manual](docs/operations.md) — the deep reference: operating modes and safety levers, dunning, reconciliation, the provider intent ledger, data retention, cutovers.
- [Merchant provisioning](docs/merchant-provisioning.md) — manifests, credentials, secrets, and API keys; [merchant names](docs/merchant-name-authority.md); [settings](docs/api/merchant-settings.md) and [configuration applications](docs/merchant-configuration.md).
- [Self-hosting with host-owned credentials](docs/self-hosting-mode1.md), [runtime configuration](docs/runtime-configuration.md) and [Vault](docs/vault.md).
- [Backup and recovery](docs/backup-and-recovery.md), [moving a merchant](docs/merchant-portability.md) and [provider uncertainty](docs/provider-uncertainty.md).
- [Rate limiting](docs/rate-limiting.md) — the built-in limits on purchase, card and subscription writes, and captcha escalation.

**Reference**

- [Glossary](docs/glossary.md) — rails, PSPs, merchants, customers, and the rest of the vocabulary.
- [Metrics & query API](docs/metrics-for-llms.md) — analytics access for dashboards and LLM agents.
- [Contributing / hacking on OpenRails](docs/dev/README.md) — dev workflow, testing, local webhooks.
- [Injected-code scan](docs/injection-scan.md) — the supply-chain gate every clone runs; rules, exclusions, and what to do when it fires.


## License

OpenRails is under the Functional Source License, Version 1.1, ALv2 Future License ([LICENSE](LICENSE)). The browser SDK `@openrails/billing-ui` ([`sdk/billing-ui`](sdk/billing-ui)) is MIT ([sdk/billing-ui/LICENSE](sdk/billing-ui/LICENSE)), so you can ship it in your own frontend under any license.


For destructive-operation limits, incident review and explicit operator clearance,
see [destructive-action controls](docs/destructive-actions.md). The incident audit
is separate from the Redis/process request-abuse state.
