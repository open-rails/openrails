# Hosted customer audiences

Configure the engine once, then mount the routes an `openrails.Routes` selects
with a framework adapter. A standalone server's `Routes` is the standalone
surface: identity, merchant, platform, customer, callback and enabled console
registrations, plus the customer profiles it is given. Process health
endpoints remain host-owned. Provider callbacks
are addressed by account (`/v1/webhooks/{rail}/{account_id}`) in every posture.

A host can additionally expose a customer audience with its own `Auth`, set on
its `Routes.CustomerProfiles` entry; profiles without one use `Routes.Auth`:

```go
client, err := openrails.New(ctx, cfg, openrails.Deps{Postgres: pool})
if err != nil { return err }
err = openrailsgin.Mount(router, client, openrails.Routes{ // on the host router
    Auth: portalAuth,
    CustomerProfiles: []openrails.CustomerRoutes{
        {Prefix: "/billing/v1/me", Scope: openrails.CustomerSelfService},
        {
            Prefix: "/api/v1/merchants/{slug}/billing/me",
            Scope:  openrails.CustomerSubscriptionManagement,
            Auth:   platformAuth,
        },
    },
})
```

`CustomerSelfService` is the full library self-service surface. The
billing-management scope adds billing history, purchased access, saved
methods, payment recovery and paying a checkout session the merchant minted,
but starts no checkout and changes no plan. The
subscription-management scope exposes exactly cancellation, resumption,
subscription payment-method changes and invoice collection-method selection.
Both profiles reuse the same route registration and customer ownership checks.
Neither exposes merchant administration, credentials or callbacks.

Each audience serves `Config.Merchant`, or the merchant its `Merchant` slug
names. On a standalone server (`Server.Routes`), an audience without a
`Merchant` serves the merchant each request selects: the `OpenRails-Merchant`
header or the merchant's API host. A request that selects none is refused
`merchant_unresolved`, and an unknown or deleted merchant `merchant_not_found`.
OpenRails binds the merchant before the audience's `Auth` runs, and the `Auth`
reads it with `openrails.RequestMerchant` to check the user is that merchant's
customer; it does not resolve the header itself. The customer is the
identity's subject at that merchant, so selecting another merchant never
reaches another subject's billing.

Its `Auth` verifies the actual credential; the identity's subject is the
customer, and a URL parameter or request-body field is never authority. Route
parameters are available through `Request.PathValue` before authentication. Original URL,
RawPath, RequestURI and body remain available for signature and sender-proof
checks. Mounting copies the selection; conflicting method/path patterns fail before router mutation.
Prefixes accept literal segments and whole-segment `{name}` parameters; native
router wildcard characters `*` and `+` are rejected. Routes sharing a parameter
position must use the same name, including when their paths diverge afterward,
so every supported native router can register the bundle.
A mounted admin console owns the GET subtree at its path (`/admin/` by
default), so routes beneath it are rejected before native registration.

Every adapter mounts on the application's root router: Gin's root Engine,
Fiber's root App, a root ServeMux or Chi Mux. `Routes.Prefix` places the
embedded API (`/billing` serves `/billing/v1/*`); the admin console sits at its
own path, and standalone bundles, with their issuer-anchored AuthKit and
console URLs, sit at the root.

A checkout session's id alone pays it with a new card; a saved card needs
its customer's proof. On a customer surface the session's customer reads and
pays it at `{prefix}/checkout-sessions/{id}` and `{prefix}/checkout-sessions/{id}/pay`
(beside `{prefix}/checkout-sessions`, which mints one), and the surface's
`Auth` is that proof. The session must be the surface's merchant's (its
`Merchant`, or the one the request selected) and the identity's subject its
customer; any other session is `checkout_session_not_found`, and only the
customer acting in person pays (`customer_action_required`). billing-ui's
`client.checkoutSource(id, { customerBase: prefix })` reads and pays there;
without `customerBase` it uses the session id alone at
`/v1/checkout-sessions/{id}`.

For the SaaS platform-billing audience, `{slug}` selects the hosted merchant
customer, not the billing merchant. Its `Auth` must check current hosted
merchant ownership and return that hosted merchant's UUID as the subject, on a
profile bound to the platform billing merchant. A stale owner or an unrelated
hosted merchant must fail before any of the four mutations.

