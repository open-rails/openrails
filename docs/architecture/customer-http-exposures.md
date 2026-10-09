# Hosted customer audiences

Configure the engine once, then mount the routes an `openrails.Routes` selects
with a framework adapter. With `Config.ControlPlane`, `Client.Routes` is the standalone surface:
identity, merchant, platform, customer, callback and enabled console
registrations. Process health endpoints remain host-owned. Provider callbacks
are addressed by account (`/v1/webhooks/{rail}/{account_id}`) in every posture.

A host can additionally expose a customer audience with its own authenticator.
`Routes.CustomerProfiles` marks the profile `Delegated`; the authenticator
itself is `Deps.AuthenticateCustomer`, which receives the profile's `Prefix`:

```go
deps.AuthenticateCustomer = func(r *http.Request, profile string) (*openrails.DelegatedPrincipal, error) {
    if profile == "/billing/v1/me" {
        return portalIdentity(r)
    }
    return platformCustomerIdentity(r)
}
client, err := openrails.New(ctx, cfg, deps)
if err != nil { return err }
err = openrailsgin.Mount(router, client, openrails.Routes{ // on the host router
    CustomerProfiles: []openrails.CustomerRoutes{
        {Prefix: "/billing/v1/me", Scope: openrails.CustomerSelfService, Delegated: true},
        {
            Prefix:    "/api/v1/merchants/{slug}/billing/me",
            Scope:     openrails.CustomerSubscriptionManagement,
            Delegated: true,
        },
    },
})
```

`CustomerSelfService` is the full library self-service surface. The
subscription-management scope exposes exactly cancellation, resumption,
subscription payment-method changes and invoice collection-method selection.
Both profiles reuse the same route registration and customer ownership checks.
Neither exposes merchant administration, credentials or callbacks.

An ordinary native audience uses `Deps.Authenticate` and the declared merchant. A `Delegated` audience is authenticated by `Deps.AuthenticateCustomer`. The authenticator verifies the
actual credential and derives its merchant and customer from trusted host policy;
a URL parameter or request-body merchant field is not authority. Route parameters
are available through `Request.PathValue` before authentication. Original URL,
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

For the SaaS platform-billing audience, `{slug}` selects the hosted merchant
customer, not the billing merchant. Its verifier/resolver must check current hosted
merchant ownership, return that captured hosted merchant UUID as the canonical
customer, and bind the principal to the PLATFORM billing merchant. A stale owner or
an unrelated hosted merchant must fail before any of the four mutations.

