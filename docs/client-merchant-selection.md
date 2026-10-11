# The merchant a call acts on

A call acts on one merchant. Nothing here grants authority: the credential
must hold the operation's permission on that merchant.

## The Go client

An `*openrails.Client` is a handle on one merchant, and no call names one.

- **In process**, the `*openrails.Engine` that `openrails.New` returns hands
  out handles: `Merchant(id)` for any live merchant in its directory,
  `MerchantBySlug` for the one `Config.Merchant` declares. Holding the engine
  is the authority; the handle carries no credential. Each call reads the
  merchant from the directory: an unknown, deleted or archived one answers
  `403 merchant_unresolved`.
- **Over HTTP**, a handle is its credential. `openrails.NewRemote` takes one
  credential per merchant, by merchant ID, and its `Merchant(id)` is that
  merchant's handle; a merchant with no credential has none.
  `openrails.NewRemoteMerchant` is the handle of the one merchant a credential
  is bound to.

```go
remote, err := openrails.NewRemote(baseURL, openrails.RemoteOptions{
    Credentials: map[billing.MerchantID]openrails.RemoteCredential{
        alphaID: {APIKey: alphaKey},
        bravoID: {Token: bravoToken}, // func(context.Context) (string, error)
    },
})
alpha, err := remote.Merchant(alphaID)
product, err := alpha.CreateProduct(ctx, params)
```

The server binds each credential to its merchant: a key for alpha filed under
bravo's ID still acts on alpha. When a response names the merchant it acted on
(the `OpenRails-Bound-Merchant` response header) and it is not the handle's,
the handle discards the response and fails the call with an
`*openrails.BoundMerchantError` naming both. The call has run by then, so file
each credential under its own merchant.

The Go client sends no `OpenRails-Merchant` header.

## The `OpenRails-Merchant` request header

Another HTTP client may name the merchant with one `OpenRails-Merchant`
header: a slug (`OpenRails-Merchant: alpha`) or a stable id
(`OpenRails-Merchant: id:<uuid>`). Every merchant-scoped route (`/v1/admin`,
`/v1/app`, `/v1/me`) honors it the same way: the server resolves the selector,
authorizes the credential for that merchant, and only then pins it, before
acquiring a merchant database connection or running business logic.

| Refusal | When |
|---|---|
| `400 merchant_selector_invalid` | a repeated, blank or malformed header |
| `404 merchant_not_found` | an unknown or inactive merchant |
| `409 merchant_binding_mismatch` | a credential or deployment bound to another merchant |

A request without the header is served as its credential resolves it. The
request path never depends on the selector.

Slugs are lookup names: the server resolves them before authorization and
never stores them as identities in tokens, jobs or foreign keys.

Customer selection is independent of merchant selection. In hosted platform billing the hosted merchant may be the
customer while the platform is the selling merchant; never substitute one for
the other.
