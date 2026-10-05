# Choosing the merchant a Client call acts on

A Client call acts on one merchant. Which one is decided in this order, and
none of it grants authority: the credential must hold the operation's
permission on that merchant.

1. **A per-call selector**, the last argument of every merchant method:

   ```go
   product, err := client.CreateProduct(ctx, params, openrails.WithMerchant("alpha"))
   price, err := client.GetPrice(ctx, priceID, billing.GetPriceParams{}, openrails.ForMerchantID(id))
   ```

   `WithMerchant` always means a slug, even a UUID-shaped one; `ForMerchantID`
   always means the stable UUID. Neither falls back to the other. A call takes
   one selector.
2. **The Client's default**: `openrails.WithDefaultMerchant("alpha")` or
   `openrails.WithMerchantID(id)` at construction, or on a client derived with
   `client.With(...)`. A per-call selector overrides it without changing the
   Client.
3. **The engine's declared merchant** (`Config.Merchant`), for a Client from
   `openrails.New`. Such an engine refuses any other merchant.

A call with no selector, no default and no declared merchant is an
invalid-request error before any credential is minted. An empty selector does
not fall back to a default.

Slugs are lookup names: the server resolves them before authorization and
never stores them as identities in tokens, jobs or foreign keys. A SaaS host
that keeps merchant UUIDs uses `ForMerchantID`, or one client per merchant
(`client.With(openrails.WithMerchantID(id))`).

## On the wire

Both transports send exactly one `OpenRails-Merchant` header: a slug
(`OpenRails-Merchant: alpha`) or a stable id (`OpenRails-Merchant: id:<uuid>`).
Every merchant-scoped route (`/v1/merchant`, `/v1/catalog`, `/v1/import`,
`/v1/me`) honors it the same way: the server resolves the selector, authorizes
the credential for that merchant, and only then pins it, before acquiring a
merchant database connection or running business logic.

| Refusal | When |
|---|---|
| `400 merchant_selector_invalid` | a repeated, blank or malformed header |
| `404 merchant_not_found` | an unknown or inactive merchant |
| `409 merchant_binding_mismatch` | a credential or deployment bound to another merchant |

A request without the header is served as its credential resolves it. The
request path never depends on the selector.

## Credentials are independent

`WithAPIKey` uses one credential: a key for alpha cannot reach bravo because a
call selects bravo. Use `WithCredentialProvider` when a bearer token must be
minted for the requested merchant. The callback receives the
`CredentialTarget` and must be safe for concurrent calls; a failed mint returns
an error without falling back to another token:

```go
client, err := openrails.NewRemote(baseURL,
    openrails.WithCredentialProvider(func(ctx context.Context, target openrails.CredentialTarget) (string, error) {
        return credentials.TokenFor(ctx, target)
    }))
```

Catalog owner (`ForCatalogOwner`) and customer selection are independent of
merchant selection. In hosted platform billing the hosted merchant may be the
customer while the platform is the selling merchant; never substitute one for
the other.
