# Products and prices through one client

The same `*openrails.Client` is returned by embedded runtime construction or
`openrails.NewRemote`. Applications use its `Products` and `Prices` resources;
the runtime provides lifecycle and HTTP composition. Resource IDs are strings.
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
