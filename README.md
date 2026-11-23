# OpenRails

OpenRails is a billing engine you own. Embed it in a Go server or run it as its
own service, and it holds your customers' billing state: the catalog of products
and prices and what each grants, orders and payments, subscriptions and their
renewals, retries of failed payments, prepaid balances, metered usage, and who
holds what right now.

It is not a payment processor: a PSP (an NMI gateway, Stripe, CCBill, or a Solana
wallet) still moves the money. OpenRails replaces the processor-specific logic
around it (catalog, billing cycles, dunning, entitlements) with one system, so
you can vault cards with the processor of your choice, switch processors, or use
several, without a new integration each time.

- **Rails:** any NMI-backed gateway, Stripe, CCBill, and Solana with USDC
  (one-off payments and on-chain recurring subscriptions).
- **Sells:** one-off purchases, rentals and timed passes, bundles, recurring
  memberships with tiers, seats and proration, prepaid credit packs, and metered
  usage billed from a balance or in arrears.
- **Answers:** "does this user hold this entitlement now?", from your own
  database, never a provider API.

### Example apps

- **Creator and adult sites:** recurring memberships on processors that board
  high-risk merchants, with duplicate-subscription prevention, dunning, and access
  that survives lost webhooks.
- **Digital storefronts:** courses, videos or downloads sold one at a time,
  permanently or as rentals.
- **SaaS:** a tier list billed per seat, with upgrades and downgrades.
- **API platforms:** prepaid balances drawn down per request, holds before
  expensive work, spend limits, or monthly invoices in arrears.

---

### How to Install (Embedded)

A course site where users sign in with [AuthKit](https://github.com/open-rails/authkit),
buy courses one at a time or as a bundle, rent one for 3 days, or join a monthly
membership. [`examples/embedded`](examples/embedded) is the whole program; the
[embedding guide](docs/embedded.md) covers every option.

```sh
go get github.com/open-rails/openrails
```

Declare what you sell:

```yaml
# catalog.yaml
schema_version: 1
products:
  course-101:
    display_name: Course 101 — Intro to CSS
    entitlements: ["course:101"]
    prices:
      purchase: { amount: 4.99 USD, access_duration: forever }
      rent:     { amount: 1.99 USD, access_duration: 3 days }
  course-bundle:
    display_name: CSS courses 101 and 102
    entitlements: ["course:101", "course:102"]
    prices:
      purchase: { amount: 8.99 USD, access_duration: forever }
  channel-membership:
    display_name: Channel membership
    entitlements: ["channel:membership"]
    prices:
      monthly: { amount: 10 USD, billing_interval: 30 days }
```

Entitlements are strings your app gives meaning to. Then declare the merchant and
its PSP (this file holds secrets; keep it out of version control):

```yaml
# merchant.yaml
slug: onlydemo
display_name: OnlyDemo
psps:
  nmi-sandbox:
    rail: nmi
    environment: sandbox # live (real money; the default), test or sandbox
    gateway_id: "000000" # the NMI dashboard's Gateway ID
    tokenization_key: your-public-tokenization-key
    secrets:
      security_key: your-private-security-key
      webhook_signing_secret: your-webhook-signing-key
```

Build the engine and mount its routes behind your auth:

```go
merchant, err := openrails.ReadMerchantFile("merchant.yaml")
if err != nil { return err }
merchant.Issuer = "https://shop.example" // your AuthKit's: its users are this merchant's customers and staff

bill, err := openrails.New(ctx, openrails.Config{
	ProviderWriteMode: openrails.ProviderWritesFull, // ProviderWritesReadOnly never charges
	Merchants:         map[string]openrails.MerchantDeclaration{merchant.Slug: merchant},
}, openrails.Deps{Postgres: db, UserInfo: ak.UserInfo()}) // applies its migrations
if err != nil { return err }
defer bill.Close(context.WithoutCancel(ctx))
shop, err := bill.MerchantBySlug(merchant.Slug) // the merchant's Go client
if err != nil { return err }
if err := bill.Start(ctx); err != nil { return err } // renewals, retries, invoices

err = openrailsgin.Mount(r, bill, openrails.Routes{
	Auth:        ak.Authenticator(), // says who a request is; OpenRails decides what each route admits
	Prefix:      "/billing",         // serves /billing/v1/*
	RouteGroups: openrails.RouteGroups{Admin: true, Metrics: true},
})
```

Staff are ordinary users holding a role built from OpenRails' permission constants:

```go
rbac := authkit.NewRoles()
perm := func(p billing.Permission) iam.Perm { return rbac.Root.Permission(p.Resource(), p.Action()) }
rbac.Root.Role("support", perm(billing.RootCustomerRead), perm(billing.RootCustomerManage))
rbac.Root.Role("operator", perm(billing.RootCustomerRead), perm(billing.RootCustomerManage),
	perm(billing.RootCatalogManage), perm(billing.RootConfigManage), perm(billing.RootMetricsRead))
```

Apply the catalog from a command of your own, once and whenever the file changes;
starting the server never does:

```go
doc, err := catalog.ReadFile("catalog.yaml")
if err != nil { return err }
receipt, err := shop.ApplyCatalog(ctx, doc, billing.ApplyCatalogParams{})
if err != nil { return err }
return openrails.WriteCatalogReceipt(os.Stdout, receipt) // what changed; an unchanged file changes nothing
```

#### Sell Your Content

Your server owns the content; OpenRails owns prices and who holds what. Gate a
course with one read:

```go
check, err := shop.CheckEntitlements(ctx, billing.CheckEntitlementsParams{
	CustomerID: customerID, // your user's id
	Entitlements: []string{"course:101", "channel:membership"},
})
if err != nil { return err }
if check.Entitlements["course:101"] || check.Entitlements["channel:membership"] {
	// serve it
}
```

To sell, your server reads the products (`shop.ListProducts`, with
`billing.BuyerPrices`) and puts them in its own page; the browser never reads the
catalog. [`@openrails/billing-ui`](sdk/billing-ui/README.md) sells them: its
`<BuyButton price>` and `<ProductList products>` create the order, take the card in
the PSP's own fields, and call `onPaid`:

```tsx
export const billing = createBillingClient({ baseUrl: "/billing/v1", fetch: auth.authFetch })

<BillingProvider client={billing}>
  <BuyButton price={price} signedIn={signedIn} onSignInRequired={signIn} onPaid={() => navigate("/courses/css-101")} />
</BillingProvider>
```

The example's [`content.go`](examples/embedded/content.go) and
[`web/`](examples/embedded/web) are the whole store: paged course lists with one
product and one entitlement read per page, a `402` with what unlocks a course, and
signed media URLs. Your own auth instead of AuthKit:
[`examples/own-auth`](examples/own-auth) and [auth](docs/auth.md#your-own-auth).

### Standalone

Run the `openrails` server (image `docker.io/openrails/openrails:vX.Y.Z`, or the
binary on each release) beside any app, with PostgreSQL 18 and, for several
replicas, Redis. Your app's own identity provider is the merchant's trusted
issuer: browsers call the server directly with a scoped token, and your backend
uses the same Go client over HTTP:

```go
client, err := openrails.NewRemoteMerchant("https://billing.example.com",
	openrails.RemoteCredential{Token: token}) // client credentials from your issuer
```

[`examples/standalone`](examples/standalone) runs it with Docker Compose;
[standalone](docs/standalone.md) explains configuration, trust and the backend, and
[Kubernetes](docs/kubernetes.md) the Helm chart.

### Hosted

[openrails.dev](https://openrails.dev) runs OpenRails for many merchants. Sign up,
add your PSPs and catalog in the dashboard, create a service token for your
backend (`openrails.NewRemoteMerchant(url, openrails.RemoteCredential{APIKey: …})`)
and register your signing application so your users' tokens reach `/v1/me`.
[`examples/hosted`](examples/hosted) is the same app against the platform.

---

### Concepts

| Term | Means |
|---|---|
| Merchant | a billing namespace: one business, its customers, catalog and PSPs |
| Rail / PSP | a gateway kind (`nmi`, `stripe`, `ccbill`, `solana`) / the merchant's account on one |
| Customer | your user, by its id; OpenRails keeps no accounts of its own |
| Product, price | what is sold and its immutable terms; a recurring price renews |
| Entitlement | a string a product grants while held; your app checks it |
| Order | one purchase of priced lines; renewals and usage bills are orders too |
| Subscription | a recurring price held over time, with one collector: OpenRails or the provider |
| Ledger | double-entry money, the source of truth; a separate grant ledger tracks credit |

Amounts are integers in each currency's native units (micros for USD), decimal
strings on the wire. More in the [glossary](docs/glossary.md).

### What it does

- **One purchase model** across rails: tokenized card fields (NMI Collect.js,
  Stripe Elements), provider-hosted pages, Solana Pay, Apple Pay and Google Pay
  through Stripe.
- **Renewals OpenRails collects** on saved cards and on-chain plans, with dunning
  derived from the billing cycle: hard declines stop at once, soft ones retry, and
  a months-old failure is parked for verification with access kept, never charged.
  Provider-scheduled subscriptions stay with the provider and are followed.
- **Access closes on evidence only:** a confirmed cancel, a terminal decline or
  exhausted dunning, never our own uncertainty.
- **Provider writes are durable intents,** idempotency-keyed and resolved by
  reading the provider before any retry; a charge is never blind-retried.
- **Environments the provider confirms:** each PSP declares `live`, `test` or
  `sandbox` and the provider must agree before it is used.
- **Metered billing and spend control:** holds before work, rate cards, budgets,
  trust levels, prepaid credit and arrears invoicing.
- **Browser self-service** on `/v1/me` (orders, subscriptions, cards, balances)
  with no proxy routes to write.
- **Imports:** a merchant's existing subscribers from its PSPs or a previous
  billing system.
- **Operations:** findings for every divergence from provider truth, proposals a
  person approves, reversible prunes, a kill switch, metrics and a staff console.
- **Embedded and standalone share one client and one handler surface;** time is
  injected everywhere, so a year of renewals runs in a test.

By default card details go from the browser to the PSP or a card custodian, never
through your server or OpenRails, which keeps you in PCI SAQ-A scope. An NMI PSP
may opt into server card entry, which puts your servers in SAQ-D scope
([NMI](docs/rails/nmi.md#card-entry-browser-or-server)).

### When not to use it

- A simple, low-risk SaaS that fully trusts Stripe: Stripe Billing is less work.
  OpenRails earns its keep with a local source of truth for access, spend control
  for metered work, or the freedom to switch rails when a processor drops you.
- You need polished invoicing, tax and revenue recognition today.
- You cannot run PostgreSQL and do not want [the hosted platform](#hosted).

### Requirements

- PostgreSQL 18+, which can be your app's database (OpenRails owns one schema).
- Redis or Garnet when running more than one instance.
- Embedded: a Go server. Vault is optional for editing merchant configuration at
  run time.

### Integrate with an AI agent

```text
Integrate OpenRails (github.com/open-rails/openrails), a self-hostable billing
engine, into this application. First read the agent integration guide:
https://raw.githubusercontent.com/open-rails/openrails/master/docs/agent-integration.md

Before writing code, confirm with me: embedded (this app is Go) or standalone;
which rails (NMI gateway, Stripe, CCBill, Solana) and whether I have sandbox
credentials; and what we sell (subscriptions, one-time purchases, metered usage).
Follow the guide's milestones in order on non-live PSPs (environment test or
sandbox), proving purchase → webhook → entitlement end to end before any live
credential.
```

### Documentation

**Integrate**
- [Embedded](docs/embedded.md), [standalone](docs/standalone.md),
  [frontend](docs/frontend.md) and [`@openrails/billing-ui`](sdk/billing-ui/README.md)
- [Auth](docs/auth.md): credentials, merchant binding, permissions, your own auth
- [Catalog](docs/catalog.md), [catalog export](docs/catalog-export.md),
  [prepaid credits](docs/prepaid-credits.md), [entitlements](docs/entitlements_timeline.md)
- [Usage billing](docs/usage-billing.md), [spend limits](docs/spend-limits.md),
  [running charges](docs/running-charges.md), [agreements](docs/agreements.md),
  [recurring credits](docs/recurring-credits.md)
- [Events and notifications](docs/events.md), [customer contacts](docs/customer-contacts.md)
- [Payment page](docs/payment-page.md), [agent integration](docs/agent-integration.md)

**API**
- [Conventions](docs/api/conventions.md), [errors](docs/api/errors.md),
  [every route](docs/api/routes.md), [every error code](docs/api/error-codes.md),
  [`api/openapi.json`](api/openapi.json), [compatibility](docs/compatibility.md)

**Rails**
- [Overview](docs/rails/README.md): [NMI](docs/rails/nmi.md),
  [Stripe](docs/rails/stripe.md), [CCBill](docs/rails/ccbill.md),
  [Solana](docs/rails/solana.md)
- [PSP environments](docs/psp-environments.md),
  [payment-method custody](docs/payment-method-custody.md)

**Run the business**
- [Merchant operations](docs/merchant-operations.md), [dunning](docs/dunning.md),
  [billing admin](docs/billing-admin.md), [metrics](docs/metrics-for-llms.md)
- [Merchant configuration](docs/merchant-configuration.md),
  [PSP import](docs/psp-import.md), [batch import](docs/batch-import.md),
  [provider-managed orders](docs/provider-managed-orders.md)

**Operate**
- [Operations](docs/operations.md), [configuration](docs/configuration.md),
  [limits](docs/limits.md), [destructive actions](docs/destructive-actions.md),
  [Vault](docs/vault.md), [Kubernetes](docs/kubernetes.md)
- [Provider uncertainty](docs/provider-uncertainty.md),
  [merchant portability](docs/merchant-portability.md),
  [server package](docs/server-package.md),
  [tenant binding](docs/embedding-tenant-binding.md)

**Contribute**
- [Contributing](docs/dev/README.md), [testing](docs/dev/testing.md),
  [local webhooks](docs/dev/local-webhooks.md), [injected-code scan](docs/dev/injection-scan.md)

### License

OpenRails is under the Elastic License 2.0 ([LICENSE](LICENSE)): use, change
and self-host it for any purpose, except offering it to others as a hosted or
managed service. The browser SDK [`@openrails/billing-ui`](sdk/billing-ui) is MIT
([sdk/billing-ui/LICENSE](sdk/billing-ui/LICENSE)).
