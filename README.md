# OpenRails Billing Engine

OpenRails is embedded into your Golang-webserver, or run as a stand-alone self-hosted application, and it owns your consumer's full billing state:

- The operator (you) creates a catalog of products + prices, and what they grant when owned (access to digital content usually).
- Create a checkout session for each purchase
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

You'll need a Go webserver, and Postgres (v18 or higher).

Here we build a members-only video site: users sign in with [AuthKit](https://github.com/open-rails/authkit), buy a monthly "premium" plan or an individual video with a card, and watch what they have access to.

First install:

```sh
go get github.com/open-rails/openrails
```

Next, declare your catalog as a YAML config file:

```yaml
# catalog.yaml
schema_version: 1 # the file format
products:
  - key: premium
    display_name: Premium
    entitlements_spec: {premium: null} # named feature; follows the subscription's access policy
    prices:
      - key: monthly
        currency: USD
        unit_amount: 9990000 # $9.99; amounts are currency micros (millionths of a dollar)
        access_duration_hours: 720 # a 30-day paid period; also the recurring billing interval
        auto_renew: true

  - key: video-101
    display_name: Video 101
    entitlements_spec: {} # access to this product is recorded without a separate feature name
    prices:
      - key: purchase
        currency: USD
        unit_amount: 4990000 # $4.99, paid once
        access_duration_hours: null # permanent ownership; no time-based expiry
        auto_renew: false
      - key: rental-3-days
        currency: USD
        unit_amount: 1990000 # $1.99, paid once
        access_duration_hours: 72 # access ends after three days
        auto_renew: false

  - key: api-credit-10
    display_name: $10 prepaid API balance
    credit_grant:
      currency: USD
      amount: 10000000 # $10 of spendable API balance
      # expires_after_days defaults to 365, starting when payment succeeds
    prices:
      - key: purchase
        currency: USD
        unit_amount: 10000000 # pay $10
        auto_renew: false

  - key: api-credit-100
    display_name: $100 prepaid API balance
    credit_grant:
      currency: USD
      amount: 100000000
      expires_after_days: 365
    prices:
      - key: purchase
        currency: USD
        unit_amount: 80000000 # pay $80 for $100 of balance: a bulk discount
        auto_renew: false

  - key: api-deposit
    display_name: Top up your prepaid API balance
    credit_grant:
      currency: USD
      from_payment: true # grant exactly the amount paid
      expires_after_days: 365
    prices:
      - key: deposit
        currency: USD
        unit_amount: 0 # customer_amount requires an explicit checkout amount; this is not free
        customer_amount:
          min_amount: 1000000 # at least $1
          max_amount: 1000000000 # at most $1,000
        auto_renew: false
```

Product keys are merchant-wide; price keys belong to their product. `premium.monthly`
therefore identifies this subscription offer. OpenRails assigns immutable price
revisions automatically (`premium.monthly.v0`, then `v1` when its terms change).
Existing subscribers keep their accepted price and benefits until explicitly
migrated. Omitted entries stay unchanged; use `archived: true` to retire an offer.

**Features, ownership, and rental access.** An entitlement is a named permission
that your application understands. Here, `premium` allows watching the whole
library. `null` follows the purchased access terms; it does not by itself mean
forever. The video needs no invented `video:101` feature: OpenRails records access
to product `video-101` directly. Its purchase price creates permanent ownership;
its rental price creates access with an expiry. `CheckProductAccess` checks either
kind; `ListProductAccess` exposes each grant's `EndsAt` (nil for permanent ownership).
Refunds and revocations can withdraw grants, including permanent ones.

**Paid time and dunning access are separate.** For the current recurring contract,
`access_duration_hours` is both the paid period and the billing interval. A monthly
and yearly price can belong to the same Premium product. For a one-off purchase,
it is the access duration; nil means no time-based expiry. Dunning can maintain
access after paid coverage ends: 30 paid days plus 15 days of grace is still a
30-day price, with a separate grace grant. The current dunning policy can keep
access while collection continues or suspend it; it does not silently label grace
as paid time. Independent recurring schedules, such as monthly installments buying
a year of access, are not expressed by this field.

**Prepaid API balance.** Customers pay ahead of time; metered API usage draws down
the funded balance. Each top-up uses the existing currency balance and usage ledger.
The two packs above give different value per dollar. Each successful purchase
automatically and idempotently creates a credit lot; another purchase creates
another lot. Credit-only products do not create permanent ownership that would
prevent repeat purchases. The amount and expiry policy are frozen at checkout;
expiry starts when payment succeeds. Omitted expiry means 365 days. Later catalog
changes affect new checkouts, preserving existing lots.

The deposit is also a product: it names what is bought, its currency, limits, and
expiry policy. The amount is selected for each checkout without creating a new
price revision. For a $100 deposit, your trusted server mints the session with:

```go
amount := int64(100_000_000) // $100 in USD micros
session, err := client.CreateCheckoutSession(ctx, billing.CreateCheckoutSessionParams{
    Customer: billing.CheckoutCustomerIdentity{ID: customerID},
    ProductKey: "api-deposit",
    PriceKey: "deposit",
    Amount: &amount,
    SuccessURL: "https://example.com/billing/success",
})
```

Hand the session to that customer's browser. Its amount cannot be changed after
minting. Fixed-price packs reject an amount override. Customer-selected deposits
currently support Stripe and NMI checkout; recurring credit benefits are not yet
supported. These are monetary API balances, consumed by priced usage, rather than
a separate request-count or token-count wallet. A voluntary refund requires the
corresponding credits to remain unused and unheld; forced reversals use the source
lot and preserve the existing rules for already-authorized usage.

### Evolving the catalog

Credit benefits evolve through the same partial updates. For example, this changes
only the expiry offered by new $100-pack checkouts. Existing credit lots keep the
expiry promised when they were bought:

```yaml
schema_version: 1
products:
  - key: api-credit-100
    credit_grant: # replaces the complete credit policy; omitted credit_grant preserves it
      currency: USD
      amount: 100000000
      expires_after_days: 180
```

Archiving the pack or deposit stops new sales. It does not expire, revoke, or
otherwise change credits already granted.


All the changes below are YAML applications. Apply each file through the same
`catalog.ParseApplicationYAML` → `client.ApplyCatalog` startup path shown below,
or through `openrails apply-catalog --merchant myvideos --file FILE.yaml`.
There are no caller-managed application IDs or version numbers. These examples
build on the initial `catalog.yaml` above.

| YAML change | What happens |
|---|---|
| Edit fields under an existing product key | Update that product in place; its revision counter advances. |
| Change financial terms under an existing product/price key | Create a new immutable price revision and archive the previous live revision. |
| Add a different price key under a product | Create a separate offer; the other prices stay unchanged. |
| Set `archived: true` on a price | Stop new sales at that price; the product and its other prices stay available. |
| Set `archived: true` on a product | Stop new sales of the product through all its prices. Their individual archive flags stay unchanged. |
| Omit a product, price, or field | Preserve its stored state. Omission never means deletion. |

**Change a product and raise its price.** Keep the keys to identify the same
product and offer:

```yaml
# catalog-update.yaml
schema_version: 1
products:
  - key: premium
    display_name: Premium Plus # mutate the existing product
    prices:
      - key: monthly
        unit_amount: 12990000 # $12.99; currency, duration and renewal terms are preserved
```

For the initial catalog, this creates `premium.monthly.v1` at $12.99 and archives
`premium.monthly.v0` at $9.99. The old price record is retained unchanged. New
subscribers buy v1; existing subscribers keep their exact accepted price and
entitlements indefinitely. Product descriptions and `entitlements_spec` can also
be edited in place; changed entitlements apply to new purchases, not retroactively
to existing grants. Neither a YAML price change nor archival schedules a
subscription migration.

**Retire one price and add another.** This stops new monthly signups and adds an
annual offer on the same product:

```yaml
# replace-monthly-with-yearly.yaml
schema_version: 1
products:
  - key: premium
    prices:
      - key: monthly
        archived: true
      - key: yearly
        currency: USD
        unit_amount: 99990000 # $99.99
        access_duration_hours: 8760 # 365 days
        auto_renew: true
```

`premium.yearly` is a new price key, starting at v0. It does not replace the
price ID held by monthly subscribers: their billing continues at their accepted
monthly terms. Archival prevents new sales; it does not cancel subscriptions,
revoke paid access, or delete history. A price cannot move to another product.

**Retire an entire product.** There is no need to repeat its prices:

```yaml
# retire-video.yaml
schema_version: 1
products:
  - key: video-101
    archived: true
```

The video stops selling, while existing buyers retain their permanent access.
`premium` is omitted and stays unchanged. To make the video available again,
apply a new batch:

```yaml
# restore-video.yaml
schema_version: 1
products:
  - key: video-101
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
  - key: premium
    prices:
      - key: monthly
        currency: USD
        unit_amount: 9990000 # the original $9.99 terms
        access_duration_hours: 720
        auto_renew: true
        trial_unit_amount: null
        trial_duration_hours: null
        archived: false
```

This restores the original monthly price ID and v0; it does not rewrite history
or create another copy of the same terms. If a different monthly revision is
currently live, it is archived. The yearly offer remains available because it is
omitted. Complete terms, including explicit null trial fields, avoid ambiguity
when a key has several archived revisions; a known immutable price `id` can also
select a particular revision.

**Replay is not rollback.** Each successful application's canonical content hash
is remembered per merchant. Reapplying any of these files changes nothing, even
after later edits. Reapplying the initial `catalog.yaml` therefore does not undo
the price increase or restore an archived video. The same rule applies to already
used archive and restore files. Comments, formatting, and product/price ordering
do not make a new application. A new document is a new atomic batch; different,
previously unseen documents have no ordering guarantee, so apply intended changes
in order.

All these examples preserve omitted entries. `prune: true` is an explicit
bulk-archive option for omitted products and prices; it never deletes them.
Changing existing subscribers' accepted prices requires an explicit scheduled
reprice operation. Changing their accepted benefits uses the
[planned agreement-change workflow](https://github.com/open-rails/tracker/blob/master/openrails/1132.md).
Those subscriber migrations are separate from the YAML offer changes above.

### Turning catalog HTTP writes on and off

Set these options **before constructing the client**:

```go
cfg.HTTP = &openrails.HTTPConfig{Merchant: true} // authenticated merchant API
cfg.AllowCatalogUpdates = false                // omit its catalog-write routes
// Set AllowCatalogUpdates to true and restart to enable catalog HTTP editing.
```

| `AllowCatalogUpdates` | Catalog writes over HTTP | In-process `client.ApplyCatalog`, `CreateProduct`, `CreatePrice`, etc. |
|---|---|---|
| `false` | Unavailable | Available |
| `true` | Available to authorized callers | Available |

Catalog reads remain available on the merchant API. Turning HTTP writes off does
not make the database read-only or prevent later client edits. The startup example
below turns them off and applies the YAML through the client on every boot.

Now let's build the billing client:

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
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/open-rails/openrails"
	openrailsgin "github.com/open-rails/openrails/adapters/gin"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/riverqueue/river"
)

func newBilling(ctx context.Context, db *pgxpool.Pool, auth *authkit.Client) (*openrails.Client, error) {
	// Your payment processor account (a PSP). This one is an NMI gateway named "mobius", read
	// from MOBIUS_RAIL=nmi, MOBIUS_ACCOUNT_ID and MOBIUS_SECURITY_KEY. Declare as many as you like.
	mobius, err := openrails.PSPFromEnv("mobius", os.LookupEnv)
	if err != nil {
		return nil, err
	}

	// What you sell. Each distinct catalog batch is applied once, even across restarts.
	// Later programmatic edits remain available and are not undone by a replay.
	raw, err := os.ReadFile("catalog.yaml")
	if err != nil {
		return nil, err
	}
	declared, err := catalog.ParseApplicationYAML(raw)
	if err != nil {
		return nil, err
	}

	cfg := openrails.Config{
		Schema:            "billing",                    // the Postgres schema OpenRails' tables go in
		TestMode:          openrails.Sandbox,            // Sandbox or Live: which PSP credentials are accepted
		ProviderWriteMode: openrails.ProviderWritesFull, // ProviderWritesReadOnly never charges anyone
		Merchant: openrails.MerchantDeclaration{
			Slug: "myvideos", // you, the seller
			PSPs: map[string]openrails.PSPConfig{"mobius": mobius},
		},
		AllowCatalogUpdates: false, // hide catalog-write HTTP routes; the Go client can still edit
		HTTP: &openrails.HTTPConfig{
			Merchant: true,                        // authenticated merchant routes; catalog HTTP writes stay disabled
			Checkout: &openrails.CheckoutConfig{}, // products, prices, checkout sessions and processor webhooks
			CustomerRoutes: []openrails.CustomerRoutesConfig{
				{Scope: openrails.CustomerSelfService}, // /v1/me/*: users manage their own subscriptions and cards
			},
		},
		River: openrails.RiverHostOwned, // renewals, dunning and invoices run on your River workers
	}

	// 1. Create or upgrade OpenRails' tables. Safe to run on every boot.
	if err := openrails.Migrate(ctx, db, cfg); err != nil {
		return nil, err
	}

	// 2. Build the billing engine. OpenRails has no logins of its own: it asks your AuthKit
	// who is calling, and each user is their own paying customer.
	client, err := openrails.New(ctx, cfg, openrails.Deps{
		Postgres: db,   // required: the same pool your app uses
		AuthKit:  auth, // who is calling, what staff may do, and how recently they signed in
	})
	if err != nil {
		return nil, err
	}
	if _, err := client.ApplyCatalog(ctx, declared); err != nil {
		_ = client.Close(context.WithoutCancel(ctx))
		return nil, err
	}
	return client, nil
}
```

Now wire everything together and mount the routes:

```go
func main() { log.Fatal(run(context.Background())) }

func run(ctx context.Context) error {
	db, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		return err
	}
	defer db.Close()

	auth, err := newAuth(ctx, db) // see AuthKit's README; set River: authkit.RiverConfig{HostOwned: true}
	if err != nil {
		return err
	}
	defer auth.Close()
	bill, err := newBilling(ctx, db, auth)
	if err != nil {
		return err
	}
	defer bill.Close(ctx)

	// One River worker fleet runs your jobs, AuthKit's and OpenRails' (rebills, retries, invoices).
	// The fleet is yours, so you create River's tables ("" is River's default schema, public).
	if err := riverhelpers.ApplyMigrations(ctx, db, ""); err != nil {
		return err
	}
	workers, err := riverhelpers.New(ctx, db, &river.Config{}, auth.RiverJobs(), bill.RiverJobs())
	if err != nil {
		return err
	}
	if err := workers.Start(ctx); err != nil {
		return err
	}
	defer workers.StopAndCancel(context.WithoutCancel(ctx))
	if err := auth.Start(ctx); err != nil {
		return err
	}
	if err := bill.Start(ctx); err != nil {
		return err
	}

	r := gin.Default()
	if err := authkitgin.Mount(r, auth); err != nil { // sign-up and sign-in under /api/v1
		return err
	}
	if err := openrailsgin.Mount(r.Group("/billing"), bill); err != nil { // billing under /billing/v1
		return err
	}

	// Premium members can watch any video; a one-off buyer can watch the video they bought.
	r.GET("/videos/:id", authkitgin.Required(auth), func(c *gin.Context) {
		claims, _ := auth.VerifyRequest(c.Request)
		customer, err := billing.ParseCustomerID(claims.UserID)
		if err != nil {
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}
		premium, err := bill.HasEntitlement(c, customer, "premium", time.Now())
		if err != nil {
			c.AbortWithStatus(http.StatusServiceUnavailable)
			return
		}
		if !premium {
			productKey := "video-" + c.Param("id")
			access, err := bill.CheckProductAccess(c, customer, billing.CheckProductAccessParams{ProductKeys: []string{productKey}})
			if err != nil {
				c.AbortWithStatus(http.StatusServiceUnavailable)
				return
			}
			if !access[productKey] {
				c.JSON(http.StatusPaymentRequired, gin.H{"error": "purchase_required"})
				return
			}
		}
		c.File("videos/" + c.Param("id") + ".mp4")
	})

	return r.Run(":8080")
}
```

That's the whole integration, and it is the program in [`examples/embedded`](examples/embedded). Your server never touches a card number: the browser hands the card to the processor's tokenization iframe, and OpenRails charges the stored token, rebills it every 30 days, retries failed renewals, and keeps `HasEntitlement` up to date.

Mounting gives your users these routes under `/billing`:

**Shopping and checkout** (`HTTP.Checkout`)

| Route | What it does |
|---|---|
| `GET /billing/v1/products` | products on sale, each with its current prices (`?limit=`, `?cursor=`) |
| `GET /billing/v1/prices` | prices on sale (`?product_id=`, `?currency=`, `?auto_renew=`, `?limit=`, `?cursor=`) |
| `GET /billing/v1/currencies` | each currency's decimal places, for formatting amounts |
| `GET /billing/v1/checkout-config` | the payment methods a buyer can use, with their browser config |
| `GET /billing/v1/checkout-sessions/{id}` | read a checkout; the session id is the credential, so a payment page on another host can use it |
| `POST /billing/v1/checkout-sessions/{id}/pay` | pay it |
| `GET`, `POST /billing/v1/checkout-attempts/{id}/solana-pay` | the Solana Pay request a wallet signs (when a Solana PSP is declared) |
| `GET /billing/v1/solana/tokens` | supported Solana tokens with live prices (when a Solana PSP is declared) |
| `GET /billing/v1/captcha/status`, `GET /billing/v1/captcha/client.js` | the captcha a card-testing wave is asked to solve |
| `GET /billing/v1/capabilities` | which route groups and features are mounted, for your UI (always mounted) |

**Your customers' own billing** (`HTTP.CustomerRoutes`, signed in, always as the caller)

| Route | What it does |
|---|---|
| `POST /billing/v1/me/checkout-sessions` | start a checkout for a price |
| `GET /billing/v1/me/entitlements` | everything they currently have access to |
| `GET /billing/v1/me/subscriptions` | their subscriptions |
| `GET /billing/v1/me/subscriptions/{id}` | one subscription |
| `POST /billing/v1/me/subscriptions/{id}/cancel` | cancel at period end (with a reason) |
| `POST /billing/v1/me/subscriptions/{id}/resume` | undo a cancellation before the period ends |
| `POST /billing/v1/me/subscriptions/{id}/change-tier` | upgrade or downgrade |
| `POST /billing/v1/me/subscriptions/{id}/change-tier/preview` | what that change would cost |
| `PUT /billing/v1/me/subscriptions/{id}/payment-method` | move a subscription to another saved card |
| `POST /billing/v1/me/subscriptions/{id}/retry-now` | retry a failed renewal now |
| `GET /billing/v1/me/payment-methods` | their saved cards, newest first |
| `POST /billing/v1/me/payment-methods` | save a card (a processor token, never the card number) |
| `PUT`, `DELETE /billing/v1/me/payment-methods/{id}` | replace or remove a card |
| `POST /billing/v1/me/payment-method-setups` | start saving a card through Stripe |
| `GET /billing/v1/me/payment-method-setups/{id}` | read that setup |
| `POST /billing/v1/me/payment-method-setups/{id}/confirm` | finish it |
| `PUT /billing/v1/me/collection-payment-method` | choose the card that pays one currency's invoices |
| `GET /billing/v1/me/payment-operations/{id}/authentication` | a payment's 3-D Secure challenge |
| `POST /billing/v1/me/payment-operations/{id}/authentication/confirm` | finish it |
| `POST /billing/v1/me/billing-portal` | open Stripe's billing portal (when a Stripe PSP is declared) |
| `GET /billing/v1/me/payments` | payment and refund history |
| `GET /billing/v1/me/invoices` | invoices |
| `GET /billing/v1/me/invoices/{id}` | one invoice |
| `POST /billing/v1/me/invoices/{id}/pay-now` | pay an open invoice with a saved card |
| `GET /billing/v1/me/balance` | prepaid balance |
| `GET /billing/v1/me/transactions` | its ledger |
| `GET /billing/v1/me/usage` | metered usage |
| `GET /billing/v1/me/spend-limits` | spending limits |
| `GET /billing/v1/me/notifications` | billing notices ("your card was declined") |
| `GET /billing/v1/me/notifications/unread-count` | how many are unread |
| `POST /billing/v1/me/notifications/{id}/read` | mark one read |

**Payment processors** (always mounted)

| Route | What it does |
|---|---|
| `POST /billing/v1/webhooks/{rail}/{account_id}` | processor notifications (Stripe, NMI, CCBill), verified per account |

`HTTP.Merchant: true` publishes the merchant API (`/billing/v1/merchant/*`) for your staff and machines rather than your users: customers, refunds, catalog, settings, PSPs, alerts. Each route is gated by its merchant permission, and each has one method on the Go `Client`. Every route is in the [route table](docs/api/routes.md); the conventions are in the [API guide](docs/api/endpoints.md).

What you will set next:

| To | Set |
|---|---|
| Send billing email (receipts, failed-payment notices) | `Config.SendGrid` (`APIKey`, `From`), or your own `Deps.EmailSender`; without one OpenRails sends no email |
| Publish the merchant API | `Config.HTTP.Merchant` |
| Serve the admin console | `Config.AdminConsole` ([admin console](docs/admin-console.md)) |
| Share one billing schema between two apps | `Config.SchemaOwner`: `Migrate` hands the schema to that role |
| Change or switch off the built-in limits on checkout and card writes | `Config.RateLimits`, `Config.RateLimitsDisabled` ([rate limiting](docs/rate-limiting.md)) |
| Report readiness | `client.Ready(ctx)` and `client.Probes()` in your own health handler |

#### The frontend

[`@openrails/billing-ui`](sdk/billing-ui) is a React package with a styled checkout and an account page; every component calls your mounted `/billing/v1` routes directly, so you write no billing endpoints of your own. Install it from the release:

```sh
pnpm add https://github.com/open-rails/openrails/releases/download/vX.Y.Z/openrails-billing-ui-X.Y.Z.tgz
```

Give it a client that sends the user's AuthKit token, then use the components:

```tsx
import { useState } from "react"
import { createBillingClient } from "@openrails/billing-ui/client"
import { BillingProvider } from "@openrails/billing-ui/react"
import { AccountBilling, BillingUiProvider, CheckoutModal } from "@openrails/billing-ui"
import "@openrails/billing-ui/styles.css"
import { auth } from "./auth" // your auth-ui client: authFetch attaches the AuthKit token

const billing = createBillingClient({ baseUrl: "/billing/v1", fetch: auth.authFetch })

export function App() {
  return (
    <BillingUiProvider appearance={{ theme: "auto" }}>
      <BillingProvider client={billing}>
        <UpgradeButton />
        {/* Subscriptions (cancel, resume, change card), saved cards and payment history. */}
        <AccountBilling plansHref="/plans" collectionCurrency="USD" />
      </BillingProvider>
    </BillingUiProvider>
  )
}

function UpgradeButton() {
  const [sessionId, setSessionId] = useState<string>()
  async function upgrade() {
    // POST /billing/v1/me/checkout-sessions: OpenRails prices it from the catalog, for the signed-in user.
    const session = await billing.createCheckoutSession({ productKey: "premium", priceKey: "monthly" })
    setSessionId(session.id)
  }
  return (
    <>
      <button onClick={upgrade}>Go premium, $9.99/month</button>
      {sessionId && (
        <CheckoutModal
          open
          onOpenChange={(open) => !open && setSessionId(undefined)}
          // Card entry happens in the processor's iframe; paying saves the card and starts the subscription.
          source={billing.checkoutSource(sessionId)}
          onComplete={() => location.assign("/videos")}
        />
      )}
    </>
  )
}
```

Once the checkout succeeds, `HasEntitlement(user, "premium")` is true on your server and `/videos/:id` starts streaming. When the card is declined at renewal, OpenRails retries on a schedule, emails the user (with an email sender configured), and `AccountBilling` shows them a "fix your card" prompt; the entitlement stays until the subscription actually ends.

Several sites selling for one merchant can share one payment page instead: set `HTTP.Checkout.PageURL` (where the page is served) and `EmbedOrigins` (the sites allowed to frame it). The payment host serves billing-ui's `<CheckoutPage>`; each site creates its session as above and shows `<CheckoutFrame url={session.url} />`. See [billing-ui's README](sdk/billing-ui/README.md).

---

### How It Works

Operator side (you, the merchant):
- Declare your merchant and payment-processor accounts (in `Config`, a YAML manifest, or the API).
- Declare your catalog: products, the entitlements they grant, and prices. OpenRails charges its own price terms; a processor-side copy is linked only where a rail needs one.
- Call one API for admissions, credits, entitlement checks, subscriptions and invoices: the Go `Client` (in-process or over HTTP), or plain HTTP from any stack.

Customer side (your users):
- Apps with channel/content purchase rules route checkout through their own server: verify those rules, then call `Client.CreateCheckoutSession` and hand the session to the buyer's browser. Catalog-only apps can expose the built-in customer mint route.
- Your frontend calls `/v1/me/*` self-service routes with a short-lived token.
- Processor webhooks land on OpenRails; it updates entitlements in your database and your app reads them.

---

### Checkout catalog references

Use a stable product/price key pair such as `post-123.purchase`. `ApplyCatalog` changes its terms by
creating an immutable price version and retiring its predecessor. Checkout owns
current availability, amount, permanent ownership eligibility and payment
idempotency; a host wrapper supplies verified identity and its content policy.

| Operation | Reference contract |
| --- | --- |
| `CreateCheckoutSession` | Either `PriceID` or the pair `ProductKey` + `PriceKey` |
| `CreateCheckoutAttempt` | Either `PriceID` or the pair `ProductKey` + `PriceKey`; optional `Entitlement` and `OfferKind` admission assertions; the same `IdempotencyKey` and request replays the accepted attempt |
| `ListOffers` | Up to 100 exact resource keys in one request; explicit kind, currency preference, per-key limit and cursors |
| `HasEntitlement` / `ListEntitlements` | Exact grant-backed access; `ListEntitlements` reads up to 500 customers at once |
| `CheckProductAccess` | Product IDs or keys; archived purchase access remains readable |
| `CreatePrice` | Exactly one existing `ProductID`, `ProductKey`, or inline `ProductData` |
| `GetCheckoutConfig` | `GetCheckoutConfigParams`: `PriceID` or `ProductKey` + `PriceKey` lists the options that can sell it |
| `PreviewPSPRouting` | Exactly one `price_id` or `price_key` |
| Catalog reads | `GetProduct` / `GetPrice` (ID) or `GetProductByKey` / `GetPriceByKey` |
| Tier changes, accepted attempts, payments, subscriptions and imports | Immutable IDs |

Keys are opaque, including UUID-shaped keys: a field that takes an ID never
accepts a key, and there is no auto-detection. An accepted attempt keeps its
original price and benefit snapshot after a key moves or an offer is archived.
More in [products and prices through the Client](docs/catalog-client.md).

### Features

- **Checkout sessions** — one session model across every rail: tokenized card entry
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
- **Payment-lifecycle emails, handled** — "your payment failed", "update your card",
  renewal and cancellation notices: templated, deduplicated (a resolved failure
  supersedes the stale notice instead of double-sending), and branded per merchant. You
  don't build the awkward-conversation system; we are the awkward-conversation system.
- **Higher approval rates, measurably** — network tokens, automatic account-updater
  refresh of expired/reissued cards before rebills, and correct stored-credential
  (MIT/CIT) framing on every recurring charge. Then prove the uplift in your own
  dashboard: approval rate is sliceable by token type, rail, and decline reason.
- **Metered billing & spend control** — pre-authorize with holds (admit → capture/release),
  per-user budgets, spend caps, trust levels, prepaid credits or arrears invoicing.
- **Solana, for real** — one-off USDC payments *and* on-chain recurring subscriptions via
  the official delegation program, devnet-tested like any other sandbox.
- **Browser-direct self-service** — end-users hit `/v1/me/*` (balance, invoices,
  subscriptions, payment methods, cancellation) straight from your frontend; no proxy
  routes to write or maintain.
- **Batch import of legacy data** — bring existing subscribers, payments, and vault
  references in from a previous billing system and let reconciliation converge them.
- **Operational notifications** — review reconciliation findings in the console,
  with immediate notifications and encrypted outbound webhook destinations.
- **Team & scoped API keys** — invite your staff with role-scoped permissions;
  machine access through API keys that carry explicit grants and can never cross
  merchants.
- **Merchant console** — an optional admin UI for your team: customers, subscriptions,
  refunds, catalog, API keys, and a metrics dashboard you can customize with your own
  key-metric widgets (plus an optional LLM analytics copilot).

---

### Who Uses OpenRails

Archetypes — the site you're building, and what OpenRails does for it:

- **Building an OnlyFans, Fansly, or Pornhub Premium** — recurring memberships on
  processors that will actually board adult content (NMI ISOs like MobiusPay/PaymentCloud,
  CCBill). You get duplicate-subscription prevention, dunning that chases failed rebills,
  and entitlements that survive lost webhooks — the subscription-lifecycle plumbing every
  paysite rebuilds badly.
- **Building a Patreon or Substack** — creator/membership tiers with upgrades,
  downgrades, and proration. Your app asks "is this person a patron at tier 2 right now?"
  against its own database, not a provider API.
- **Building a Gumroad, itch.io, or Teachable** — videos, courses, games, downloads sold
  individually. Purchases can grant permanent ownership or time-limited rental access; your
  server checks the stored access grant. There is one checkout model whether the buyer pays by card or USDC.
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
- **The customer conversation, handled** — templated, deduplicated payment-failure and
  card-expiry emails that supersede themselves when the problem resolves.
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
- **Sandbox that can't lie.** `test_mode=sandbox` doesn't trust configuration — it
  proves it: live Stripe keys refuse to boot, and every NMI account is probed with a
  card only a simulator would approve. No real money can move in a sandbox boot, in any
  environment.
- **Your users never pay for our malfunction.** Access is closed by *evidence* — a
  confirmed cancellation, a terminal decline, exhausted dunning — never by the clock
  alone. A lost webhook, a dead pipe, or stale data parks the subscription for
  verification with access intact. Terminal cancellation is the last resort, not the
  default.
- **Explicit merchant isolation.** Authorization selects the merchant; scoped SQL
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

- **Redirect flow**: Browser -> payment provider's checkout page (ex. Stripe) -> user enters credit card details -> payment provider redirects back to your frontend. Behind the scenes Stripe webhook -> Your OpenRails server -> updates entitlements in your database.
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
money: no checkout, no dunning, no ledger, no rails. And metering is after-the-fact by
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

### Quickstart

```bash
# Try the full stack locally (Postgres + Garnet + OpenRails, zero-config):
task docker-up
curl http://localhost:3053/health/ready

# Or add it to your Go app as a library:
go get github.com/open-rails/openrails
```

The local Compose profile defaults to sandbox credentials with provider writes enabled
(`TEST_MODE=sandbox`, `PROVIDER_WRITE_MODE=full`). Set `PROVIDER_WRITE_MODE=readonly`
before `task docker-up` when you want a read-only local stack.

Then pick your integration path below.

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
the next. All development runs against sandbox rails (test_mode=sandbox);
prove the full checkout → webhook → entitlement flow end-to-end before ever
touching live credentials.
```

The agent-facing guide itself lives at [docs/agent-integration.md](docs/agent-integration.md).

---

### Documentation

**Integrate** — for developers wiring OpenRails into an application:

- [Embedded integration (Go library)](docs/embedded-integration.md) — run the engine in-process: boot, migrations, declaring your merchant, mounting the billing routes on your server, calling the in-process client.
- [Standalone integration (service)](docs/standalone-integration.md) — deploy OpenRails as its own service: production config, first-run provisioning, API keys, the Go SDK and plain-HTTP integration.
- [Frontend integration](docs/frontend-integration.md) — the browser side: self-service routes, checkout sessions, payment methods, tokens, and error handling.
- [`@openrails/billing-ui`](sdk/billing-ui/README.md) — the embeddable checkout and account-billing React UI; each release attaches `openrails-billing-ui-X.Y.Z.tgz`.
- [The auth model](docs/auth.md) — one credential per trust domain: why embedded uses your session credential and standalone uses delegated tokens.
- [Products and prices through the Client](docs/catalog-client.md) and [choosing the merchant a call acts on](docs/client-merchant-selection.md).
- [Batch import / legacy migration](docs/batch-import.md) — moving an existing subscriber base onto OpenRails: the import surface, the phased playbook, and the limited-mode cutover.

**API** — the contract:

- [API guide](docs/api/endpoints.md) — conventions and behavior; [every route](docs/api/routes.md); [every error code](docs/api/error-codes.md); [`api/openapi.json`](api/openapi.json).
- [Checkout](docs/api/commerce.md), [errors](docs/api/errors.md) and [money, ids and times on the wire](docs/money-wire.md).
- [Compatibility](docs/compatibility.md) — what v1 freezes and what counts as additive.
- [Migrating to v1](docs/migrating-to-v1.md) — everything a v0 host changes.

**Payment rails** — per-rail setup: credentials, the manifest entry, webhooks, sandbox testing:

- [Rail matrix](docs/rails/certification-matrix.md) — what each rail does, and what evidence backs a verification claim
- [NMI](docs/rails/nmi.md) (MobiusPay, PaymentCloud, PayKings, and other NMI-backed ISOs)
- [Stripe](docs/rails/stripe.md)
- [CCBill](docs/rails/ccbill.md)
- [Solana](docs/rails/solana.md) (USDC, self-custody, on-chain recurring subscriptions)
- [Payment-method custody](docs/payment-method-custody.md) — who *holds* a stored card vs who *charges* it
- [Sandbox posture](docs/sandbox-posture.md) — how a sandbox deployment proves its credentials are test credentials

**Run your business** — for the merchant defining plans and managing customers:

- [Merchant guide](docs/merchant-guide.md) — authoring the catalog (products, prices, rate cards, entitlements), applying it, and day-to-day customer management.
- [Admin console](docs/admin-console.md) — the merchant portal: turning it on/off, building the assets, and using it.
- [Entitlements](docs/entitlements_timeline.md) — the access-timeline model your app reads.
- Usage billing — [request admission](docs/admission-operations.md), [billing policies](docs/billing-policies.md), [arrears and delinquency](docs/arrears-delinquency.md), [invoice administration](docs/invoice-administration.md), [provider obligations](docs/architecture/provider-obligation-contract.md).

**Operate the deployment** — for whoever keeps it running:

- [Operator guide](docs/operator-guide.md) — infrastructure requirements (Postgres, Redis/Garnet, Vault), what runs by itself, and the drift toolbox.
- [Operations manual](docs/operations.md) — the deep reference: operating modes and safety levers, dunning, reconciliation, the provider intent ledger, data retention, cutovers.
- [Merchant provisioning](docs/merchant-provisioning.md) — manifests, credentials, secrets, and API keys; [merchant names](docs/merchant-name-authority.md); [settings](docs/api/merchant-settings.md) and [configuration applications](docs/merchant-configuration-applications.md).
- [Self-hosting with host-owned credentials](docs/self-hosting-mode1.md), [runtime configuration](docs/runtime-configuration.md) and [Vault](docs/vault.md).
- [Backup and recovery](docs/backup-and-recovery.md), [moving a merchant](docs/merchant-portability.md) and [provider uncertainty](docs/provider-uncertainty.md).
- [Rate limiting](docs/rate-limiting.md) — the built-in limits on checkout, card and subscription writes, and captcha escalation.

**Reference**

- [Glossary](docs/glossary.md) — rails, PSPs, merchants, customers, and the rest of the vocabulary.
- [Metrics & query API](docs/metrics-for-llms.md) — analytics access for dashboards and LLM agents.
- [Contributing / hacking on OpenRails](docs/dev/README.md) — dev workflow, testing, local webhooks.
- [Injected-code scan](docs/injection-scan.md) — the supply-chain gate every clone runs; rules, exclusions, and what to do when it fires.

---

### Injected-code scan (required for every clone)

The script `scripts/scan-injected-code.sh` scans the whole tree on every push and pull
request and blocks the merge if worm-injected code is detected.

Install the pre-commit hook once per clone:

```bash
./scripts/install-git-hooks.sh
```

Manual use: `./scripts/scan-injected-code.sh` (whole tree), `--staged`,
`--range origin/master...HEAD`, or `--self-test`. Rules, exclusions and what to
do when it fires are in [docs/injection-scan.md](docs/injection-scan.md).

