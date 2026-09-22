# Merchant selection on the portable Client

One Client can make concurrent calls for multiple merchants. An operation is a
normal synchronous Go call, with an optional merchant selector at the end:

```go
product, err := client.Products.Create(ctx, params, openrails.WithMerchant("alpha"))
price, err := client.Prices.Retrieve(ctx, priceID, openrails.WithMerchant("bravo"))
```

Use `ForMerchantID(id)` when the caller already stores the stable merchant UUID.
Slugs are lookup names; the server resolves them before authorization and never
uses them as durable identities in tokens, jobs or foreign keys. A call accepts
one selector, not both slug and UUID or several competing options.

`WithDefaultMerchant("alpha")` is a Client constructor option for a host that
usually calls one merchant. Existing `WithMerchantID(id)` supplies a UUID default.
An explicit per-call option overrides either default without mutating the Client.
These defaults do not restrict the runtime's capability or grant authority. An
operation without a selector or default returns an invalid-request error before
minting a credential. An empty explicit selector does not fall back to a default.

## Credentials are independent

`WithAPIKey` uses one credential. A key for alpha cannot access bravo just because
the call selects bravo. A cross-merchant operator must have the current permission
for the exact target and operation. Trusted embedded host capability remains
distinct from an authenticated application's user request.

Use `WithCredentialProvider` when a Bearer token must be minted for the requested
merchant. The callback receives `CredentialTarget` explicitly and must be safe for
concurrent calls. Its target is requested data, not proof of merchant existence or
authority. A failed mint returns an error without falling back to another token:

```go
client, err := openrails.NewRemote(baseURL,
    openrails.WithCredentialProvider(func(ctx context.Context, target openrails.CredentialTarget) (string, error) {
        return credentials.TokenFor(ctx, target)
    }))
```

This is the existing Bearer/API-key path, not a conversion of DPoP or other
sender-constrained credentials into an unrestricted Bearer. The server preserves
the original credential's scope and proof requirements. Native permissions remain
live; client selection never assigns a role or broadens a credential ceiling.

The SDK emits exactly one target header: `X-OpenRails-Merchant-Slug` or
`X-OpenRails-Merchant-ID`. Ambient merchant context values cannot supply a missing
selector. An ambient ID must match an explicit/default ID; combining it with a
slug is refused because the SDK cannot resolve that equality locally.

Slug-selected operations use the distinct `/v2/merchant`, `/v2/catalog`, `/v2/me`
and `/v2/import` operation paths. A pre-selector server must refuse these paths
before any mutation; it must not ignore an unknown header and execute against the
credential's merchant. There is no retry or fallback to v1. UUID-selected calls
retain v1's already enforced binding. The actual request path is chosen before
request construction/signing and is preserved through server verification.

## Operation classification and migration

The existing billing, catalog, customer, product-access, import and archive SDK
methods are merchant-scoped. The existing `Verify` method is also scoped: it reads
`/v1/merchant/settings` with settings-read authority, and is not a global health
probe. Global health/info and control-plane discovery are currently separate
server surfaces. Future global Client operations must use explicit platform scope
for credential minting; an omitted merchant never means platform authority.

All scoped methods, including reads, deletes and archive streams, accept the same
`...openrails.RequestOption` tail. Direct calls without options remain syntactically
valid when the Client has a default. Go interfaces and method-expression types
must include the new variadic parameter; update narrow host interfaces when
adopting this API. The same Client implementation and options are used for local
in-process and remote HTTP transports.

Catalog owner and customer selectors remain independent from merchant selection.
In SaaS platform billing, the hosted merchant may be the payer while the platform
is the selling merchant; never substitute one for the other. Merchant declaration
and provisioning remain a separate explicit bootstrap concern, not a side effect
of selecting a merchant for a read.
