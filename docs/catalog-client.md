# Products and prices through one client

The same `*openrails.Client`, a handle on one merchant, comes from an
`openrails.New` engine (in process) and from `openrails.NewRemote` or
`openrails.NewRemoteMerchant`. Catalog operations are flat methods on it with typed IDs
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
identical requests return the same product and price. A price key with
different financial terms is a conflict; invalid
initial prices leave no orphan product. A price's terms never change: a new
amount is a new price, and the same key with new terms archives its
predecessor, so past checkouts stay valid.

Inline creation takes local terms only; PSP links on an existing product go
through `CreatePrice` with `ProductID` and `UpdatePrice`. Provider writes never
run inside the local transaction.

Updates are merge patches (`catalog.Field`): omitted fields keep their values,
`catalog.Null` clears one. Send the `Revision` you read as `ExpectedRevision`
to refuse an update over someone else's change (`revision_mismatch`); see
[catalog ownership](catalog-ownership.md) for how edits and catalog documents
share fields. Reads are `GetProduct`/`GetPrice` by ID, or
`ListProducts` with `Keys` and `ListPrices` with `ProductKey` and `Key` (a key's
current price is its one not archived); lists return `billing.ListPage` with a cursor.

## Reading products and prices

`ListProducts` and `GetProduct` include populated `Prices`: current, unarchived
price records with their IDs, amount, currency and billing terms. A product
without a current price has `prices: []`. A retired price remains retrievable
through `GetPrice`; use `ListPrices` with `ProductID` and `Archived` to inspect
its history. Reads require `merchant:catalog:read`; edits require
`merchant:catalog:manage`.

```go
products, err := client.ListProducts(ctx, billing.ProductListParams{
    Keys: []string{"premium", "course-101"},
    Archived: new(false),
    ForSale: new(true),
})
// Each products.Items[i].Prices entry is a full price with a stable ID.
price, err := client.GetPrice(ctx, priceID, billing.GetPriceParams{})
prices, err := client.ListPrices(ctx, billing.PriceListParams{ProductID: productID})
```

This follows the product/related-prices model used by
[Paddle](https://developer.paddle.com/api-reference/products/list-products/):
OpenRails includes current prices by default. Like
[Stripe](https://docs.stripe.com/api/prices/list), prices also have independent
lookups filtered by product. Shopify's
[product model](https://shopify.dev/docs/api/admin-graphql/latest/objects/ProductVariant)
exposes priced variants; OpenRails keeps its existing price records rather
than introducing variants. Public browser lookups remain bounded to named
product keys and redact provider-private price fields.

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

order, err := client.CreateOrder(ctx, billing.CreateOrderParams{
    CustomerID:     customerID,
    Lines:          []billing.OrderLineParams{{PriceID: price.ID}},
    IdempotencyKey: key,
})
```

Usually the customer's browser creates and pays the order itself with the
customer's own credential (`POST /v1/me/orders`); `CreateOrder` creates an
unpaid one with the merchant's credential. Either way only the customer pays
it, signed in. Verified provider events and the order's own status establish
payment and access; a browser's return is navigation only.

## What a host declares

Declare the merchant in `Config.Merchants`, and a host whose `catalog.yaml` is the
truth in `MerchantDeclaration.Catalog` (`catalog.ParseApplicationYAML`). Everything else is
a Client method. See [embedding](embedded-integration.md) and the
[merchant guide](merchant-guide.md).

Price IDs name immutable financial versions: product `premium`, price key
`buy-usd`, and version one have ID `premium:buy-usd:v1`. New terms under the
same keys create `premium:buy-usd:v2`; existing orders and subscriptions keep
v1. Archiving and restoring v1 do not renumber it. Recreating identical terms
also returns v1. `Price.Version` is this one-based financial version;
`Price.Revision` is the separate optimistic-concurrency counter for changes
to the price key or its provider links.

Use `billing.ParsePriceID("premium:buy-usd:v1")` for a stored spelling, or
`billing.NewPriceID("premium", "buy-usd", 1)` when constructing it from keys.
`GetPrice`, `UpdatePrice`, `ListPriceHistory`, order lines, subscription
changes, and price filters all take the canonical ID. They do not accept a
UUID alias. `ListPrices` can instead filter by `ProductKey` and `Key` to find
the current version without guessing its number.

Database UUIDs remain private foreign keys so upgrades preserve accepted
financial facts, provider identities, and historical references. Migration
0073 derives public IDs from existing immutable keys and the stored revision.
It does not reprice existing purchases or contact payment providers. Existing
applications must switch stored public IDs to the IDs returned by the updated
catalog API when upgrading their client and server together.
