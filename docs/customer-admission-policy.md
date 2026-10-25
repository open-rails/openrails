# Host policy before ordinary customer admission

A standalone server accepts `server.Deps.CustomerAdmissionPolicy`. It receives
the original verified credential and the resolved merchant/customer pair after
ordinary identity, binding and self-scope checks, before `db.AdmitCustomer` can
create the customer. Return an AuthKit refusal (`auth.ErrForbidden`, expired,
revoked, unavailable) to deny the request using the normal HTTP error mapping.
An unknown error fails closed. A nil hook preserves ordinary OSS behavior.

The callback applies to automatic `/v1/me` admission, including reads for an
already existing customer. It does not run on staff or AuthKit routes, a host's
`Engine.CustomerRoutes` mapping, or a pay URL bound to an immutable order.
The host authorizes its mapped relationships separately. It must not reconstruct
a verified identity from browser-supplied issuer, account or customer fields.

## Explicit hosted OAuth configuration

Declaring an agreement no longer installs platform scopes, OAuth clients or
consent policy. A host that offers merchant client sign-in explicitly declares
the resource and group-client scopes and any required agreements. In particular,
replace the removed implicit `network-terms` activation with configuration like:

```go
cfg.Auth.Resource.Scopes = map[string][]string{
    "openrails:link": {}, // host consent; grants no billing permission
}
cfg.Auth.AuthorizationServer.Resources = []authkit.ResourceServerConfig{{
    ID: apiOrigin,
    Scopes: []string{"openrails:self", "openrails:link"},
}}
cfg.Auth.AuthorizationServer.GroupClients = authkit.GroupClientsConfig{
    Scopes: []authkit.GroupClientScope{
        {Name: "openrails:self", Resource: apiOrigin, Description: "Use your billing account at this merchant."},
        {Name: "openrails:link", Resource: apiOrigin, Description: "Link your merchant account to your platform account."},
    },
    Agreements: []string{"network-terms"},
}
```

Merge these entries with other host resource/client settings; do not replace
unrelated configured scopes or clients. The server preserves host resource
scopes but owns the fixed `openrails:self` and `openrails:merchant` permission
ceilings. A host override cannot turn the self scope into administration.
The host owns any first-party shared-checkout OAuth client and its redirect
origins; the generic payment page has no platform sign-in return endpoint.

The extraction tests exercise real AuthKit authorization-code consent, a
host-defined scope, canonical self-scope isolation from staff operations,
pre-admission refusal without a new billing customer, and mapped-route access.
The existing shipping product scenarios must move to the SaaS suite before the
full removal candidate is merged; a narrow local test is not that full gate.
