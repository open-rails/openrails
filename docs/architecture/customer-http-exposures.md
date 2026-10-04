# Hosted customer audiences

Configure the engine once, then mount its routes once with a framework
adapter. With `Config.ControlPlane`, `Client.Routes` is the standalone surface:
identity, merchant, platform, customer, callback and enabled console
registrations. Process health endpoints remain host-owned. Hosted control-plane
posture selects Host-bound provider callbacks; private standalone posture retains
provider-account routing.

A host can additionally expose a customer audience with its own authenticator.
`Config` marks the profile `Delegated`; the authenticator itself is
`Deps.AuthenticateCustomer`, which receives the profile's `Prefix`:

```go
cfg.HTTP = &openrails.HTTPConfig{
    CustomerRoutes: []openrails.CustomerRoutesConfig{
        {Prefix: "/billing/v1/me", Delegated: true},
        {
            Prefix:    "/api/v1/merchants/{slug}/billing/me",
            Scope:     openrails.CustomerSubscriptionManagement,
            Delegated: true,
        },
    },
}
deps.AuthenticateCustomer = func(r *http.Request, profile string) (*openrails.DelegatedPrincipal, error) {
    if profile == "/billing/v1/me" {
        return portalIdentity(r)
    }
    return platformCustomerIdentity(r)
}
client, err := openrails.New(ctx, cfg, deps)
if err != nil { return err }
err = openrailsgin.Mount(router, client) // once, on the host router
```

The default customer scope uses the full library self-service surface. The
subscription-management scope exposes exactly cancellation, resumption,
subscription payment-method changes and invoice collection-method selection.
Both profiles reuse the same route registration and payer ownership checks.
Neither exposes merchant administration, credentials or callbacks.

An ordinary native audience uses `Deps.Authenticate` and the declared merchant. A `Delegated` audience is authenticated by `Deps.AuthenticateCustomer`. The authenticator verifies the
actual credential and derives its merchant and payer from trusted host policy;
a URL parameter or request-body merchant field is not authority. Route parameters
are available through `Request.PathValue` before authentication. Original URL,
RawPath, RequestURI and body remain available for signature and sender-proof
checks. Construction copies exposure declarations; conflicting method/path patterns fail before router mutation.
Prefixes accept literal segments and whole-segment `{name}` parameters; native
router wildcard characters `*` and `+` are rejected. Routes sharing a parameter
position must use the same name, including when their paths diverge afterward,
so every supported native router can register the bundle.
An enabled standalone console owns the GET subtree at `admin_console.path`
(`/admin/` by default), so full customer audiences beneath it are rejected
before native registration.

Standalone bundles contain issuer-anchored AuthKit and console URLs and therefore
mount at the application root. Gin requires the root Engine; Fiber requires the
root App. The net/http adapter accepts a root ServeMux without a prefix. For Chi,
use `openrailshttp.MountRoot(rootRouter, client)`: this is an explicit caller assertion, because
Chi's public router API cannot distinguish a root Mux from a nested subrouter.
Group mounts are refused before registration. Embedded-only bundles remain
mountable under arbitrary host prefixes.

For the SaaS platform-billing audience, `{slug}` selects the hosted merchant
payer, not the billing merchant. Its verifier/resolver must check current hosted
merchant ownership, return that captured hosted merchant UUID as the canonical
payer, and bind the principal to the PLATFORM billing merchant. A stale owner or
an unrelated hosted merchant must fail before any of the four mutations.

