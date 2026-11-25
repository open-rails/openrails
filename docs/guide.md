# OpenRails integration guide

OpenRails is a billing engine you add to an existing application. It holds a merchant's catalog, orders, subscriptions, prepaid balances and metered usage, collects through NMI, Stripe, CCBill or Solana, emails customers their receipts and billing notices ([customer messages](#customer-messages)), and answers what each customer may access. Your app keeps its users and sign-in; a customer is your user's id.

## Choose a path

| Path | Billing runs | Your backend calls it with | Guide |
|---|---|---|---|
| Embedded | in your Go server's process, on your PostgreSQL | the in-process `*openrails.Client` | [Embedded](embedded.md) |
| Standalone | as the `openrails` server you host | the same `*openrails.Client` over HTTP, with your issuer's tokens or an API key | [Standalone](standalone.md) |
| SaaS | on the hosted platform | the same `*openrails.Client` over HTTP, with a console API key | [Client: credentials](client.md#credentials) |
| HTTP only | any of the three | JSON over HTTP from any language | [Without the Go client](client.md#without-the-go-client) |

Your customers never sign in to OpenRails; who presents which credential, in each path, is [authentication](authentication.md).

Every path serves the same HTTP API and the same browser SDK,
[`@openrails/billing-ui`](../web/billing-ui/README.md). The [client guide](client.md)
covers everything the client does; the rest of this page is the shortest
working integration, embedded, as [`examples/embedded`](../examples/embedded) runs it.

## A minimal integration

**1. Install.** OpenRails needs PostgreSQL 18 or newer; it creates and migrates
its own schemas (`billing` and `billing_river`) through your pool.

```sh
go get github.com/open-rails/openrails@latest
```

**2. Declare the merchant and one PSP.** A merchant is the seller; a PSP is its
account on a payment rail. Start with the PSP's non-live environment and keep
the file out of version control. Per-rail fields:
[merchant configuration and PSPs](client.md#merchant-configuration-and-psps).

```yaml guide:merchant
slug: onlydemo # your merchant's name: lowercase letters, digits and hyphens
display_name: OnlyDemo
psps:
  nmi-main: # your name for this account; any key you like
    rail: nmi # the gateway kind: nmi, stripe, ccbill or solana
    environment: sandbox # live (real money; the default), test (an NMI account in test mode) or sandbox (a sandbox.nmi.com account)
    gateway_id: "000000" # the NMI dashboard's Gateway ID
    tokenization_key: your-public-tokenization-key # the browser's card fields use it
    secrets:
      security_key: your-private-security-key
      webhook_signing_secret: your-webhook-signing-key
```

Build the engine from it, on your pool, and take the merchant's handle:

```go guide:embedded-new
// You, the seller, and your payment processor accounts (PSPs), declared in
// merchant.yaml. It carries PSP credentials: keep it out of version control.
raw, err := os.ReadFile("merchant.yaml")
if err != nil {
	return nil, nil, err
}
merchant, err := openrails.ParseMerchantDocument(raw)
if err != nil {
	return nil, nil, err
}

cfg := openrails.Config{
	Database: billing.DatabaseConfig{Schema: "billing"}, // the Postgres schema OpenRails' tables go in
	Merchants: []billing.MerchantDeclaration{{
		MerchantDocument: merchant,
		Issuer:           issuer, // your users, signed in at your AuthKit, are this merchant's customers and staff
	}},
}

// Build the billing engine; it creates or upgrades its own tables. It has no logins
// of its own: your auth guards its routes when you mount them.
bill, err := openrails.New(ctx, cfg, openrails.Deps{
	Postgres: db,            // the same pool your app uses
	UserInfo: ak.UserInfo(), // AuthKit's user lookup: each answer is the user's current email, name and username
})
if err != nil {
	return nil, nil, err
}
// Your merchant's handle: the Go client your own code calls, in process.
id, err := bill.DeclaredMerchant(merchant.Slug)
if err != nil {
	_ = bill.Close(context.WithoutCancel(ctx))
	return nil, nil, err
}
shop, err := bill.Merchant(id)
if err != nil {
	_ = bill.Close(context.WithoutCancel(ctx))
	return nil, nil, err
}
```

**3. Mount the routes with your auth.** `Routes.Auth` says who a request is;
OpenRails decides what it may do. Public, customer (`/v1/me`) and webhook
routes always mount; staff groups mount only when `RouteGroups` turns them on,
and their roles hold `billing.Permissions()` ([auth](embedded.md#auth)).

```go guide:embedded-mount
// Billing. Public, customer (/me) and webhook routes are always mounted.
err = openrailsgin.Mount(r, bill, openrails.Routes{
	Auth:        ak.Authenticator(), // says who a request is; OpenRails decides what to admit
	Prefix:      "/billing",         // the API is served at /billing/v1/*
	RouteGroups: openrails.RouteGroups{Admin: true, Catalog: true, MerchantConfig: true, Metrics: true},
})
if err != nil {
	return err
}
```

Then `bill.Start(ctx)` runs the workers: renewals, retries and provider operations.
Point the PSP's webhooks at `/billing/v1/webhooks/{rail}/{account_id}` on your
server. Starting OpenRails never changes the catalog.

**4. Apply a catalog.** Products grant entitlements, plain strings your app gives
meaning to; prices are their terms. Amounts are written `4.99 USD` and stored as
native units (micros for USD).

```yaml guide:catalog
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

Applying is an operator step, like a deploy: run your app's own command, which prints what changed. Reapplying a file that applied whole, even an older one, replays its receipt and changes nothing.

```go guide:catalog-apply
products, err := catalog.ReadFile(path)
if err != nil {
	return err
}
receipt, err := shop.ApplyCatalog(ctx, products, billing.ApplyCatalogParams{})
if err != nil {
	return err
}
if _, err := io.WriteString(out, receipt.String()); err != nil {
	return err
}
if n := len(receipt.Conflicts); n > 0 {
	return fmt.Errorf("%s skipped %d object(s) an edit changed", path, n)
}
return nil
```

**5. Sell with billing-ui.** The browser never reads the catalog: your server
puts the products it sells, priced as a buyer sees them, in its own page.

```go guide:buyer-prices
listed, err := bill.ListProducts(c, billing.ProductListParams{Archived: new(false), ForSale: new(true), Keys: keys, PageRequest: billing.PageRequest{Limit: len(keys)}})
if err != nil {
	c.AbortWithStatus(http.StatusServiceUnavailable)
	return
}
// Each product's prices as a buyer sees them, for the page's BuyButtons.
prices := map[string][]billing.Price{} // by product key
for _, product := range listed.Items {
	prices[product.Key] = billing.BuyerPrices(product.Prices)
}
```

The page sells them signed in as the customer, with the PSP's own card fields:

```tsx
const billing = createBillingClient({ baseUrl: "/billing/v1", fetch: auth.authFetch })

<BillingProvider client={billing}>
  <ProductList products={products} signedIn={signedIn}
    onSignInRequired={openSignIn} onPaid={() => navigate("/courses/css-101")} />
</BillingProvider>
```

`onPaid` is a hint for the page. Grant access from what OpenRails holds: an
entitlement check (below) or the `order.completed` event
([events](client.md#events)), never the browser's word.

**6. Gate access.** Ask OpenRails which keys the signed-in user holds, in one
call per page, where you serve the protected feature:

```go guide:entitlement-check
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
```

Never infer access from a subscription's status: a canceled subscription keeps
its paid access, and the keys follow the product's current entitlements.

**7. Go live.**

- Buy once in the PSP's sandbox: the order completes, the check answers `true`,
  and a refund or cancel behaves as you expect.
- Declare the live PSP (`environment` omitted means real money) and register its
  webhook URL and signing secret; OpenRails checks the credentials before arming it.
- A merchant writes to its providers unless its controls hold it (`openrails merchant writes`, [merchant controls](standalone.md#merchant-controls)).
- Register `Ready` and `Probes()` with your supervisor ([embedded](embedded.md#health)).

## Customer messages

OpenRails emails customers their billing messages through `Deps.Email` (the
server's `email_smtp`), from the profile's `from_email`, to the address in your
directory. A message is a notification [event](client.md#events) whose type is
on; it is queued with its fact, so a replayed fact sends once.

**On by default:** receipts (`one_off_purchase_completed`, `premium_started`,
`refund_completed`), payment problems (`payment_method_failed`,
`payment_method_update_required`, `payment_authentication_required`), the
cancellation confirmations (`subscription_canceled`; `premium_ended` when the
customer, staff or failed payments ended it; `order_collection_stopped`) and the
price-change notice (`subscription_reprice_scheduled`). Every other notification
type has a built-in English message and is off. A declined checkout, the failure
that ended a subscription and a refund the provider reports more than seven days late send none.

A message ends in the log as `sent` or why not: `disabled`, `test_environment` (its PSP is a test or sandbox account), `no_contact`, `rate_limited` (ten an hour per customer, with a `life.customer.messages_rate_limited` finding) or `failed`. A send is retried, ten attempts in all; only a mail server's refusal of the recipient is final, and a refused sender opens `life.customer.messages_sender_refused` until one is accepted. Both findings alert the merchant's operator once.

**The `messages` document** turns types on or off and replaces any message. It
sits beside the merchant document (`MerchantDocument.Messages`; in Vault,
`<scope_prefix>/merchants/<merchant-id>/messages`):

```yaml guide:messages
messages:
  default_language: en
  events:
    premium_renewed:
      enabled: true # on, with its built-in message
    one_off_purchase_completed:
      templates:
        en:
          subject: "Your {{.Merchant.Name}} receipt"
          text: "You paid {{.Event.Amount}} for {{.Event.ProductName}}."
          html: "<p>You paid <strong>{{.Event.Amount}}</strong> for {{.Event.ProductName}}.</p>"
  layouts:
    en:
      text: |
        {{template "body" .}}
        -- {{.Merchant.Name}}
```

A template is a `subject` and a `text` or `html` body, Go `text/template` and
`html/template` over `billing.MessageData` (version 1), whose fields are text
formatted for the customer and empty when absent (test them with `with`):

| Field | |
|---|---|
| `.Customer` | `ID`, `Name` (the directory's name, else username) |
| `.Merchant` | the profile: `Name`, `LogoURL`, `SupportURL`, `BillingURL`, `SubscribeURL`, `TermsURL`, `PrivacyURL`, `StatementDescriptor` |
| `.Event` | the fact's fields: amounts with their currency (`12.50 USD`), UTC dates (`January 2, 2006`), `Reason`, `ProductName`, `Voided`, ids |
| `.Links` | `ManageBilling`, `UpdatePaymentMethod`, `Pay` (`billing_url`), `Resubscribe` (`subscribe_url`), each falling back to `support_url` |

A layout wraps every body where it runs `{{template "body" .}}`; a part you leave
out is the built-in one. Templates may call `and`, `or`, `not`, `eq`, `ne`, `lt`,
`le`, `gt`, `ge`, `len`, `default`, `upper` and `lower`, but not `range`,
`define`, `template` or a variable; each part is at most 64 KiB. Messages are
written in `default_language`, else `en`.

**Validation.** The document is checked as it loads: every field a template
names, in any branch, must exist, and every message must render against sample
data. A refused document is not served (the previous one keeps serving) and a
`consistency.merchant_config.not_served` finding names the event type and error.

**Preview, test and the log** ([client](client.md#customer-messages)):
`PreviewMessage` (`POST /v1/merchant/messages/preview`) renders a type, or a
candidate template, over sample data and your profile; `CreateTestMessage`
(`POST /v1/merchant/messages/test`) sends that rendering to `alert_email` only,
ten an hour; `ListCustomerMessages` (`GET /v1/admin/customers/{customer_id}/messages`)
is the delivery log, kept two years and with no body. Embedded,
`Deps.MessageRenderer` renders any message in your code instead
([embedded](embedded.md#deps)).

## Where next

- [Embedded](embedded.md): `Config` and `Deps`, River, several instances,
  routes and auth, the checkout page, transactions with your own rows.
- [Standalone](standalone.md): hosting the server: configuration, Helm, Vault,
  trust, upgrades, backups, monitoring and every operator command.
- [Authentication](authentication.md): who signs in, and with what, as a customer, a backend or staff, in each path.
- [Client](client.md): every method by capability (catalog, checkout,
  subscriptions, credits, usage limits and the Gate, events, imports), SaaS and
  customer credentials, errors, idempotency, and the HTTP API without Go.
- Generated references: [routes](api/routes.md), [error codes](api/error-codes.md),
  [`api/openapi.json`](../api/openapi.json).
- Runnable examples: [`examples/`](../examples/README.md).
- [Architecture](architecture/README.md): how each system works and keeps its guarantees, for people changing or reviewing OpenRails.
