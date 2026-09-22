# Products and prices through one client

The same `*openrails.Client` is returned by embedded runtime construction or
`openrails.NewRemote`. Applications use its `Products` and `Prices` resources;
the runtime provides lifecycle and HTTP composition. Product, price and customer
references in ordinary SDK operations are strings.
The server validates their kind, merchant and catalog before applying changes.

```go
owner, err := client.ForCatalogOwner(authorID)
if err != nil { return err }
price, err := owner.Prices.Create(ctx, &openrails.PriceCreateParams{
    ProductData: &openrails.PriceCreateProductDataParams{
        Key: "post-" + billingKey,
        DisplayName: title,
    },
    Key: "post-" + billingKey + "-usd-" + strconv.FormatInt(priceCents, 10),
    UnitAmount: priceCents * 10_000, // USD micros, not Stripe cents.
    Currency: "USD",
})
// price.ID and price.ProductID are strings suitable for host persistence.
```

`ProductID` selects an existing product. `ProductData` instead creates or reuses
its immutable key in the authorized catalog. They are mutually exclusive.
Unlike Stripe's inline product creation, OpenRails reuses an existing key; its
existing display name and description remain unchanged. Set labels deliberately
through `Products.Update`, after any host-specific revision acceptance.

The inline operation commits its local product and price together. Concurrent
identical requests return the same product and price. A key owned by another
catalog, an offer key with different financial terms, or an attempt to relabel
an existing price is a conflict. Invalid initial prices leave no orphan product.
A new amount with a new key creates a distinct immutable offer and retains the
previous offer, so a failed host revision check cannot invalidate past checkout.

Inline creation supports local engine catalog terms. It rejects provider-native
links or legacy catalog provisioning, which may require network calls, without
committing the product. Existing `ProductID` creation and provider-link updates
retain their separate provisioning semantics. Provider writes never execute
inside the inline local transaction.

Optional update pointers distinguish an omitted field from explicit false or
zero. Product/price retrieval uses `Retrieve(ctx, id)` or
`RetrieveByKey(ctx, key)`; listing uses typed parameters and the existing page
contract rather than introducing new pagination machinery.

This design applies the context-first, resource-oriented methods and typed
operation parameters in [Stripe Go v86.4.2](https://github.com/stripe/stripe-go/tree/v86.4.2).
See Stripe's [client migration guide](https://github.com/stripe/stripe-go/wiki/Migration-guide-for-Stripe-Client),
[product creation](https://docs.stripe.com/api/products/create?lang=go), and
[price creation](https://docs.stripe.com/api/prices/create?lang=go). OpenRails keeps
its own money precision, catalog ownership and immutable pricing rules.

## Access and checkout

Check only the products already selected for a host page. `CheckMany` returns a
map for that bounded input; it does not load the customer's complete purchase
history. Use `ProductAccess.List` with its cursor only when displaying purchase
history itself.

```go
access, err := client.ProductAccess.CheckMany(ctx, &openrails.ProductAccessCheckManyParams{
    CustomerID: customerID,
    ProductIDs: pageProductIDs,
})
if err != nil { return err }
_ = access[productID]

session, err := client.CreateCheckoutSession(ctx, openrails.CreateCheckoutSessionRequest{
    Customer: openrails.CheckoutCustomerIdentity{ID: customerID},
    PriceID: price.ID,
    PaymentOptions: openrails.CheckoutPaymentOptions{Rail: "stripe"},
    IdempotencyKey: checkoutAttemptKey,
    SuccessURL: successURL,
    CancelURL: cancelURL,
})
```

The server derives one-off or recurring checkout from the selected price.
`PaymentOptions` selects the payment rail and carries its applicable collection
inputs. It does not set the price's product or merchant. Browser return URLs
provide navigation; verified provider events establish payment and access.

The typed ID utilities remain available for advanced declared billing imports,
provider-obligation and host-transaction contracts. They are not required to
pass ordinary customer, product or price references between SDK resources.

## Runtime ownership

Declare the local merchant through `embed.Options.Merchant`, and HTTP policy
through `Options.HTTP` when it is known at construction. Obtain one client from
`runtime.Client()` and use it for application operations, including creator
catalog scoping through `client.ForCatalogOwner(subject)`.

Filesystem manifests, provider reconciliation and preserved-identity restoration
are explicit local maintenance tools in `embed/operator`. They are outside the
ordinary HTTP client contract. Shared host database commits use the explicit
`embed.NewHostTransactions(runtime)` extension. Neither exposes a database
handle or adds domain methods to Runtime.

A multi-merchant host may learn a merchant ID only after its local control plane
provisions it. During setup, `embed/operator.New(runtime).DeclarePSP` supplies
attribution for that merchant's imported trial or historical facts. It shares
constructor declaration behavior: existing identity, alias, archive state,
custody and evidence are preserved; mismatched ownership or aliases refuse.
It neither configures credentials nor arms a provider. Keep this operator local
to setup before serving requests or starting workers, not in request handlers.
