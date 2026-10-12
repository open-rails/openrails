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

You'll need a Go webserver, and Postgres (v18 or higher).

Here we'll build a simple app where users register / login with [AuthKit](https://github.com/open-rails/authkit), buy CSS courses individually or as a bundle, and can buy a recurring monthly or annual membership.

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
        access_duration: null # permanent access
      rent:
        amount: 1.99 USD
        access_duration: 3 days # access expires 3 days after purchase

  course-102:
    display_name: Course 102 — Intro to Tailwind
    entitlements: ["course:102"]
    prices:
      purchase:
        amount: 4.99 USD
        access_duration: null
      rent:
        amount: 1.99 USD
        access_duration: 3 days

  course-bundle:
    display_name: CSS courses 101 and 102
    entitlements: ["course:101", "course:102"] # the same keys as individual sales
    prices:
      purchase:
        amount: 8.99 USD # both courses at a discounted price
        access_duration: null

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

Products and their prices are maps keyed by a stable key you choose. The entitlements are arbitrary strings; OpenRails tracks these, but it's up to your application to give them meaning.

#### Build the billing client

You, the seller, and your payment processor account go in `merchant.yaml`. This
one is an NMI gateway; declare as many PSPs as you like. The file holds your
gateway's secret keys, so keep it out of version control:

```yaml
# merchant.yaml
slug: onlydemo # your merchant's name: lowercase letters, digits and hyphens
display_name: OnlyDemo
psps:
  mobius: # your name for this account; any key you like
    rail: nmi # the gateway kind: nmi, stripe, ccbill or solana
    account_id: "000000" # the NMI dashboard's "Gateway ID"
    settings:
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
	// You, the seller, and your payment processor accounts (PSPs), declared in merchant.yaml.
	merchant, err := openrails.ReadMerchantFile("merchant.yaml")
	if err != nil {
		return nil, nil, err
	}

	// What you sell. New applies each distinct catalog once, even across restarts.
	// Later edits through the Go client remain available and are not undone by a replay.
	products, err := catalog.ReadFile("catalog.yaml")
	if err != nil {
		return nil, nil, err
	}

	merchant.Catalog = products
	merchant.Issuer = issuer // your users, signed in at your AuthKit, are this merchant's customers and staff
	cfg := openrails.Config{
		Database:          openrails.DatabaseConfig{Schema: "billing"}, // the Postgres schema OpenRails' tables go in
		TestMode:          openrails.Sandbox,                           // Enforces that supplied PSP credentials must give access to test / sandbox environments only, or it throws an error
		ProviderWriteMode: openrails.ProviderWritesFull,                // Set to ProviderWritesReadOnly to prevent any billing
		Merchants:         map[string]openrails.MerchantDeclaration{merchant.Slug: merchant},
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
	shop, err := bill.MerchantBySlug(merchant.Slug)
	if err != nil {
		_ = bill.Close(context.WithoutCancel(ctx))
		return nil, nil, err
	}
	return bill, shop, nil
}
```

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

Every permission has two names. The `root:` one (`root:customer:read`), held in
AuthKit's root group, reaches every merchant. The `merchant:` one
(`merchant:customer:read`), held in a merchant's own group (the group whose id
is the merchant's id), reaches that merchant alone: declare those on a
`merchant` persona when you want staff of one merchant only.

Then build AuthKit with those roles. AuthKit signs your users in, and says
who each request to OpenRails' routes is. The rest of its config (email, SMS,
rate limits) is in AuthKit's README:

```go
ak, err := authkit.New(ctx, authkit.Config{
	Database: authkit.DatabaseConfig{Schema: "profiles"}, // AuthKit's tables, beside OpenRails' "billing"
	Token:    authkit.TokenConfig{Issuer: "https://myapp.com", IssuedAudiences: []string{"myapp"}},
	Keys:     authkit.KeysConfig{Path: "/vault/auth"}, // its signing keys
	Roles:    rbac,
}, authkit.Deps{Postgres: db}) // the same pool OpenRails uses
if err != nil {
	return err
}
```

Finally, mount OpenRails' routes with AuthKit guarding them:

```go
err = openrailsgin.Mount(r, bill, openrails.Routes{
	Auth:   ak.Authenticator(), // says who a request is; required. Public, customer (/v1/me) and webhook routes are always on
	Prefix: "/billing",         // the API is served at /billing/v1/*

	// Every other route group is off unless you turn it on here.
	RouteGroups: openrails.RouteGroups{
		Admin:          true,  // customer support: customers, their subscriptions, payments, refunds and orders
		Catalog:        true,  // the catalog: update products and prices
		MerchantConfig: true,  // merchant config: PSPs and their credentials, findings, settings, billing import and export
		Metrics:        true,  // business metrics, read-only: revenue, sales, the dashboard's charts
		Programmatic:   false, // /v1/app: your backend's own calls over HTTP, each with its permission, and SCIM user provisioning; embedded, your Go code calls the Client instead
	},

	// The staff dashboard at /billing/admin. It has one area per staff group above,
	// shown only when that group is on and the staff member holds its permission;
	// the admin area is read-only without billing.RootCustomerManage. Here support
	// sees and helps customers; operators see all four.
	AdminConsole: true,
})
```

You name no permission and no scope: each route checks its own, and `Mount`
fails for a route neither of whose names your AuthKit declares.

Every route group, and what turns it on. Anything not always on is off until
you turn it on, embedded or standalone:

| Route group | Turned on by | Caller must hold | Who may call it |
|---|---|---|---|
| public (`/v1/products`, `/v1/config`, an order's Solana Pay request) | always on | nothing | anyone |
| customer (`/v1/me`) | always on | a sign-in | a signed-in customer, for their own billing |
| webhooks (`/v1/webhooks`) | always on | a valid signature | payment processors |
| admin: customer support (`/v1/admin`) | `RouteGroups.Admin` | `root:customer:read`; changes `root:customer:manage` | staff, or your backend |
| catalog (`/v1/catalog`, `/v1/catalog/price-migrations`) | `RouteGroups.Catalog`; with a declared merchant's `Catalog`, the file skips what an edit changed | `root:catalog:manage` | operators |
| merchant config (`/v1/merchant`) | `RouteGroups.MerchantConfig` | `root:config:manage` | operators |
| business metrics, read-only (`/v1/metrics`, `/v1/metrics/dashboard`) | `RouteGroups.Metrics` | `root:metrics:read` | staff, or your backend |
| programmatic (`/v1/app`) | `RouteGroups.Programmatic` | an application credential holding the route's permission (`root:entitlements:read`, `root:usage:manage`, …) | your backend, never a person |
| user provisioning (`/v1/app/scim/v2`) | `RouteGroups.Programmatic`, without `Deps.UserInfo` | an application credential or a provisioning token | your user directory |
| admin console (`/admin`) | `AdminConsole: true`, with at least one staff group on | each area's permission | staff, in the browser |

Each permission is also admitted by its `merchant:` name, held in the scope of
the merchant the request acts on.

Customer-support staff use `merchant:customer:read` and
`merchant:customer:manage`. Catalog management uses `/v1/catalog/`; merchant configuration uses
`/v1/merchant/`, including findings and PSP refresh.
Metrics reads stay independently grantable. The old operator URLs under
`/v1/admin/` and the old `billing` permission names are not aliases; update
custom roles and trusted-issuer grants when upgrading.

Admin and operator changes do not require a recent sign-in. Applications can require 2FA
and DPoP for their admin credentials; destructive-operation ceilings provide
an additional limit on destructive actions.

#### Customer contact info

Your AuthKit is the source of truth for your users' emails and names. Embedded,
OpenRails keeps no copy: it asks your AuthKit whenever it sends a receipt or a
failed-payment notice, or shows a customer in the admin routes or console. Pass
it to `New`:

```go
bill, err := openrails.New(ctx, cfg, openrails.Deps{
	Postgres: db,
	UserInfo: ak.UserInfo(), // AuthKit's user lookup: each answer is the user's current email, name and username
})
```

Without AuthKit, implement `openrails.UserInfo` (`Get` by ids and `Search` by
text) over your own user table. Leave it out and OpenRails
sends your customers no email; in-app notices and host events still arrive.

A standalone or hosted OpenRails cannot reach into your process: its own
AuthKit keeps a copy of your users, in the merchant's directory, which your
directory pushes to over SCIM 2.0. Point AuthKit's `Provisioning` (or Okta, or
Entra ID) at the server's `/directory/scim/v2` with your application's own
token. AuthKit sends its changes every 5 minutes and reconciles daily, so the
copy stays current, and deleting a user erases it. Each customer request also
brings the user's current email and name in its access token, so someone who
signs up and buys at once is known before the next push. (Embedded, you can
keep a pushed copy in OpenRails instead: leave `UserInfo` out, turn on
`RouteGroups.Programmatic` and push to `/billing/v1/app/scim/v2`.)

#### Using your own auth

Without AuthKit, tell OpenRails who a request is and what they hold. You write
no middleware: OpenRails checks each route's permission and a person's recent
sign-in itself, and answers refusals (401, 403, sign in again) the same way
for every host. [`examples/own-auth`](examples/own-auth) is a complete one,
over its own users, sessions and roles tables, with its tests:

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

// CheckRecentSignIn: signed in within 15 minutes, for writes that move money.
func (v session) CheckRecentSignIn(context.Context) error {
	if time.Since(v.s.SignedInAt) > 15*time.Minute {
		return &auth.Challenge{Err: auth.ErrStepUpRequired, MaxAge: 15 * time.Minute}
	}
	return nil
}
```

The types are `github.com/open-rails/authkit/auth`. Mount with
`Routes{Auth: shopAuth{sessions}, ...}`, build your roles from
`billing.Permissions()`, and check your auth in CI with
`openrailstest.CheckAuth(t, routes, authtest.Cases{...})`
(`github.com/open-rails/authkit/auth/authtest`). Serving one merchant, set
`MerchantDeclaration.Issuer` to your issuer and return a zero `BoundScope`.

If your app already loads its own configuration (koanf, kong, flags), build the
same declaration in Go instead of reading `merchant.yaml`. Each rail has a typed
struct (`NMIPSP`, `StripePSP`, `CCBillPSP`, `SolanaPSP`) whose fields are its
secrets and settings, so a misspelled name fails to compile. `MerchantDeclaration`
and `PSPConfig` carry `yaml` tags, so they can also sit inside your own YAML
config:

```go
merchant := openrails.MerchantDeclaration{
	Slug: "onlydemo",
	PSPs: map[string]openrails.PSPConfig{
		"mobius": openrails.NMIPSP{
			AccountID:            conf.NMI.GatewayID,
			TokenizationKey:      conf.NMI.TokenizationKey,
			SecurityKey:          conf.NMI.SecurityKey,
			WebhookSigningSecret: conf.NMI.WebhookSigningSecret,
		}.PSPConfig(),
	},
}
```

#### Manage merchant config at runtime (Vault)

Name a KV mount and Vault holds merchant config; name only a Transit mount and
it only signs. Instead of declaring your configuration in a file, let your
staff manage it: declare only the merchant's slug (anything more is refused
beside Vault), connect OpenRails to HashiCorp Vault, and mount the
merchant-config routes (`RouteGroups.MerchantConfig`). Staff then add PSPs,
rotate credentials and change settings over HTTP or in the admin dashboard,
starting from empty, and OpenRails writes them to Vault, never to Postgres.
Automation can write the documents straight into Vault instead
([vault.md](docs/vault.md) has the paths and shapes):

```go
cfg := openrails.Config{
	Database:          openrails.DatabaseConfig{Schema: "billing"},
	TestMode:          openrails.Sandbox,
	ProviderWriteMode: openrails.ProviderWritesFull,
	Merchants:         map[string]openrails.MerchantDeclaration{"onlydemo": {Issuer: "https://shop.example"}}, // Vault holds the rest
	Vault: &openrails.VaultConfig{
		Address:    "https://vault.internal:8200",
		AuthMethod: "kubernetes", // or "approle", "token"
		K8sRole:    "onlydemo-billing",
		KVMount:    "onlydemo/kv", // the KV v2 mount merchant configuration lives in
	},
}
```

If your app already has an authenticated Vault client, pass it as
`Deps.Vault` instead of `Config.Vault`.

#### Integrate OpenRails into your server

Start OpenRails' background work, mount the billing routes next to AuthKit's,
and gate your content with `ListEntitlements`:

```go
func main() { log.Fatal(run(context.Background())) }

func run(ctx context.Context) error {
	db, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		return err
	}
	defer db.Close()

	rbac := authkit.NewRoles()

	// OpenRails' permissions, by its own names. Held on root, they reach every
	// merchant you serve.
	perm := func(p billing.Permission) iam.Perm { return rbac.Root.Permission(p.Resource(), p.Action()) }
	billingRead, billingManage := perm(billing.RootCustomerRead), perm(billing.RootCustomerManage)
	catalogManage, configManage, metricsRead := perm(billing.RootCatalogManage), perm(billing.RootConfigManage), perm(billing.RootMetricsRead)

	rbac.Root.Role("support", billingRead, billingManage)
	rbac.Root.Role("operator", billingRead, billingManage, catalogManage, configManage, metricsRead)

	ak, err := newAuth(ctx, db, rbac) // authkit.New, as above
	if err != nil {
		return err
	}
	defer func() { _ = ak.Close(context.WithoutCancel(ctx)) }()
	bill, shop, err := newBilling(ctx, db, ak) // the engine, and your merchant's handle
	if err != nil {
		return err
	}
	defer bill.Close(context.WithoutCancel(ctx))

	// Background work. OpenRails runs its own River workers: rebills, retries of
	// failed payments, usage orders.
	if err := ak.Start(ctx); err != nil {
		return err
	}
	if err := bill.Start(ctx); err != nil {
		return err
	}

	r := gin.Default()
	if err := authkitgin.Mount(r, ak); err != nil { // sign-up and sign-in under /api/v1
		return err
	}
	// Billing. Public, customer (/me) and webhook routes are always mounted.
	err = openrailsgin.Mount(r, bill, openrails.Routes{
		Auth:        ak.Authenticator(), // says who a request is; OpenRails decides what to admit
		Prefix:      "/billing",         // the API is served at /billing/v1/*
		RouteGroups: openrails.RouteGroups{Admin: true, Catalog: true, MerchantConfig: true, Metrics: true},
	})
	if err != nil {
		return err
	}

	// Which entitlement unlocks each video. Buying course-101 or the bundle grants
	// course:101; either membership price grants channel:membership.
	contentAccess := map[string]string{
		"css-101":      "course:101",
		"tailwind-102": "course:102",
		"members-qa":   "channel:membership",
	}
	r.GET("/videos/:id", authkitgin.Required(ak), func(c *gin.Context) {
		id := c.Param("id")
		entitlement, exists := contentAccess[id]
		if !exists {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		claims, _ := ak.VerifyRequest(c.Request)
		customer, err := billing.ParseCustomerID(claims.UserID) // each AuthKit user is their own customer
		if err != nil {
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}
		held, err := shop.ListEntitlements(c, billing.EntitlementListParams{CustomerIDs: []billing.CustomerID{customer}, Entitlements: []string{entitlement}})
		if err != nil {
			c.AbortWithStatus(http.StatusServiceUnavailable)
			return
		}
		if len(held.Items) == 0 { // a key not listed is not held
			c.JSON(http.StatusPaymentRequired, gin.H{"error": "access_required"})
			return
		}
		c.File("videos/" + id + ".mp4")
	})

	return r.Run(":8080")
}
```

`openrails.Routes` is what OpenRails mounts on your router. The public,
customer and webhook routes are always mounted; every other route group is off
until `RouteGroups` turns it on:

- `Auth`: your auth, which only says who a request is (`Authenticate`). Customer
  routes take a person acting for themself; staff routes ask the answer's
  `Can` for the route's permission, without a recent-sign-in check. OpenRails
  answers every refusal: 401 with a `WWW-Authenticate` challenge, 403, or
  503 when your auth cannot answer. Customer-network operations marked
  `sensitive` retain their recent-sign-in check.
- Permissions are OpenRails' own (`billing.Permissions()`), each named
  `persona:resource:action` in two personas. A route acting on a merchant
  admits a caller holding its `root:` name in your auth's root scope
  (authkit/auth `Root`: AuthKit's root group) or its `merchant:` name in that
  merchant's own scope (the AuthKit group whose id is the merchant's). With a
  route group on, `Auth` must be a `Root`, and `Mount` fails for a route
  neither of whose names it knows.
- `Prefix`: where the API lives; `/billing` serves `/billing/v1/*`.
- `RouteGroups.Admin`: customer support, the routes your staff and back office
  call at `/billing/v1/admin` to read and change customers' billing: payments
  and refunds, subscriptions, orders, credits, access and usage. Callers need
  `root:customer:read`; changes need `root:customer:manage` (grant a role only
  the read for read-only staff). OpenRails decides which routes read and which
  change.
- `RouteGroups.Catalog`: the catalog: products, prices, meters and their rates,
  archiving a product, applying a catalog document, moving subscribers
  between prices. Callers need `root:catalog:manage`. With `MerchantDeclaration.Catalog`
  too, the file skips what an edit changed (below); your Go code can edit the
  catalog either way.
- `RouteGroups.MerchantConfig`: the merchant's own configuration: settings,
  PSPs and their credentials, reading and resolving findings, pulling the PSPs'
  state again (`POST /v1/merchant/psps/refresh`), billing import and export, the metrics
  dashboard's layout. Callers need `root:config:manage`. Hosts upgrading must
  enable this group to keep the findings routes available; `Admin` alone
  no longer mounts them.
- `RouteGroups.Metrics`: business metrics, read-only: revenue, sales, usage and
  the dashboard's charts. Callers need `root:metrics:read`.
- `RouteGroups.Programmatic`: the routes at `/billing/v1/app`, for your own
  backend when it talks to OpenRails over HTTP (a standalone or hosted
  OpenRails, or a service of yours not written in Go): the content gate,
  usage and admissions, running charges, host events, and user
  provisioning. They take an application your auth says a request is (an API
  key, a client's own token) holding the route's permission, and refuse a
  person: `root:entitlements:read` for the content gate, `root:catalog:read`
  for catalog reads, `root:usage:manage`, `root:events:read`, `root:orders:manage`. Give a program a role holding only what its task
  needs. Embedded,
  your Go code calls the same operations on the `Client` and you can leave this
  off.
- `AdminConsole`: `true` serves the staff dashboard at `/billing/admin` (below);
  off by default.

`bill` is the engine: it starts the workers, mounts the routes and reports
readiness. `shop` is your merchant's handle, the Go `Client`: every admin,
catalog, merchant-config, metrics and programmatic route is a method on it,
and it checks no permission, since code that holds the engine is trusted. An
engine that serves several merchants hands out a handle for each,
`bill.Merchant(id)`; over HTTP the same `Client` comes from
`openrails.NewRemoteMerchant` with the merchant's credential. Every request
acts on the merchant its credential is bound to ([merchant
binding](docs/merchant-binding.md)).

Against a standalone or hosted OpenRails, your backend mints each signed-in
customer a 15-minute token for the browser with its registered signing key:

```go
shop, err := openrails.NewRemoteMerchant(serverURL, openrails.RemoteCredential{APIKey: apiKey, Assertion: &signingKey})
token, err := shop.CustomerToken(ctx, billing.CustomerTokenParams{CustomerID: customer})
// the browser sends token.AccessToken to /v1/me; mint another before token.ExpiresAt
```

That is the whole server integration, and it is the program in
[`examples/embedded`](examples/embedded). Your server never touches a card
number: the browser hands the card to the processor's tokenization iframe, and
OpenRails charges the stored token, rebills memberships at their interval,
retries failed renewals, and keeps `ListEntitlements` current. A rental's access
ends after its 3 days; a canceled membership keeps access until its paid term ends.

#### Admin dashboard

OpenRails ships a dashboard for your staff, the admin console, with four areas:
customer support (customers, subscriptions, payments and refunds, orders),
the catalog, merchant config (PSPs, findings, settings) and business metrics.
It is off by default; `AdminConsole: true` serves it at `/billing/admin`, over
the routes it drives. This one has only customer support:

```go
err = openrailsgin.Mount(r, bill, openrails.Routes{
	Auth:         ak.Authenticator(),
	Prefix:       "/billing",
	RouteGroups:  openrails.RouteGroups{Admin: true},
	AdminConsole: true,
})
```

- **Who signs in**: your staff, with their AuthKit accounts, through your
  AuthKit at `/api/v1` on the same origin. Every page calls the routes of its
  area, which check that the staff member holds that area's permission. A
  `sensitive` change after a stale sign-in asks them to confirm it is them.
- **What each sees**: an area appears only when its group is on and the
  staff member holds its permission; on load the console asks
  `GET /v1/admin/access`, which any signed-in staff member may call. Customer
  support is read-only without `root:customer:manage`. Editing the metrics
  dashboard's layout needs `root:config:manage` as well as
  `root:metrics:read`. Someone holding none of them sees only that they have
  no access.
- **Requirements**: at least one staff group on (`RouteGroups.Admin`, `Catalog`,
  `MerchantConfig` or `Metrics`), and a console build passed as
  `Deps.ConsoleAssets` ([admin console](docs/admin-console.md)); `Mount` fails
  without either.
- **Its own host**: the dashboard calls `/billing/v1` and `/api/v1` on its own
  origin, and the admin routes answer no cross-origin requests. To serve it at
  `billing.example.com`, send that host to this server.

The standalone server's switch is `admin_console.enabled` in `config.yaml`
(with `admin_console.path`), and its route groups are `route_groups` there.

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

#### Add a storefront

[`@openrails/billing-ui`](sdk/billing-ui) is a React package with an order
checkout, a cart and an account page. Its components call your mounted `/billing/v1`
routes directly, so you write no billing endpoints of your own. Install the
version that matches your OpenRails:

```sh
pnpm add @openrails/billing-ui@X.Y.Z
```

The storefront lists products with their current prices. A customer who already holds a
course's entitlement gets a link to watch it instead of a buy button:

```tsx
import { useEffect, useState } from "react"
import { AccountBilling, BillingProvider, BuyButton, createBillingClient } from "@openrails/billing-ui"
import { auth } from "./auth" // your auth-ui client: authFetch attaches the AuthKit token

const billing = createBillingClient({ baseUrl: "/billing/v1", fetch: auth.authFetch })

const courses = [
  { video: "css-101", entitlement: "course:101", product: "course-101", title: "Intro to CSS" },
  { video: "tailwind-102", entitlement: "course:102", product: "course-102", title: "Intro to Tailwind" },
]

export function Store() {
  const held = useEntitlements()
  return (
    <BillingProvider client={billing} appearance={{ theme: "auto" }}>
        {courses.map((course) => (
          <section key={course.product}>
            <h2>{course.title}</h2>
            {held.has(course.entitlement) ? (
              <a href={`/videos/${course.video}`}>Watch</a>
            ) : (
              <>
                <BuyButton product={course.product} price="purchase" label="Buy for $4.99" {...handlers} />
                <BuyButton product={course.product} price="rent" label="Rent for 3 days, $1.99" {...handlers} />
              </>
            )}
          </section>
        ))}
        <BuyButton product="course-bundle" price="purchase" label="Both courses for $8.99" {...handlers} />

        <h2>Channel membership</h2>
        {held.has("channel:membership") ? (
          <a href="/videos/members-qa">Members-only Q&A</a>
        ) : (
          <>
            <BuyButton product="channel-membership" price="monthly" label="$10 every 30 days" {...handlers} />
            <BuyButton product="channel-membership" price="yearly" label="$99 every 365 days" {...handlers} />
          </>
        )}

        {/* Memberships (cancel, resume, change card), saved cards and payment history. */}
        <AccountBilling plansHref="/store" collectionCurrency="USD" />
    </BillingProvider>
  )
}

// The signed-in customer's current entitlements: GET /billing/v1/me/entitlements.
function useEntitlements() {
  const [held, setHeld] = useState(new Set<string>())
  useEffect(() => {
    auth
      .authFetch("/billing/v1/me/entitlements")
      .then((response) => response.json())
      .then((page: { data: { entitlement: string }[] }) => setHeld(new Set(page.data.map((e) => e.entitlement))))
  }, [])
  return held
}

// BuyButton looks its price up in your merchant's public catalog, creates the
// customer's order and pays it in a dialog; your gate confirms access once it
// is paid.
const handlers = {
  merchant: "<your merchant's id>", // shop.MerchantID(), from your server
  onPaid: () => location.reload(),
  onSignInRequired: () => auth.signIn(),
}
```

Buying the bundle grants both `course:101` and `course:102`, so both courses show
**Watch**. A membership grants `channel:membership`, which unlocks members-only
videos but not the paid courses. `AccountBilling` lets members cancel, resume or
change the card for a failed renewal.

Every purchase is an order. `BuyButton` and `<Cart>` create the customer's
order and pay it in `<OrderCheckout>`, on your own page under your own auth: a
saved card, a new card from the PSP's own fields (charged and saved in one
request), a hosted Stripe page, or a Solana Pay request. A payment the bank
challenges answers `next_action`; the checkout runs it (Stripe.js 3-D Secure)
and confirms the order. A server that decides what its customer buys creates
the order with `Client.CreateOrder` and hands its id to the browser, which pays
it with `<OrderCheckout orderId={id} />`. A plan change the bank challenges is
completed with `authenticateChange`. See [billing-ui's
README](sdk/billing-ui/README.md).

#### Routes

Mounting gives your users these routes under `/billing`:

**Shopping and checkout** (always mounted; no sign-in)

| Route | What it does |
|---|---|
| `GET /billing/v1/products` | a merchant's products on sale named by key, each with its current prices (`?merchant=<merchant id>&keys=`) |
| `GET`, `POST /billing/v1/orders/{id}/solana-pay` | the Solana Pay request a wallet signs for an order (when a Solana PSP is declared) |
| `GET /billing/v1/config` | which route groups and features are mounted, each currency's decimal places, the rails a PSP can be declared on, and the captcha to solve when asked (always mounted) |

**Your customers' own billing** (always mounted; signed in, always as the caller)

| Route | What it does |
|---|---|
| `POST /billing/v1/me/orders/preview`, `POST /billing/v1/me/orders` | price lines; buy them (with a `payment`, in one call) |
| `GET /billing/v1/me/orders`, `GET /billing/v1/me/orders/{id}` | their orders, subscription renewals and usage billed in arrears included (`?origin=`, `?status=`, `?subscription_id=`, `?overdue=true`) |
| `POST /billing/v1/me/orders/{id}/pay`, `/confirm`, `/cancel` | pay an open order (a renewal or usage order with a saved card), finish the bank's challenge, cancel a purchase |
| `GET`, `DELETE /billing/v1/me/cart` | their cart, priced now with the ways it can be paid; empty it |
| `PUT /billing/v1/me/cart/lines/{price_id}` | add a price or change its line (quantity 0 removes it); the cart is shared by every site of the merchant |
| `GET /billing/v1/me/config` | the merchant's name and the payment methods a buyer can use, with their browser config |
| `GET /billing/v1/me/entitlements` | everything they currently have access to |
| `GET /billing/v1/me/subscriptions` | their subscriptions |
| `GET /billing/v1/me/subscriptions/{id}` | one subscription |
| `POST /billing/v1/me/subscriptions/{id}/cancel` | cancel at period end (with a reason) |
| `POST /billing/v1/me/subscriptions/{id}/resume` | undo a cancellation before the period ends |
| `POST /billing/v1/me/subscriptions/{id}/change` | change plan or seats |
| `POST /billing/v1/me/subscriptions/{id}/change/preview` | what that change would cost |
| `POST /billing/v1/me/subscriptions/{id}/confirm` | finish a change after the bank's 3-D Secure step |
| `PUT /billing/v1/me/subscriptions/{id}/payment-method` | pay a subscription with another saved card, or with their default again |
| `POST /billing/v1/me/subscriptions/{id}/retry-now` | retry a failed renewal the PSP schedules (an engine-owned renewal is an order, paid with `/orders/{id}/pay`) |
| `GET /billing/v1/me/payment-methods` | their saved cards, newest first |
| `POST /billing/v1/me/payment-methods` | save a card in one call (the PSP's token, never the card number) |
| `PUT`, `DELETE /billing/v1/me/payment-methods/{id}` | replace or remove a card |
| `PATCH /billing/v1/me/payment-methods/{id}` | change a card's expiry, billing details or reuse |
| `POST /billing/v1/me/payment-methods/{id}/confirm` | finish saving a card after the bank's 3-D Secure step |
| `PUT /billing/v1/me/default-payment-methods/{currency}` | choose the card that pays one currency: its usage orders and every subscription without its own card |
| `POST /billing/v1/me/stripe/billing-portal-sessions` | open Stripe's billing portal (when a Stripe PSP is declared) |
| `GET /billing/v1/me/payments` | payment and refund history |
| `GET /billing/v1/me` | balance per currency, the card that pays each currency, and unread notices |
| `GET /billing/v1/me/balance/transactions` | its balance transactions |
| `GET /billing/v1/me/usage` | metered usage |
| `GET /billing/v1/me/notifications` | billing notices ("your card was declined") |
| `POST /billing/v1/me/notifications/read` | mark up to 100 read, or all |

**Webhooks** (always mounted; payment processors, verified by signature)

| Route | What it does |
|---|---|
| `POST /billing/v1/webhooks/{rail}/{account_id}` | processor notifications (Stripe, NMI, CCBill), verified per account |

**Programmatic** (with `RouteGroups.Programmatic`; your backend, never a person)

Your backend calls these with its own credential: a client-credentials token
from your issuer (AuthKit issues them) for this deployment, holding the route's
permission: `root:entitlements:read` for the content gate, `root:usage:manage`
for usage events, admissions and running charges, `root:events:read` for host
events, `root:orders:manage` for orders. A person is refused.
Writes use their durable operation identity (request ID, usage source, running-charge
reference, or an order's `Idempotency-Key`), so a retry never double-counts. Embedded, call
the same `Client` methods in process instead. The `scim/v2` routes also take a
merchant provisioning token, for directories like Okta or Entra ID that can't
get a token from your issuer; that token opens nothing else.

| Route | What it does |
|---|---|
| `POST /billing/v1/app/entitlements/check` | which entitlements a user holds now, up to 100 keys and 10 prefixes at once: gate your content with it (a read; no `Idempotency-Key`) |
| `POST /billing/v1/app/usage-events` | record metered usage, failed work included |
| `POST /billing/v1/app/admissions` | admit requests against a customer's budget, holding funds |
| `POST /billing/v1/app/admissions/capture` | settle them at what the work used, priced by the rate cards |
| `POST /billing/v1/app/admissions/release`, `/extend` | free holds; move their deadlines |
| `POST /billing/v1/app/running-charges` | charge a customer at a rate per hour until you stop it |
| `POST /billing/v1/app/running-charges/{reference}/stop` | stop it and post what it charged |
| `GET /billing/v1/app/host-events` | events for your backend to process, oldest first |
| `POST /billing/v1/app/host-events/acknowledge` | mark them processed |
| `POST /billing/v1/app/orders/preview` | an order's total and how it can be paid, for one of your users; changes nothing |
| `POST /billing/v1/app/orders` | an unpaid order for one of your users, which they pay on your site; it never charges |
| `POST /billing/v1/app/orders/{id}/cancel` | withdraw an unpaid order you created |
| `POST /billing/v1/app/scim/v2/Bulk` | your directory's batch of user changes (AuthKit sends one every 5 minutes) |
| `POST`, `GET /billing/v1/app/scim/v2/Users`; `GET`, `PUT`, `PATCH`, `DELETE /billing/v1/app/scim/v2/Users/{id}` | one user at a time, for directories that don't batch (Okta, Entra ID) |
| `GET /billing/v1/app/scim/v2/ServiceProviderConfig`, `/ResourceTypes`, `/Schemas` | what OpenRails accepts, including the batch limits |

The SCIM routes are not mounted with `Deps.UserInfo`, which asks your AuthKit
directly: one source of truth. See [customer contacts](docs/customer-contacts.md).
To read what it needs beyond these (a customer, their entitlement lists), give
your backend's application `root:customer:read` too: customer support's reads take an
application as readily as a person.

The staff route groups (`RouteGroups.Admin`, `Catalog`, `MerchantConfig`,
`Metrics`) publish `/billing/v1/admin/*` for customer support, `/billing/v1/metrics/*`
for metrics,
`/billing/v1/catalog/*` for catalog management and `/billing/v1/merchant/*`
for merchant configuration, for staff and machines rather than
your users, each route behind the permission of its group, and each has one
method on the Go `Client`. Every route and its permission are in the [route
table](docs/api/routes.md); the conventions are in the [API
guide](docs/api/endpoints.md).

What you will set next:

| To | Set |
|---|---|
| Send billing email (receipts, failed-payment notices) | `Config.SMTP` (`Host`, `Port`, `Username`, `Password`, `From`: any SMTP server) or your own `Deps.Email`, plus where addresses come from: `Deps.UserInfo` (embedded) or SCIM provisioning (standalone) |
| Turn on the staff and programmatic routes | `Routes.RouteGroups` (admin, catalog, merchant config, metrics, programmatic), and grant `billing.Permissions()` in your roles |
| Serve the admin console | `Routes.AdminConsole` ([admin dashboard](#admin-dashboard)) |
| Share one billing schema between two apps | Connect both as one role, or `SET ROLE` to a shared one on every connection: the role `New` runs as owns every object |
| Change or switch off the built-in limits on checkout and card writes | `Config.RateLimits`, `Config.RateLimitsDisabled` ([rate limiting](docs/rate-limiting.md)) |
| Report readiness | `bill.Ready(ctx)` and `bill.Probes()` in your own health handler |

### How access and billing work


**Access and ownership.** Use `ListEntitlements` to check whether the customer may
access content now. That includes individual purchases, bundles and unexpired
rentals. `ListProductAccess` identifies the product actually acquired and its
expiry: a bundle purchase records the bundle product, while its entitlement
grants unlock the constituent courses. Current access alone does not prove a
permanent purchase. Refunds and revocations can withdraw granted access.

**Billing and access.** `billing_interval` sets the time between payments. A
positive interval recurs until the customer cancels; `null` means a one-time
payment. There is no separate renewal switch. `access_duration` sets the access
granted by each payment; `null` means no scheduled expiry. These fields are
independent: a price may charge every 30 days and grant 45 days of access per
payment. A one-time annual pass uses `billing_interval: null` with
`access_duration: 365 days`. Day counts are fixed durations, not calendar months
or years. Zero and negative durations are invalid.

On creation, omitted billing and access durations default to no recurrence and
no scheduled expiry. In a partial catalog update, omitted fields preserve the
existing terms; explicit `null` clears that duration. Changed terms create a new
immutable price revision. Cancellation stops future payments and preserves
access already purchased; refunds and explicit revocations may withdraw it.
Dunning access follows the separate grace policy and does not extend paid time.

Product keys are merchant-wide; price keys belong to their product. For example,
`channel-membership.monthly` selects the 30-day membership price.
OpenRails assigns immutable price revisions automatically, starting at v0.
Existing subscribers keep their accepted price and benefits until explicitly
migrated. Omitted entries stay unchanged; use `archived: true` to retire a price.

### Evolving the catalog

The following files form a catalog migration: update products in place, create
new price revisions, retire prices, and restore an earlier revision. All changes
are YAML applications. Apply each file through the same
`catalog.ReadFile` → `MerchantDeclaration.Catalog` startup path shown above, through
`client.ApplyCatalog`, or through `openrails apply-catalog --merchant onlydemo --file FILE.yaml`.
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

**Replay is not rollback.** The canonical content hash of each document that
applied whole is remembered per merchant. Reapplying any of these files changes
nothing, even after later edits. Reapplying the initial `catalog.yaml` therefore
does not undo the price increase or restore an archived bundle. The same rule
applies to already used archive and restore files. Comments, formatting, and
product/price ordering do not make a new application. Different, previously
unseen documents have no ordering guarantee, so apply intended changes in order.

**A file and the console share the catalog.** OpenRails records which manager
last set each field: the file (`MerchantDeclaration.Catalog`, `ApplyCatalog`, the
applications route, `openrails apply-catalog`) or an edit (the console, the
product, price and meter routes, `UpdateProduct` and the other edit methods).
A product, a price key and a meter each apply whole. When a document names a
field whose live value an edit set differently, that object is skipped and
reported, and the rest of the document applies:

| Step | Result |
|---|---|
| The file sets prices `a` $1 and `b` $2 | Both apply; the file owns them |
| Staff set `a` to $1.50 | The edit always succeeds and takes `a`'s amount |
| The file changes `b` to $2.50 | `b` applies; `a` stays $1.50, listed in the receipt's `conflicts` |

A document with conflicts is not recorded as applied, so the next start, or the
next apply, retries it; what already applied is a no-op. One finding,
`life.catalog.conflicts`, names the skipped objects until a document applies
whole. To resolve one, change the file to agree (equal values are shared, never
a conflict), drop the field or entry from the file (the live value stays), or
apply with `ApplyCatalogParams{Force: true}`, `?force=true` or
`openrails apply-catalog --force-conflicts`, which overwrites the edit and takes
the field. `MerchantDeclaration.Catalog` never forces, and a conflict never fails `New`.

All these examples preserve omitted entries. `prune: true` makes a document the
whole catalog: an omitted product or price that no edit set is deleted when
nothing ever referred to it and archived otherwise; one an edit set, the file
lets go. A purchased product is never deleted.
Changing existing subscribers' accepted prices requires an explicit scheduled
reprice operation. A product's keys are not part of the accepted terms:
editing a product's `entitlements` changes what every buyer and subscriber of
it holds, at once (see [Entitlements follow the product](docs/entitlements_timeline.md)).

### Backing up and restoring a complete catalog

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
openrails apply-catalog --merchant onlydemo --file catalog.yaml
```

That document can recreate equivalent prices, using the destination's product and
price identities. It does not preserve archived history or original IDs. Provider
references belong to their configured PSP accounts and must be reviewed before
copying between merchants. Reapplying an already applied document is a permanent
hash replay, so this workflow does not roll back later edits.

### Turning the catalog routes on and off

The catalog over HTTP is its own route group: turn on `RouteGroups.Catalog`.
Its callers hold `root:catalog:manage`:

```go
err = openrailsgin.Mount(r, bill, openrails.Routes{
	Auth:        ak.Authenticator(),
	Prefix:      "/billing",
	RouteGroups: openrails.RouteGroups{Catalog: true},
})
```

| Mount | The catalog over HTTP | In-process `client.ApplyCatalog`, `CreateProduct`, `CreatePrice`, etc. |
|---|---|---|
| `RouteGroups.Catalog` off | Not mounted | Available |
| on | Readable and changeable by staff holding `root:catalog:manage`; with a declared merchant's `Catalog`, the file skips what an edit changed | Available |

Customers and your site read the live catalog at
`/v1/products?merchant=<merchant id>` either way. Turning HTTP changes off does not make the database read-only or prevent
later client edits. The startup example above applies `catalog.yaml` through
`MerchantDeclaration.Catalog` on every boot; with the group on, staff edits and the file
share the catalog as above. The standalone server mounts them with
`route_groups.catalog`.

Every product, price key and meter carries a `revision` that
advances on each change. An edit may send the revision it read as
`expected_revision`; if the object changed since, it is refused with
`409 revision_mismatch` and `metadata.revision`. The console always sends it.

---

### Per-seat prices

A recurring price is sold per seat when it declares `quantity`: its amount is
one seat's, a subscription holds `min` to `max` seats, and renewals bill each.
More seats charge now for the rest of the period; fewer take effect at the next
renewal (`POST /billing/v1/me/subscriptions/{id}/change`). Seats are billed only
by OpenRails on NMI or Stripe; any other price has no quantity.

```yaml
products:
  team:
    display_name: Team
    entitlements: [team]
    prices:
      monthly:
        amount: 10 USD
        billing_interval: 30 days
        quantity: {min: 1, max: 50}
```

---

### Prepaid API-balance catalog

An API service can apply this separate catalog through the same
`catalog.ReadFile` and `MerchantDeclaration.Catalog` flow. These products top
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
        amount: 0 USD # customer_amount requires an explicit checkout amount; this is not free
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
prevent repeat purchases. The amount and expiry policy are frozen at checkout;
expiry starts when payment succeeds. Omitted expiry means 365 days. Later catalog
changes affect new checkouts, preserving existing lots.

The deposit is also a product: it names what is bought, its currency, limits, and
expiry policy. The amount is chosen for each order without creating a new
price revision. For a $100 deposit, the customer's browser orders the line with
`"amount": "100000000"` on `POST /billing/v1/me/orders`. With the merchant's
credential, the Go client creates the order instead:

```go
amount := int64(100_000_000) // $100 in USD micros
order, err := client.CreateOrder(ctx, billing.CreateOrderParams{
    CustomerID:     customerID,
    Lines:          []billing.OrderLineParams{{PriceID: depositPriceID, Amount: &amount}},
    IdempotencyKey: key,
})
```

Only that customer pays it, signed in. Its amount is frozen when the order is
created. Fixed-price packs reject an amount override. Customer-selected deposits
currently support Stripe and NMI checkout; recurring credit benefits are not yet
supported. These are monetary API balances, consumed by priced usage, rather than
a separate request-count or token-count wallet. A voluntary refund requires the
corresponding credits to remain unused and unheld; forced reversals use the source
lot and preserve the existing rules for already-authorized usage.

#### Updating prepaid prices

Credit benefits evolve through the same partial updates. For example, this changes
only the expiry offered by new $100-pack checkouts. Existing credit lots keep the
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

### How It Works

Operator side (you, the merchant):
- Declare your merchant and payment-processor accounts (in `Config`, a YAML manifest, or the API).
- Declare your catalog: products, the entitlements they grant, and prices. OpenRails charges its own price terms; a processor-side copy is linked only where a rail needs one.
- Call one API for admissions, credits, entitlement checks, subscriptions and orders: the Go `Client` (in-process or over HTTP), or plain HTTP from any stack.

Customer side (your users):
- The customer buys with their own credential: their browser creates an order (`POST /v1/me/orders`, billing-ui) and pays it.
- Apps with purchase rules of their own can create the order with the merchant's credential instead: `Client.CreateOrder`, in process or remote with an application key holding `root:orders:manage` (or `merchant:orders:manage` at the merchant); only the customer pays it, signed in.
- Your frontend calls `/v1/me/*` self-service routes with a short-lived token.
- Processor webhooks land on OpenRails; it updates entitlements in your database and your app reads them.

---

### Checkout catalog references

Use a stable product/price key pair such as `course-101.purchase`. `ApplyCatalog` changes its terms by
creating an immutable price version and retiring its predecessor. Checkout owns
current availability, amount, permanent ownership eligibility and payment
idempotency; a host wrapper supplies verified identity and its content policy.

| Operation | Reference contract |
| --- | --- |
| `CreateOrder`, `PreviewOrder` | Each line's `PriceID` |
| `ListEntitlements` | Keys derived from the products customers hold: up to 100 customers and 100 keys, or one prefix, in one page; without customers, one key's holders |
| `ListProductAccess` | Customer and product IDs; archived purchase access remains readable |
| `CreatePrice` | Exactly one existing `ProductID`, `ProductKey`, or inline `ProductData` |
| `PreviewPSPRouting` | Exactly one `price_id` or `price_key` |
| Catalog reads | `GetProduct` / `GetPrice` (ID), or `ListProducts` by `Keys` and `ListPrices` by `ProductKey` + `Key` |
| Tier changes, accepted attempts, payments, subscriptions and imports | Immutable IDs |

Keys are opaque, including UUID-shaped keys: a field that takes an ID never
accepts a key, and there is no auto-detection. An accepted attempt keeps its
original price and benefit snapshot after a key moves or a price is archived.
More in [products and prices through the Client](docs/catalog-client.md).

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
- **Browser-direct self-service** — end-users hit `/v1/me/*` (balance, orders,
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
  key-metric widgets.

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
exceeds their balance. OpenRails' admission API (hold → capture/release, spend caps)
exists precisely for that pre-authorization gap, and our
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
- [Frontend integration](docs/frontend-integration.md) — the browser side: self-service routes, orders, payment methods, tokens, and error handling.
- [`@openrails/billing-ui`](sdk/billing-ui/README.md) — the embeddable checkout and account-billing React UI, on npm.
- [The auth model](docs/auth.md) — one credential per trust domain: why embedded uses your session credential and standalone uses trusted issuers' access tokens.
- [Customer contacts](docs/customer-contacts.md) — where customers' emails and names come from: your AuthKit in process, or your directory's SCIM 2.0 pushes.
- [Products and prices through the Client](docs/catalog-client.md).
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
- Usage billing — [request admission](docs/admission-operations.md), [billing policies](docs/billing-policies.md), [arrears and delinquency](docs/arrears-delinquency.md), [usage orders](docs/usage-orders.md), [running charges](docs/running-charges.md).

**Operate the deployment** — for whoever keeps it running:

- [Operator guide](docs/operator-guide.md) — infrastructure requirements (Postgres, Redis/Garnet, Vault), what runs by itself, and the drift toolbox.
- [Operations manual](docs/operations.md) — the deep reference: operating modes and safety levers, dunning, reconciliation, the provider intent ledger, data retention, cutovers.
- [Merchant provisioning](docs/merchant-provisioning.md) — manifests, credentials, secrets, and API keys; [merchant names](docs/merchant-name-authority.md); [settings](docs/api/merchant-settings.md) and [configuration](docs/merchant-configuration.md).
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


## License

OpenRails is under the Functional Source License, Version 1.1, ALv2 Future License ([LICENSE](LICENSE)). The browser SDK `@openrails/billing-ui` ([`sdk/billing-ui`](sdk/billing-ui)) is MIT ([sdk/billing-ui/LICENSE](sdk/billing-ui/LICENSE)), so you can ship it in your own frontend under any license.
