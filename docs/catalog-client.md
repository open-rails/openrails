# Products and prices through one client

The same `*openrails.Client` comes from `openrails.New` (in process) and
`openrails.NewRemote`. Catalog operations are flat methods on it with typed IDs
(`billing.ProductID`, `billing.PriceID`); each noun has one
shape (`billing.Product`, `billing.Price`, `billing.Meter`) in Go and on the wire.

```go
price, err := client.CreatePrice(ctx, billing.CreatePriceParams{
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
identical requests return the same product and price. An offer key with
different financial terms is a conflict; invalid
initial prices leave no orphan product. A price's terms never change: a new
amount is a new price, and the same key with new terms archives its
predecessor, so past checkouts stay valid.

Inline creation takes local terms only; PSP links on an existing product go
through `CreatePrice` with `ProductID` and `UpdatePrice`. Provider writes never
run inside the local transaction.

Updates are merge patches (`catalog.Field`): omitted fields keep their values,
`catalog.Null` clears one. Reads are `GetProduct`/`GetPrice` by ID, or
`ListProducts` with `Keys` and `ListPrices` with `ProductKey` and `Key` (a key's
current price is its one not archived); lists return `billing.ListPage` with a cursor.

## Access and checkout

Check only the products already selected for a host page: `ListProductAccess`
with those `ProductIDs` and `LiveOnly` lists the live windows of that bounded
input, without loading the customer's complete purchase history. A product with
no window listed is not held.

```go
access, err := client.ListProductAccess(ctx, billing.ProductAccessListParams{
    CustomerIDs: []billing.CustomerID{customerID}, ProductIDs: pageProductIDs, LiveOnly: true,
})
if err != nil { return err }
_ = access.Items

session, err := client.CreateCheckoutSession(ctx, billing.CreateCheckoutSessionParams{
    Customer:   billing.CheckoutCustomerIdentity{ID: customerID},
    PriceID:    price.ID,
    SuccessURL: successURL,
})
```

Usually the customer's browser mints the session itself with the customer's
own credential (`POST /v1/me/checkout-sessions`); `CreateCheckoutSession` mints
it with the merchant's credential. Either way the customer pays it on the
payment page, and a saved card needs the customer's own proof. The server
derives one-off or recurring checkout from the selected price. Browser return
URLs provide navigation; verified provider events establish payment and access.

## What a host declares

Declare the merchant in `Config.Merchant`, and a host whose `catalog.yaml` is the
truth in `Config.Catalog` (`catalog.ParseApplicationYAML`). Everything else is
a Client method. See [embedding](embedded-integration.md) and the
[merchant guide](merchant-guide.md).
