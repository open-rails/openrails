# OpenRails

OpenRails is a billing engine for application developers. It manages a merchant's
catalog, orders, subscriptions, prepaid balances, and metered usage. Your app can
read effective customer access from OpenRails after payment or renewal. NMI,
Stripe, CCBill, and Solana handle payment according to the chosen rail.

## Choose an integration

| Where billing runs | How your app connects | Start here |
| --- | --- | --- |
| **Embedded** in your Go process | Start the engine with a PostgreSQL pool. Call its Go client and mount HTTP routes for browser or external callers. | [Embedded guide](docs/embedded.md) · [Running Go example](examples/embedded) |
| **Self-hosted standalone** on a server you run | Deploy the OpenRails server and connect over HTTP. Go backends can use the remote SDK. Your app supplies a trusted identity issuer. | [Standalone guide](docs/standalone.md) · [Compose sample](examples/standalone) · [Kubernetes](docs/kubernetes.md) |
| **OpenRails SaaS** at [openrails.dev](https://openrails.dev) | Set up a merchant on the hosted platform. Connect your backend and choose how your customers reach billing. | [SaaS developer guide](docs/saas.md) · [Hosted example](examples/hosted) |

## Build the first billing flow

1. **Scope the merchant and identity.** Set up a test merchant, configure an
   account on the appropriate [payment rail](docs/rails/README.md), and use its
   non-live [environment](docs/psp-environments.md). Follow the guide for your
   deployment to bind backend credentials and customer identities. [Auth](docs/auth.md)
2. **Define what the customer buys.** Create a product, a price, and the
   entitlement it grants. Apply the catalog explicitly. Monetary amounts use
   integer native units and travel as decimal strings on the wire. [Catalog](docs/catalog.md) ·
   [API conventions](docs/api/conventions.md#money-ids-and-times)
3. **Connect checkout.** Use your integration guide's checkout flow. The
   [browser guide](docs/frontend.md) and [Billing UI SDK](sdk/billing-ui/README.md)
   cover embeddable checkout and account UI. The [payment page guide](docs/payment-page.md)
   describes the self-hosted payment page.
4. **Verify access after payment.** Complete a sandbox checkout and verify its
   receipt and the customer's effective entitlement. Check that entitlement at
   the point where your app serves the protected feature. [Events](docs/events.md) ·
   [Entitlement timeline](docs/entitlements_timeline.md)

The Go SDK offers the same entitlement call for an embedded or remote merchant
client. Use a server credential with the appropriate merchant read permission.

```go
check, err := client.CheckEntitlements(ctx, billing.CheckEntitlementsParams{
    CustomerID:   customerID,
    Entitlements: []string{"course:101"},
})
if err != nil {
    return err
}
if check.Entitlements["course:101"] {
    // Serve the course.
}
```

The [examples](examples/README.md) show a complete checkout and access check in a
sample application for each integration. The [own-auth example](examples/own-auth)
shows how an embedded host integrates its existing application identity.

## Developer reference

| Need | Documentation |
| --- | --- |
| Call the API | [Conventions](docs/api/conventions.md) · [Routes](docs/api/routes.md) · [OpenAPI](api/openapi.json) · [Errors](docs/api/errors.md) |
| Define products and access | [Catalog](docs/catalog.md) · [Export and restore](docs/catalog-export.md) · [Entitlement timeline](docs/entitlements_timeline.md) |
| Bill recurring or metered usage | [Billing intervals](docs/billing-intervals.md) · [Dunning](docs/dunning.md) · [Usage billing](docs/usage-billing.md) · [Credits](docs/prepaid-credits.md) · [Running charges](docs/running-charges.md) · [Spend limits](docs/spend-limits.md) |
| Collect payment | [Rails](docs/rails/README.md) · [Provider environments](docs/psp-environments.md) · [Custody](docs/payment-method-custody.md) · [Agreements](docs/agreements.md) |
| Integrate customer and host workflows | [Customer contacts](docs/customer-contacts.md) · [Events and notifications](docs/events.md) · [Billing admin](docs/billing-admin.md) |
| Bring over an existing billing book | [Legacy import](docs/batch-import.md) · [Provider import](docs/psp-import.md) · [Billing archive](docs/merchant-portability.md) |
| Inspect runtime behavior and run a rehearsal | [Operations inventory](docs/operations-inventory.md) · [Recurring and event-driven River jobs](docs/river-jobs.md) · [Operations](docs/operations.md) · [Provider uncertainty](docs/provider-uncertainty.md) |
| Deploy and configure your server | [Configuration](docs/configuration.md) · [Kubernetes](docs/kubernetes.md) · [Vault](docs/vault.md) · [Limits](docs/limits.md) |

For repository development, see [Contributing](docs/dev/README.md),
[Testing](docs/dev/testing.md), and [CI and releases](docs/ci.md).

## License

OpenRails uses the Functional Source License 1.1 with an Apache 2.0 future
license. See [LICENSE](LICENSE). The browser SDK
[`@openrails/billing-ui`](sdk/billing-ui) is [MIT-licensed](sdk/billing-ui/LICENSE).
