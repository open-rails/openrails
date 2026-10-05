# Products and prices through one client

The same `*openrails.Client` comes from `openrails.New` (in process) and
`openrails.NewRemote`. Catalog operations are flat methods on it with typed IDs
(`billing.ProductID`, `billing.PriceID`, `billing.CatalogID`); each noun has one
shape (`billing.Product`, `billing.Price`, `billing.Meter`) in Go and on the wire.

```go
owner, err := client.ForCatalogOwner(authorID)
if err != nil { return err }
price, err := owner.CreatePrice(ctx, billing.CreatePriceParams{
    ProductData: &billing.CreatePriceProduct{
        Key:         "post-" + billingKey,
        DisplayName: title,
    },
    Key:        "post-" + billingKey + "-usd-" + strconv.FormatInt(priceCents, 10),
    UnitAmount: priceCents * 10_000, // USD micros, not Stripe cents.
    Currency:   "USD",
})
// Persist price.ID and price.ProductID (price.ID.String() for a string column).
```

Exactly one of `ProductID`, `ProductKey` and `ProductData` names the product.
`ProductData` creates the product or reuses the one under its key unchanged;
set labels deliberately through `UpdateProduct`.

The inline operation commits its product and price together. Concurrent
identical requests return the same product and price. A key owned by another
catalog or an offer key with different financial terms is a conflict; invalid
initial prices leave no orphan product. A price's terms never change: a new
amount is a new price, and the same key with new terms archives its
predecessor, so past checkouts stay valid.

Inline creation takes local terms only; PSP links on an existing product go
through `CreatePrice` with `ProductID` and `UpdatePrice`. Provider writes never
run inside the local transaction.

Updates are merge patches (`catalog.Field`): omitted fields keep their values,
`catalog.Null` clears one. Reads are `GetProduct`/`GetPrice` by ID or
`GetProductByKey`/`GetPriceByKey`; lists return `billing.ListPage` with a cursor.

## Access and checkout

Check only the products already selected for a host page. `CheckProductAccess`
returns a map for that bounded input; it does not load the customer's complete
purchase history. Use `ListProductAccess` with its cursor only when displaying
purchase history itself.

```go
access, err := client.CheckProductAccess(ctx, customerID, billing.ProductAccessCheckParams{
    ProductIDs: pageProductIDs,
})
if err != nil { return err }
_ = access[productID]

attempt, err := client.CreateCheckoutAttempt(ctx, billing.CreateCheckoutAttemptRequest{
    Customer: billing.CheckoutCustomerIdentity{ID: customerID},
    PriceID: price.ID,
    PaymentOptions: billing.CheckoutPaymentOptions{Rail: "stripe", PaymentMethodID: methodID},
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

Declare the merchant in `Config`, and a host whose `catalog.yaml` is the truth in
`Config.Catalog` (`catalog.ParseApplicationYAML`). Everything else, creator
scoping through `client.ForCatalogOwner(subject)` included, is a Client method.
