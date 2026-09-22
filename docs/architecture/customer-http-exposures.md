# Hosted customer audiences

Configure the runtime once, then mount one framework-native route bundle. Hosts
with an attached OpenRails control plane select `HTTPConfig.Standalone`; its
identity, merchant, platform, customer, callback and enabled console registrations
are reused. Process health endpoints remain host-owned. Hosted control-plane
posture selects Host-bound provider callbacks; private standalone posture retains
provider-account routing.

A host can additionally expose a customer audience with its own authenticator:

```go
runtime.ConfigureHTTP(embed.HTTPConfig{
    Standalone: true,
    CustomerExposures: []embed.CustomerHTTPConfig{
        {Prefix: "/billing/v1/me", DelegatedAuthenticator: portalIdentity},
        {
            Prefix: "/api/v1/merchants/{slug}/billing/me",
            Scope: embed.CustomerSubscriptionManagement,
            DelegatedAuthenticator: platformCustomerIdentity,
        },
    },
})
routes, err := openrailsgin.Routes(runtime)
// Handle err, then mount once on the host router.
err = routes.Mount(router)
```

The default customer scope uses the full library self-service surface. The
subscription-management scope exposes exactly cancellation, resumption,
subscription payment-method changes and invoice collection-method selection.
Both profiles reuse the same route registration and payer ownership checks.
Neither exposes merchant administration, credentials, treasury or callbacks.

Each audience must supply its own authenticator. The authenticator verifies the
actual credential and derives its merchant and payer from trusted host policy;
a URL parameter or request-body merchant field is not authority. Route parameters
are available through `Request.PathValue` before authentication. Original URL,
RawPath, RequestURI and body remain available for signature and sender-proof
checks. Runtime configuration copies exposure declarations and freezes before
mounting; conflicting method/path patterns fail before router mutation.
Prefixes accept literal segments and whole-segment `{name}` parameters; native
router wildcard characters `*` and `+` are rejected. Routes sharing a parameter
position must use the same name, including when their paths diverge afterward,
so every supported native router can register the bundle.
An enabled standalone console owns the `/admin/` GET subtree, so full customer
audiences beneath it are rejected before native registration.

Standalone bundles contain issuer-anchored AuthKit and console URLs and therefore
mount at the application root. Gin requires the root Engine; Fiber requires the
root App. The net/http adapter accepts a root ServeMux without a prefix. For Chi,
use `routes.MountRoot(rootRouter)`: this is an explicit caller assertion, because
Chi's public router API cannot distinguish a root Mux from a nested subrouter.
Group mounts are refused before registration. Embedded-only bundles remain
mountable under arbitrary host prefixes.
