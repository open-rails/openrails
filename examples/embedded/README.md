# Embedded example

A course store with the app, AuthKit and OpenRails in one Go process: courses
to buy or rent, a bundle, and a monthly or yearly membership.
[The integration guide](../../docs/guide.md#a-minimal-integration) walks through it.

- `main.go`: AuthKit, the engine and the merchant's handle (`newBilling`), the mount.
- `catalog.go`, `catalog.yaml`: the catalog command; starting the site never applies it.
- `content.go`: the course list with buyer prices, each video gated by
  `CheckEntitlements` (`402` with the products that unlock it), signed media URLs.
- `web/`: the React app; billing-ui's `BuyButton` and `ProductList` sell.

```sh
cp merchant.example.yaml merchant.yaml    # your NMI sandbox gateway's IDs and keys
(cd web && pnpm install && pnpm build)
DATABASE_URL=postgres://… go run . catalog apply catalog.yaml
DATABASE_URL=postgres://… go run .        # http://localhost:8080
```
