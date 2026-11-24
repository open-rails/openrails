# OpenRails

OpenRails is a billing engine you add to an existing application. It keeps a merchant's catalog, orders, subscriptions, prepaid balances and metered usage in your PostgreSQL, collects through NMI, Stripe, CCBill or Solana, emails customers their receipts and billing notices, and answers what each customer may access. Your app keeps its users and sign-in.

| Path | Billing runs | Start here |
|---|---|---|
| Embedded | in your Go server's process | [Embedded guide](docs/embedded.md) · [example](examples/embedded) |
| Standalone, with the Go SDK | as the `openrails` server you host | [Standalone guide](docs/standalone.md) · [example](examples/standalone) |
| SaaS, with the Go SDK | on the hosted platform at [openrails.dev](https://openrails.dev) | [Client guide](docs/client.md#credentials) |
| HTTP only | any of the three, from any language | [Without the Go client](docs/client.md#without-the-go-client) |

[The integration guide](docs/guide.md) is the overview and the shortest working integration. [The client guide](docs/client.md) covers everything the Go client does, on every path. The browser SDK is [`@openrails/billing-ui`](web/billing-ui/README.md). Generated references: [routes](docs/api/routes.md), [error codes](docs/api/error-codes.md), [OpenAPI](api/openapi.json).

[The architecture docs](docs/architecture/README.md) describe each system and how it keeps OpenRails' guarantees, for people changing or reviewing it.

## License

OpenRails is under the Elastic License 2.0 ([LICENSE](LICENSE)): use, change
and self-host it for any purpose, except offering it to others as a hosted or
managed service. The browser SDK [`@openrails/billing-ui`](web/billing-ui) is MIT
([web/billing-ui/LICENSE](web/billing-ui/LICENSE)).
