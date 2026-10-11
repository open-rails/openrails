# Migrating to v1

v1.0.0 is a hard cut from every v0 release: no aliases, no deprecated paths, no
compatibility fields. A host moves once, at v1.0.0, and from then on
[v1.x only adds](compatibility.md). This page lists what changed, grouped by
what a host does. A row a host never used needs nothing.

Before the code:

- **Database.** v1 installs one new baseline and does not upgrade a v0 schema.
  Start from an empty schema (`openrails.New` creates it) and bring billing facts in
  with `ImportBilling` ([batch import](batch-import.md)). A merchant archive
  written by v0 is not restored.
- **Versions.** Pin `github.com/open-rails/openrails` and
  `@openrails/billing-ui` to the same release; two apps sharing one billing
  schema move together.
- **Vocabulary.** A rail is a gateway kind (`nmi`); a PSP is an account on a
  rail (`mobius`). The payer is a *customer*, the opaque caller an *invoker*,
  and a stopped subscription is `canceled`.

## The merchant a request acts on

v0 let a request name its merchant several ways; v1 has one, the merchant its
credential is bound to ([merchant binding](merchant-binding.md)). Each way
that went, and what replaced it; the sections below give the details.

| Before | After |
|---|---|
| The `OpenRails-Merchant` header (a slug or `id:<uuid>`) | The credential binds the merchant; nothing in the request names it |
| Merchant API hosts (`<slug>.api.…`): `api_host`, `MerchantDeclaration.APIHost`, the host claim and verify, `GET /v1/admin/api-host` | One API host serves every merchant; a request's Host names none |
| The merchant in the path: billing-ui's `customerBase`, `/v1/merchants/{id}/…` | A customer token is bound to its merchant |
| The configured merchant, filling in when nothing named one: `Config.Merchant` | `Config.Merchants`; a single-merchant host's own sign-ins bind by `MerchantDeclaration.Issuer` |
| `WithMerchant`, `ForMerchantID`, `WithDefaultMerchant`, `WithMerchantID`, `CredentialTarget`, `openrails.RequestMerchant` | The merchant's handle: `Merchant(id)` on the engine or the remote, or `openrails.NewRemoteMerchant` |
| `Routes.Scope`, `Routes.Permissions` and the host's own permission names | Each route checks its `billing.Permission` pair: the `merchant:` name in the merchant's scope, the `root:` name in the Authenticator's `RootScope` |
| Codes `merchant_selector_invalid`, `merchant_not_found`, `merchant_binding_mismatch`, `merchant_directory_unavailable` | `403 merchant_unresolved` (bound to no live merchant), `403 customer_issuer_mismatch` (another issuer's customer) |
| Public reads finding their merchant by Host or the configured merchant | `GET /v1/products` takes `merchant=<merchant id>`; `GET /v1/config` is the deployment's, `GET /v1/me/config` the merchant's for a bound credential |
| billing-ui's `anonymous` call option | Public reads send no token; customer calls send a merchant-bound one (`tokenSource`) |
| The console's merchant in `sessionStorage`, sent as the header | A token exchanged for the chosen merchant, held in memory |
| The client merchant-selection guide | [Merchant binding](merchant-binding.md) |

## 1. Configuration

### `openrails.Config` and `Deps`

| Before | After |
|---|---|
| `HTTPConfig.MerchantAdmin`, `MerchantAPI`, `MerchantConfig`, `Catalog`; `Config.MerchantConfigHTTP` | `Routes.RouteGroups` (`Admin`, `Catalog`, `MerchantConfig`, `Metrics`, `Programmatic`) turns each group on; each route checks its own permission, one of `billing.Permissions()` ([auth](auth.md#permissions)) |
| Programmatic routes (`/v1/app`) admitting any application | Each admits an application holding its permission: `root:entitlements:read`, `root:catalog:read`, `root:usage:manage`, `root:events:read`, `root:orders:manage`, or the `merchant:` twin at the merchant ([auth](auth.md#permissions)) |
| `server.MerchantRead`, `server.MerchantWrite`, `server.MerchantAdmin`, `server.MerchantMetrics` | `billing.MerchantCustomerRead`, `billing.MerchantCustomerManage`, `billing.MerchantCatalogManage` and `billing.MerchantConfigManage` (the one admin permission split), `billing.MerchantMetricsRead`; a program on `/v1/app` holds `billing.MerchantEntitlementsRead`, `billing.MerchantCatalogRead`, `billing.MerchantUsageManage`, `billing.MerchantEventsRead` or `billing.MerchantOrdersManage` |
| `openrails.Permissions()`, `openrails.MachinePermissions()`, the `billing.Merchant…` permission names; `authkit.Config.Merchant` with `Root` | `billing.Permission` constants in two personas (`billing.MerchantCustomerRead`, `billing.RootCustomerRead`, …), listed by `billing.Permissions()`: declare them in your auth and grant them in its roles. `Routes` names no permission and no scope; a `root:` permission is checked in the Authenticator's `RootScope`, a `merchant:` one in the merchant's own scope |
| `Config.HTTP` (`HTTPConfig` with `Checkout`, the customer routes, `Merchant`, the cookie origin); `Config.AllowCatalogUpdates`; `Config.AdminConsole` and `Client.AdminConsole`; mounting under a router group | An `openrails.Routes` given to the engine's `Routes` and each adapter's `Mount` on the root router: `Auth`, `Prefix`, `RouteGroups`, `AdminConsole`. The public, customer and webhook routes are always mounted. A missing hook fails the mount, not `New` |
| `Routes.Guards`, the `openrails.RouteSet` constants (level, resource and route groups); `Routes.Storefront`, `Routes.Customers` and the customer scopes (`CustomerSelfService`, …); `Routes.Merchant`, `Routes.MerchantConfig` | `Routes.RouteGroups` turns each group on; OpenRails decides each admin route's level (read or update) and the permission it checks |
| `openrails.Migrate(ctx, pool, cfg)` before `New` | Removed: `New` creates or upgrades OpenRails' tables, River's and this month's partitions before anything else touches the database |
| `VaultConfig.Enabled`; `vault.enabled` (env `VAULT_ENABLED`) on the standalone server | Removed: a non-nil `Config.Vault` (or `Deps.Vault`) connects to Vault; on the standalone server a `vault:` section or any `VAULT_*` setting declares it, and `vault.enabled` refuses boot |
| `catalog_edits` (env `CATALOG_EDITS`) on the standalone server | Removed: catalog edits over HTTP are always on, documents skip what an edit set, and `catalog_edits` refuses boot |
| `Config.SecretBackend`, `Config.Encryption`, `CredentialSnapshotID`, `CredentialReadOnly`, `AlertSecretBackend`; `secret_backend`, `encryption` (env `SECRET_BACKEND`, `ENCRYPTION_MASTER_KEY`) | Removed, and refused at boot: merchant configuration lives in `Config.Merchants` (the manifest) or, with `Config.Vault.KVMount`, in Vault; Postgres stores no credential. Export a database-held configuration with the previous release's `dump-merchant-config` before upgrading ([vault.md](vault.md)) |
| `Config.Schema`, `Config.RiverSchema` | `Config.Database` (`openrails.DatabaseConfig`) with `Schema` and `RiverSchema`; in `config.yaml`, `database.schema` and `database.river_schema` (env `DATABASE_SCHEMA`, `DATABASE_RIVER_SCHEMA`) |
| `Config.River` (`RiverManaged`, `RiverHostOwned`) | `New` always creates River's tables in `Config.Database.RiverSchema` (default: the schema plus `_river`). The engine's `Start(ctx)` runs OpenRails' own River there; `Start(ctx, openrails.WithRiverClient(fleet))` runs on a host fleet built with the engine's `RiverJobs()` in that schema. `New` starts nothing |
| `Config.Catalog *billing.CatalogApplyParams`, `billing.ParseCatalogApplicationYAML` | `MerchantDeclaration.Catalog` is a `*catalog.Application` from `catalog.ReadFile`, `catalog.ParseApplicationYAML` or `catalog.ParseApplicationJSON` |
| `Config.Merchant` and `Config.Catalog`: one merchant per engine | `Config.Merchants`, one `openrails.MerchantDeclaration` per merchant keyed by its slug, each with its `Catalog` and, for a host whose own users sign in, its `Issuer` (set in Go; a merchant file's `issuer` is refused). An issuer binds its credentials to one merchant: declaring it twice fails `New` |
| `PSPConfig` as a one-entry map keyed by rail; `CustodianConfig` keyed by kind | `openrails.PSPConfig` with `Rail`, `AccountID`, `Archived`, `Custodian`, `Signer`, `Secrets`, `Settings`; `openrails.CustodianConfig` with `Kind` |
| `openrails.PSPFromEnv` | Removed: build each PSP from your own configuration with the rail's typed struct (`openrails.NMIPSP`, `StripePSP`, `CCBillPSP`, `SolanaPSP`) and its `PSPConfig()`, or keep the merchant in a file read by `openrails.ReadMerchantFile` |
| `MerchantDeclaration` with `Profile`, `Invoice`, `BillingPolicies`, `CheckoutRouting` and their `*Config` types | `MerchantDeclaration.Settings` is the `billing.MerchantSettings` document the configuration API reads and applies |
| `Deps.EmailSender` for control-plane mail beside `Config.SendGrid` for billing mail; `Deps.SMSSender` | One sender: `Deps.Email` (an `openrails.EmailSender`: `Send` an `openrails.Email`, `CheckHealth`) or `Config.SMTP` (any SMTP server: `Host`, `Port`, `Username`, `Password`, `From`). Setting both is refused. The standalone server's AuthKit mail goes through it rendered, or through the `Email` and `SMS` senders of `server.Deps.Auth` |
| `Config.SendGrid` (`SendGridConfig`, SendGrid's API); the standalone server's `sendgrid` section (`SENDGRID_*`) | `Config.SMTP` (`SMTPConfig`); the `email_smtp` section (`EMAIL_SMTP_HOST`, `EMAIL_SMTP_PORT`, `EMAIL_SMTP_USERNAME`, `EMAIL_SMTP_PASSWORD`, `EMAIL_SMTP_FROM`). SendGrid over SMTP is host `smtp.sendgrid.net`, port 587, username `apikey`, password the API key. A `sendgrid` setting refuses boot |
| `Config.ControlPlane` (`ControlPlaneConfig`, `AuthConfig`, `MerchantCreationConfig`, `ResourceServerConfig`, `NamingConfig`), `ErrNoControlPlane`, `Deps.SMS`, `Deps.HasVaultedPaymentMethod`, `AdminConsole.Issuer`, `Email.Auth`, and the control-plane `Client` methods (`ProvisionMerchant`, `AuthKit`, `FleetAnalytics`, …) | The `server` package: `server.New(ctx, server.Config{Engine: cfg, Auth: …}, server.Deps{Engine: deps})` builds the engine, its own AuthKit and the control plane; the methods are the `server.Server`'s; `server.Config.ConsoleIssuer` signs staff in to the console at a trusted issuer |
| `server.AuthConfig`, `server.ResourceServerConfig`, `server.TrustedIssuerConfig`, `server.AuthRateLimit`; `server.Config`'s `Registration`, `ResourceServer`, `PasswordlessLogin`, `PasswordlessAutoRegistration`, `FrontendBaseURL`, `TrustedProxies`, `CloudflareProxies`, `AuthRateLimits`; `server.Deps`' `SMS` and `AuthEmail`; the `auth` keys `issuer`, `schema`, `keys_path`, `mint_disabled`, `allow_*`, `direct_peer_ip`, `request_origin` and `naming`, and the `resource_server` section | `server.Config.Auth` is AuthKit's own configuration (`authkit.Config`), passed through: `auth:` in AuthKit's keys, `AUTH_<PATH>` in the environment ([runtime configuration](runtime-configuration.md#authentication)); `server.Deps.Auth` is AuthKit's dependencies; merchant names follow `server.Config.Naming`; a merchant's trusted issuers are AuthKit remote applications in its group ([auth](auth.md#trusted-issuers)). A retired key refuses boot and names its replacement |
| A hand-written `ALTER … OWNER` pass after migrating a shared schema | The role of the pool `New` runs with owns every object; for another owner, have that pool's connections `SET ROLE` to it |
| `Routes.CookieOrigin` and cookie admission; `Routes.Provisioning`; the `AdminConsole` struct (`Path`, `AuthBaseURL`, `Extensions`) | Removed: ambient cookies never authenticate, a browser sends its credential in a header; SCIM mounts with `RouteGroups.Programmatic`; `AdminConsole: true` serves the console at the prefix's `/admin`, signing staff in at `Routes.AdminConsoleAuthBase`, `/api/v1` by default (a standalone server's path and extensions are `server.Config.AdminConsole`'s) |
| `CustomerRoutesConfig{Authenticate: fn}` per profile, further customer surfaces | Removed: one customer surface, `/v1/me`; every route acts on the merchant its credential is bound to, and `Routes.Auth` names the customer |
| `Deps.AuthKit`, `Deps.CustomerFor`, `Deps.AuthorityFor`, `Deps.Authenticate`, `Deps.Authorize`, `Deps.RecentSignIn`, `Deps.AuthenticateCustomer` | `New` takes no auth. `Routes.Auth`, an `openrails.Authenticator` that says who a request is, is given at `Mount`; a route group fails the mount without one that is a authkit/auth `Root` |
| `Deps.UserExists`, `Deps.UserEmail`, `Deps.ResolveUsername`, `Deps.CheckoutCustomer` | `Deps.UserInfo` (your AuthKit) answers each customer's email, username and name when OpenRails needs them; a standalone server keeps what your directory pushes over SCIM. Your auth decides who may sign in |
| `Deps.Contacts: ak`, `openrails.Contacts`, `openrails.Contact`, `openrailstest.Contacts` | `Deps.UserInfo: ak.UserInfo()` (AuthKit v1.14.0), `openrails.UserInfo` (helpers' `userinfo.Lookup`: `Get`, `Search`), `userinfo.User`, `openrailstest.UserInfo` |
| `openrails.Identity` (`Kind`, `SubjectID`, `CustomerID`, `CredentialClass`, `Permissions`), `PrincipalKind`, `CredentialClass`, `Requirement`, `Authority`, `Target`, `Scope`, `DelegatedPrincipal`, `GateError`, `RequestAuthenticator`, `ErrForbidden` | `openrails.Identity`: `Subject` and `SubjectKind`, `Invoker`, `Credential`, from `Auth.Identity` |
| `Deps.ProviderCredentials`, `ProviderCredentialSnapshot` | Removed: PSP secrets come from `Config.Merchants` (`PSPConfig.Secrets`) or the secret store |
| `MerchantDeclaration.APIHost` (`api_host` in a merchant file or manifest); `Deps.DNSResolver` | Removed: a merchant has no API host, and a request names its merchant by its credential. A file or manifest that still sets `api_host` refuses boot |
| No parser for a merchant's YAML | `openrails.ReadMerchantFile`, `openrails.ParseMerchantDeclaration`; the file names its merchant with a required `slug:` |
| `HTTP.Checkout` needed `Deps.Authenticate` | `Routes.Storefront` needs none: the routes are public or addressed by session id |
| Helper methods on `Config` and its nested types (`IsTestMode`, `SchemaName`, `Validate`, …) | Removed. `openrails.New` validates; compare fields (`cfg.TestMode == openrails.Live`) |
| `Config.Port`, `Config.Host`, `Config.MerchantManifestOverlays`; `koanf` struct tags | Removed: they are the standalone server's own settings. A host that decoded a file into an OpenRails type declares its own struct |
| `AuthConfig.Naming merchant.NamingConfig` | `server.NamingConfig`, `server.FormerNamesConfig`, `server.FormerNamesMode` |
| `openrails.New` ignored `WithAPIKey`, `WithTokenProvider`, `WithCredentialProvider`, `WithHTTPClient` | `openrails.New` takes no options; a credential is an `openrails.RemoteCredential` given to `openrails.NewRemote` |
| `Close` on a client from `With` closed the engine | There is no derived client: `Close` is the engine's, and a handle owns no lifecycle |
| `openrails.WithCurrency`; `client.Balance` | Removed: every request names its currency |

### Rate limits and health

| Before | After |
|---|---|
| `rate_limits.default`: 300 a minute per address on every other route | Removed. `rate_limits` takes `checkout`, `payment`, `subscribe` and `webhook`; any other key refuses boot. A per-address ceiling belongs to the proxy |
| `GET /`, `/healthz`, `/readyz`, `/health/ready?verbose=1`, 503 `not_ready` | `/health/live` and `/health/ready`; a failed readiness check is 503 `service_unavailable` and the failing dependency is logged, not answered |
| Capability `hosted_checkout`; route groups `merchant_admin`, `merchant_api`, `merchant_config`, `catalog` | Capability `checkout_sessions`; `/v1/config`'s `capabilities` list the groups `checkout`, `customer`, `merchant`, `merchant_config`, `webhooks` |
| Permissions `merchant:payment-providers:read`, `merchant:payment-providers:update` | Removed: the PSP routes are behind `billing.MerchantConfigManage` or `billing.RootConfigManage` |
| CLI flag `--provider-account` | `--psp` |

## 2. YAML manifests

In the catalog document, products, prices and meters are maps keyed by their
key instead of lists of objects with a `key:` field. The list form is refused:

```yaml
# before
products:
  - key: course-101
    display_name: Course 101
    prices:
      - key: purchase
        amount: 4.99 USD

# after
products:
  course-101:
    display_name: Course 101
    prices:
      purchase:
        amount: 4.99 USD
```

The JSON body of `POST /v1/catalog/apply` (was `/v1/merchant/catalog/applications`) and the Go
`catalog.Application` (`Products`, `Prices` and `Meters` are maps) change the
same way. Plain lists such as `entitlements`, `psps` and `rate_cards` stay lists.

The merchant declaration (`merchant_config.yaml`, `merchants.<slug>` in a
standalone manifest) changes in two places.

PSPs and custodians are no longer nested under their rail or kind, and neither
are their secret overlays:

```yaml
# before
psps:
  mobius:
    nmi: {account_id: "000000", settings: {...}}
custodians:
  vault-one:
    basis_theory: {account_id: tenant-id}

# after
psps:
  mobius: {rail: nmi, account_id: "000000", settings: {...}}
custodians:
  vault-one: {kind: basis_theory, account_id: tenant-id}
```

The merchant's own settings move under one `settings:` block with the names and
units of the configuration API. Omitted fields keep their stored values.

| Before | After (under `settings:`) |
|---|---|
| `profile:` | `profile:` |
| `invoice.collection_threshold`, `invoice.monthly_floor`, `invoice.billing_period_boundary` | `collection_threshold`, `monthly_floor`, `billing_period_boundary` |
| `invoice.delinquency_grace_days`, `invoice.delinquency_amount_floor` | `arrears_grace_days`, `arrears_delinquency_floor` |
| `billing_policies` as a map by name, `window: 15m`, `outstanding_cap` | `billing_policies` as a list with `name`, `outstanding_cap_amount` and windows `{key, window_seconds, limit}` |
| `delegated_invoker_wasted_spend_windows` | `delegated_invoker_wasted_spend_limits` |
| lower-case currencies | upper-case (`USD`) |

## 3. Go calls

The Client is flat: no sub-clients (`client.Products`, `client.Prices`,
`client.Catalog`, `client.ProductAccess`, `client.PaymentProviders`,
`client.MerchantConfiguration`), no `Retrieve`. Lists return
`billing.ListPage` and take a `billing.PageRequest` (`Cursor`, `Limit`); there is
no offset and no total; every list's params embed `billing.PageRequest`
(`PaymentListParams.Page` and its siblings are gone). Ids are typed: `billing.CustomerID`, `billing.ProductID`,
`billing.PriceID`, `billing.PSPID`, `billing.CheckoutAttemptID`.

Every merchant route has exactly one Client method, named for it, and a
request type is `…Params` named for its method: `CreateCheckoutSessionRequest`
is `billing.CreateCheckoutSessionParams`, `CaptureParams` is
`billing.CaptureAdmissionParams`, `ListHostEventsRequest` is
`billing.HostEventListParams`. Settings nouns lose their `Input` suffix
(`billing.BillingPolicy`, `billing.MerchantProfile`).

### The engine and merchant handles

`New` returns the engine and `NewRemote` the remote; a `*Client` is a handle
on one merchant from either, and no call names its merchant.

| Before | After |
|---|---|
| `openrails.New(ctx, cfg, deps, opts...)` returned the `*Client` | `openrails.New(ctx, cfg, deps)` returns the `*openrails.Engine`. A merchant's operations are on its handle: `MerchantBySlug` for one `Config.Merchants` declares, `Merchant(id)` for any live merchant |
| `client.Start`, `Close`, `Ready`, `Probes`, `Routes`, `RiverJobs` | The same methods on the `*openrails.Engine`; each adapter's `Mount` takes the engine |
| `openrails.NewRemote(url, opts...)` returned the `*Client` | `openrails.NewRemote` takes `openrails.RemoteOptions` and returns the `*openrails.Remote`: `Merchant(id)` is a merchant's handle, `Ready` and `Close` are the remote's. `openrails.NewRemoteMerchant` is one merchant's handle directly |
| `WithAPIKey`, `WithTokenProvider`, `WithCredentialProvider`, `CredentialTarget` | An `openrails.RemoteCredential` (`APIKey` or `Token`) per merchant, in `RemoteOptions.Credentials` by merchant ID. A response naming another merchant fails the call with an `openrails.BoundMerchantError` |
| `WithHTTPClient`, `WithTimeout` | `RemoteOptions.HTTPClient` and `RemoteOptions.Timeout`; in process, and with `openrails.NewRemoteMerchant`, the call's context is its bound |
| `WithMerchant`, `ForMerchantID` on a call; `WithDefaultMerchant`, `WithMerchantID`; `client.With` | Removed: a handle acts on its own merchant. Take the other merchant's handle |
| `openrails.RequestMerchant` | Removed: OpenRails binds the merchant; a host's `Authenticate` says who the request is |
| `client.DeclarePSP(ctx, merchantID, declaration)` | `client.DeclarePSP(` on the merchant's handle takes the declaration alone |
| `server.Server.Client` | `server.Server.Engine`, whose `Merchant(id)` is any live merchant's handle |
| `openrails billing export` and `import` with `--url`, `get-merchant-config` and `apply-merchant-config` with `--server-url`, took `--merchant` | They act on the token's merchant and refuse `--merchant`, which names a local merchant |

### Customers

| Before | After |
|---|---|
| `client.EnsureCustomers`, `billing.EnsureCustomerParams` (`Email`, `Username`, `Blocked`), `POST /customers/ensure` | Nothing to declare: a customer is your user's id, created by its first use; `client.UpdateCustomer` creates one OpenRails has not seen. Emails and names come from `Deps.UserInfo` or SCIM ([customer contacts](customer-contacts.md)) |
| `Customer.Email`, `Username`, `Blocked` | `Customer.Contact` (`Email`, `Name`, `Username`, `Active`, `SyncedAt`), null when the directory holds none |
| `CustomerListParams.Query` (`?q=`) | `Search` (`?search=`): email, username, name, or the customer's id |
| `DeclaredCustomer.Email` in a billing import | Removed: the directory supplies emails |
| A blocked customer refused at checkout (`customer_blocked`) | Your auth decides who signs in; OpenRails refuses no customer by flag |

### Entitlements and access

| Before | After |
|---|---|
| `HasEntitlement(ctx, subject string, key, at)`, `CheckEntitlements(ctx, subject string, keys, at)` | Your backend: `client.CheckEntitlements(` with `billing.CheckEntitlementsParams` (`CustomerID`, at most 100 `Entitlements`, `At`). Staff: `client.ListEntitlements(` with `billing.EntitlementListParams` (`CustomerIDs`, `Entitlements`, at most 100 each, and `At`); a key not listed is not held |
| `ListActiveEntitlements(ctx, subjects, at)`, `ListEntitlements(ctx, subject, at)` | `client.ListEntitlements(` with up to 100 `CustomerIDs` (and a `Prefix`, `At`, a page), answering each customer's keys |
| `ListCustomersWithEntitlement` | `client.ListEntitlements(` with one key in `Entitlements` and no `CustomerIDs` |
| `GrantEntitlement`, `RevokeEntitlement` | Grant the product that carries the key: `client.CreateProductAccess(`, `client.RevokeProductAccess(` |
| `ResolveEffectiveTier` | Removed: a customer's tier is the `tier_group` and `tier_rank` of the products they hold (`client.ListProductAccess(`) |
| `client.ProductAccess.Check`, `CheckMany`, `List` | `client.ListProductAccess(` (`CustomerIDs`, `ProductIDs`, `LiveOnly`), `client.CreateProductAccess(`, `client.RevokeProductAccess(` |

Every entitlement now derives from a grant; `source_type` is `purchase`,
`subscription`, `admin` or `grace`.

### Catalog

| Before | After |
|---|---|
| `client.Products.Create`, `Ensure`, `Retrieve`, `RetrieveByKey`, `Update`, `List` | `client.CreateProduct(` (`Ensure`: a catalog application, `client.ApplyCatalog(`), `client.GetProduct(`, `client.UpdateProduct(`, `client.ListProducts(` (by key: `Keys`) |
| `client.Prices.Create`, `Retrieve`, `RetrieveByKey`, `Update`, `List`, `SetKey` | `client.CreatePrice(`, `client.GetPrice(`, `client.UpdatePrice(` (a merge patch: `archived`, `psp_links`), `client.ListPrices(` (by key: `ProductKey`, `Key`), `client.ListPriceHistory(` |
| `client.Catalog.Apply`, `Revision` | `client.ApplyCatalog(`; its receipt reports the catalog revision (`applied_revision`), and each product, price and meter its own `revision` |
| `ProductCreateParams`, `PriceCreateParams`, … | `billing.CreateProductParams`, `billing.CreatePriceParams`, `billing.UpdateProductParams`, `billing.UpdatePriceParams` |
| `EnsureUsageMeter`, `GetUsageMeter`, `ListUsageMeters`, `SetDefaultUsageRateCard`, `DeleteDefaultUsageRateCard` | `client.SetMeter(` (its `RateCard`; null removes it), `client.GetMeter(`, `client.ListMeters(`; the meter's rate card prices every customer (no per-customer price) |
| `ListOffersForEntitlements` | `client.ListProducts(` with `Entitlements` and `ForSale`; each product carries its current prices |
| Creator-owned catalogs: `EnsureOwnCatalog`, `EnsureCatalogForOwner`, `GetCatalogForOwner`, `WithOwnCatalog()`, `ForCatalogOwner`, `EnsureCatalog`, `GetCatalog`, `ListCatalogs`; `billing.Catalog`, `billing.CatalogID`, `billing.EnsureCatalogParams`, `billing.CatalogListParams` | Removed: a merchant has one catalog |
| `CatalogID` on `billing.Product`, `CreateProductParams`, `CreatePriceProduct`, `ProductListParams`, `PriceListParams` and `CatalogApplicationReceipt`; `catalog.Application.CatalogID` | Removed with the catalogs |
| Product and price activate and deactivate; price `providers` | `archived` in the update; `psps` |
| `CheckCatalogDrift` | `client.RefreshPSPs(`, which also reads each PSP's catalog for drift |
| `ListPurchaseReviews`, `ResolvePurchaseReview`; `ArchiveProductParams` with `Action` and `Window` | Removed: archiving a product (`client.UpdateProduct(` with `Archived`) stops its sales and refunds nobody; refund a purchase with `client.RefundPayment(` |

### Checkout

| Before | After |
|---|---|
| `CreateHostedCheckoutSession`, `CreateCheckoutSession(…)`, `GetCheckoutSession`, `ConfirmCheckoutSession` | `client.CreateOrder(` with `billing.CreateOrderParams` (`CustomerID`, `Lines`, `IdempotencyKey`): an unpaid order the customer pays signed in (`POST /v1/me/orders/{id}/pay`) |
| `LookupCheckoutSession`, `GetCheckoutSessionByKey` | Removed: repeat the same request with the same `IdempotencyKey` |
| `ListCheckoutRailOptions`, `ListCheckoutRailOptionsByKey` | `client.PreviewOrder(`: the preview's `PaymentOptions` |
| `GetCheckoutConfig(ctx)` | Removed: `GET /v1/me/config` answers a merchant's customers its PSPs as `payment` |
| `billing.CheckoutSessionID`, `cs_` ids, `CheckoutSession` | `billing.OrderID`, `ord_` ids, `billing.Order` |
| `next_action` as `redirect_to_url`, `solana_qr`, `solana_transaction`, with a top-level `url` | `billing.NextAction` with `type` (`redirect_to_url`, `authenticate`, `solana_pay`, `solana_sign_transactions`), `url`, `psp_id`, `transactions` and `payload` |
| `CheckoutPaymentOptions` (`Card`, `Rail`, flat `NameOnCard`, `Address1`, `Zip`, …) | Removed: a Client never pays; the customer's order payment names a saved card, a token with `billing.BillingDetails`, a Solana token or a return URL |
| `CreatePaymentMethodSession`, `CreateSolanaCancelSession`, `CreateSolanaTierChangeSession` | Removed |

Checkouts in flight at the upgrade complete. A Stripe Checkout page opened
before the upgrade and paid after it is booked from its checkout attempt's
accepted terms (credits included) when `checkout.session.completed` arrives,
so keep that event, and `checkout.session.async_payment_succeeded`, enabled
for a day after upgrading. A card sale still pending settles through its
provider operation, and a Solana transfer for a transaction already handed to
the wallet is credited. A checkout not yet paid cannot be paid after the
upgrade; its buyer buys through an order.

### Subscriptions

| Before | After |
|---|---|
| `CancelSubscription`, `ResumeSubscription`, `UpdateSubscriptionPaymentMethod` returned `error` | `client.CancelSubscription(`, `client.ResumeSubscription(` and `client.SetSubscriptionPaymentMethod(` each return the `billing.Subscription` |
| `ChangeTier(ctx, id, idempotencyKey, ChangeTierRequest)` | `client.ChangeSubscription(` and `client.PreviewSubscriptionChange(` take `billing.ChangeSubscriptionParams` (`PriceID`, `Quantity`, `IdempotencyKey`) |
| `ListSubscriptions(ctx, SubscriptionFilter)` with a total | `client.ListSubscriptions(` with `billing.SubscriptionListParams`, a cursor page, newest first |
| `Subscription.CustomerID`, `ProductID`, `PriceID` as strings; `SubscriptionPrice` | Typed ids; `price` is the catalog `billing.Price` |
| `models.StatusCancelled`, `cancelled_at` | `billing.SubscriptionCanceled`, `canceled_at` |
| `SubscriptionAccess.StartAt`, `EndAt` (`start_at`, `end_at`) | `StartsAt`, `EndsAt` (`starts_at`, `ends_at`) |
| `CutoverProvider`, `GetProviderCutover`, `PreviewProviderCutover`; engine takeover (`TakeOverBilling`, …) | Removed: a provider-owned subscription stays on its PSP until it ends |
| `CancelPlanMigration`; reprice-all routes; reprice batches; reprices | `client.CreatePriceMigration(`, `client.PreviewPriceMigration(`, `client.ListPriceMigrations(`, `client.GetPriceMigration(`, `client.CancelPriceMigration(`; a subscription's pending move is its `scheduled_change`, cleared by a change back to its current price |
| `GetMyInvoice`, `GetMySubscription`, `PayInvoiceNow`, `RetrySubscriptionNow` | Removed from the Client: `/v1/me` is for browsers |

### Credits, usage and admissions

| Before | After |
|---|---|
| `DepositCredits`, `GetDeposit` | `client.CreateCreditGrants(` with up to 100 `billing.CreateCreditGrantParams`, all or none; `client.ListCreditGrants(` by `source_id` answers what a key did |
| Revoke with a body on `DELETE` | `client.RevokeCreditGrant(` |
| `GetCreditAccount` | `client.GetBalance(` answers `billing.Balance` (`balance_amount`, `held_amount`, `available_amount`, `owed_amount`) |
| `Admit`, `AdmitBatch`, `Capture(ctx, id, amount, usage)`, `Release`, `ExtendHold` | `client.Admit(` takes a slice of `billing.AdmitParams` and answers one `billing.AdmissionVerdict` each; `client.CaptureAdmission(` (`billing.CaptureAdmissionParams`, amount required), `client.ReleaseAdmissions(`, `client.ExtendAdmissions(` (one `billing.AdmissionResult` per item). The caller's `request_id` is the id |
| `RecordUsage(RecordUsageInput)`, `UsageRollup`, `ResourceRevenueDaily` | `client.RecordUsage(` with `billing.RecordUsageParams`; usage reports through `client.QueryMetrics(`. Resource revenue is removed |
| `SetCustomerSpendDelegations`, `SetCustomerSpendDelegation`, `DeleteCustomerSpendDelegation` | Removed: only the customer's own credential spends its balance |
| `InvokerTypePayer` (`invoker_type: "payer"`) | `billing.InvokerTypeCustomer` (`"customer"`) |
| `DeclaredTransaction.AmountCents` | `Amount`, in native units (micros for USD) |
| `DeclaredSubscription.UserEmail`; `DeclaredPaymentMethod.LastFour`, `CardType`, `ExpiryDate`, `InitialTransactionID` | `DeclaredCustomer.Email`; `DeclaredPaymentMethod.Card` (`billing.CardDetails`); `InitialTransactionID` is removed |

### Payments and invoices

| Before | After |
|---|---|
| `ListPayments(ctx, PaymentFilter)` | `client.ListPayments(` with `billing.PaymentListParams`; no status filter |
| `Payment` with `Object`, `Refunded`, `Captured`, string ids | `billing.Payment` with `Kind`, a typed `Status`, `Channel`, `PSPID`, `Card`, `Failure`, and `Refunds` as a slice |
| `ListPaymentMethods(ctx, customerID string, PageOptions)` | `client.ListPaymentMethods(` with a `billing.CustomerID` and `billing.PageRequest` |
| `SetDefaultPaymentMethod` | Removed: a charge names its card; the customer sets a default card per currency on `/v1/me` |
| `ListMerchantInvoices`, `GetMerchantInvoice`, `ListInvoicePaymentAttempts`, `RecordInvoicePayment` | Invoices are usage orders: `client.ListOrders(` with `Origin: billing.OrderOriginUsage`, `client.GetOrder(`, `client.ListPayments(` and `client.ListPaymentAttempts(` with `OrderID`, `client.CreatePayment(` with `OrderID` |
| `EnsureCustomerInvoiceProfile`, `GetCustomerInvoiceProfile` | `client.UpdateCustomer(` with `InvoiceProfile`; `client.GetCustomer(`. No profile already means net 0, charged automatically |
| `HasSettledPayment` | `client.ListOrders(` with `PriceID` and `Status: billing.OrderComplete` |
| `billing.ChannelAdmin` | Removed: a payment's channel is `billing.ChannelRail` or `billing.ChannelManual` |
| `CreateOffChannelPayment` answered `{payment_id, status, entitlements}` | `client.CreatePayment(` with the `OrderID` of an unpaid order; it answers the `billing.Payment`, and changed terms under the same transaction id are `billing.ErrIdempotencyKeyReused` |
| `Subscription.RailSubscriptionID`, `CreditGrant.SourceID`, `AlertWebhook.Name` as `string` (`""` when absent) | `*string`, nil when absent |
| `Invoice.PeriodFrom`, `PeriodTo`; `InvoiceListParams.PeriodFrom`, `PeriodTo` | A usage order's `Usage.PeriodStartsAt`, `PeriodEndsAt`; orders are listed newest first, by `Origin`, `Status` and `Overdue` |

### PSPs, configuration and operations

| Before | After |
|---|---|
| `client.PaymentProviders.List`, `Retrieve`, `Upsert`, `Archive`; `RefreshProviders` | `client.ListPSPs(`, `client.GetPSP(`, `client.CreatePSP(`, `client.UpdatePSP(` (which also archives), `client.PreviewPSPRouting(`, `client.RefreshPSPs(`; the rail registry is `rails` in the public `GET /v1/config` |
| `psp_id` as a UUID string; `DeclarePSP` returned a `uuid.UUID` | `billing.PSPID` (`psp_…` on the wire); `client.DeclarePSP(` returns the `billing.PSP` |
| `GetMerchantSettings`, `SetMerchantSettings`, `Verify`, `client.MerchantConfiguration` | `client.GetMerchantConfiguration(`, `client.UpdateMerchantConfiguration(`. A `Remote`'s `Ready` checks reachability; any authenticated call proves the credential |
| `ListHostEvents(ctx, HostEventListOptions)` returned a slice; `AcknowledgeHostEvent(ctx, uuid.UUID)` | `client.ListHostEvents(` with `billing.HostEventListParams`, a page; `client.AcknowledgeHostEvents(` takes up to 100 `billing.HostEventID`s and answers each event |
| Merchant webhooks | `client.ListAlertWebhooks(`, `client.CreateAlertWebhook(`, `client.UpdateAlertWebhook(`, `client.DeleteAlertWebhook(` |
| `FleetAnalytics` and `FleetTimeseries` clamped an out-of-range window | `billing.ErrInvalid` |
| `ExportMerchantBilling`, `ImportMerchantBilling` | `client.ExportBillingArchive(`, `client.ImportBillingArchive(` |
| `GetMerchantAPIHost`, `SetMerchantDisplayName`, `RenameMerchant`, `ListUserMerchants` on the in-process Client | Removed. A merchant has no API host; the display name is `billing.ProvisionMerchantParams` at provisioning, then `client.UpdateMerchantConfiguration(`; a rename is the server's `RenameMerchant`; a user's merchants are their merchant groups in the server's AuthKit |
| The server's `AuthenticateUser`, `UserActor`, `CredentialActor`, `OperatorActor`, `CheckRecentSignIn`, `ResolveAuthorizedMerchant`, `ResolveMerchantForGroup`, `HasRootPermission`, `CredentialRefusal`, and its team, API key and federated grant methods | AuthKit's: authenticate with the server's AuthKit Authenticator and ask its answer's `Can` in the merchant's scope (`srv.MerchantScope(ctx, id)`); teams, invitations and API keys are AuthKit's, in the merchant's group, with the roles `server.MerchantRoles` names; a name resolves with `MerchantByName`; `ListUserMerchants` takes the user's id |
| `client.ListWorkerHealth(`, `client.SetAPIHost(`, `client.VerifyAPIHost(` | The server's `ListWorkerHealth` (`openrails workers`, the private listener's `/metrics`); API hosts are removed |
| Merchant API hosts: `client.GetAPIHost(`, `billing.MerchantAPIHost`, `APIHostClaim`, `APIHostRecord`, `MerchantConfigurationState.APIHost`; the server's `SetMerchantAPIHost`, `ClaimMerchantAPIHost`, `VerifyMerchantAPIHost` and its `ErrInvalidAPIHost`, `ErrAPIHostReserved`, `ErrAPIHostTaken`, `ErrAPIHostClaimMissing`, `ErrAPIHostUnproven` | Removed: a request's Host names no merchant. A request names its merchant by its credential |
| `GetUnreadNotificationCount` (merchant) | Removed: count open findings with `client.QueryMetrics(` (`open_findings`) |
| `ListRepairAlerts` | Removed: ledger repairs and worker stalls are critical findings in `client.ListFindings(` |
| `ListActiveMerchantIDs(ctx, limit, offset)` | The server's `ListActiveMerchantIDs` takes a `billing.PageRequest` and returns a page |
| `billing.Page`, `billing.PageOptions`, `billing.UserDirectory`, `billing.UsernameResolver` | Removed |
| Permission `merchant:repair-alerts:read`; the merchant inbox under `merchant:metrics:read` | The inbox and findings are admin reads (`billing.MerchantCustomerRead`, `billing.RootCustomerRead`); worker health is the operator's |
| Permissions `merchant:catalog:read-own`, `merchant:catalog:update-own`; the control plane's `creator` role | Removed with creator-owned catalogs; a teammate or API key holds `viewer`, `support` or `owner` |

## 4. HTTP routes and shapes

No request names its merchant: a merchant route acts on the merchant its
credential is bound to, and the response names it in `OpenRails-Bound-Merchant`.
The `/v2` mirror is gone. Every list is `{data, next_cursor}` with `?cursor=` and
`?limit=`; there is no `offset`, `total` or `has_more`. A `DELETE` answers 204.
Optional members are present as `null`. A request body may hold only declared
fields (`400 unknown_field`), and every error code is in
[error-codes.md](api/error-codes.md).

### Removed with no replacement

- Customer treasury: every `/v1/customers/{customer_id}/…` route.
- Browser checkout: `POST /v1/checkout`, `/v1/checkout/{id}`, `/v1/me/checkout`.
  A browser buys through an order.
- `GET /v1/me/status`, `/v1/me/tier`, `/v1/me/products`: read
  `/v1/me/entitlements` and `/v1/me/subscriptions`.
- PSP cutover and engine takeover routes.
- `GET|PUT /v1/merchant/settings`, `GET /v1/solana/config`,
  `GET /v1/merchant/checkout-options`.
- The purchase-review routes (`/v1/merchant/purchase-reviews`): a purchase under
  review is a finding.
- Merchant API hosts: `GET /v1/admin/api-host`, `api_host` in a configuration
  application and in `GET /v1/merchant/configuration`, and the capability
  `api_host`. A request's Host names no merchant.
- `GET /v1/merchant/repair-alerts`: read `/v1/merchant/notifications`.
- Delegated access tokens (`delegated-access+jwt`), remote-application tokens
  (`remote-application-access+jwt`) and service JWTs, with the
  `delegated_token_*`, `delegated_merchant_unresolved` and
  `delegated_verification_unavailable` codes. Standalone OpenRails accepts RFC
  9068 access tokens from trusted issuers instead: `openrails:self` for
  `/v1/me`, `openrails:merchant` for the merchant API, client credentials for
  machines ([auth](auth.md#trusted-issuers)).
- The standalone control plane and platform: `/v1/merchants` (registration
  and a user's merchants), its invites, `/v1/merchant/name`,
  `/v1/merchant/api-keys`, `/v1/merchant/team…`,
  `/v1/merchant/federated-grants`, the API-host claim and verify, both
  worker-health routes and `/v1/platform/*`. Merchants are registered by the
  manifest or the `server` package's Go methods (`ProvisionMerchant`, …);
  teams and keys are the server's AuthKit's, for a hosted product to build its
  own routes on; the operator's directory, worker health and lockouts are the
  `openrails merchants`, `workers` and `admin-lockouts` commands. A
  self-hosted remote client authenticates with a client-credentials token
  from the merchant's trusted issuer.
- Public `GET /metrics`: the private listener serves it (`private_port`,
  `server.Config.PrivateAddr`).

### Renamed or reshaped

| Before | After |
|---|---|
| `/v1/merchant/…` (staff and configuration routes) | `/v1/admin/…` |
| The standalone server's own credential codes: `sender_proof_required` and its nonce refusal, `access_token_invalid`, `access_token_issuer_unknown`, `access_token_merchant_not_bound`, `insufficient_scope`, `service_credential_invalid`, `service_credential_merchant_unresolved`, `service_credential_resource_scope_denied`, `host_merchant_mismatch`, `merchant_context_mismatch` | A refused credential is `401 authentication_required` (or `credential_expired`, `credential_revoked`) with the auth provider's challenge headers; a credential bound to no merchant here is `403 merchant_unresolved`; a missing permission `403 permission_required` |
| The `OpenRails-Merchant` header (a merchant's slug or `id:<uuid>`) and its codes `merchant_selector_invalid`, `merchant_not_found`, `merchant_binding_mismatch`, `merchant_directory_unavailable` | Removed: the credential binds the merchant, by its group (the merchant id), its declared issuer or the OpenRails object it was issued for; anything else is `403 merchant_unresolved`. A customer of another issuer whose subject is already taken is `403 customer_issuer_mismatch` |
| `/v1/merchant/payment-providers…` | `/v1/merchant/psps`, `/v1/merchant/psps/{id}` (`PATCH` with `expected_revision`, or `{archived: true}`), `/v1/merchant/psps/routing-preview`, `/v1/merchant/psps/refresh`; rails in `GET /v1/config` |
| `POST /v1/merchant/hosted-checkout-sessions`, `POST /v1/me/checkout/sessions`, `/v1/merchant/checkout-sessions…` (engine checkout) | `POST /v1/app/orders` (an unpaid order for a customer), `POST /v1/me/orders` and `POST /v1/me/orders/{id}/pay` |
| `/v1/merchant/credits/deposit`, `/v1/merchant/customers/{id}/credits` | `POST /v1/admin/credit-grants`, `/v1/admin/credit-grants?customer_id=`, `/v1/admin/credit-grants/{id}/revoke` |
| `/v1/merchant/credits/balance`, `/v1/merchant/credit-limit`, `/v1/merchant/trust-level` | `/v1/admin/customers/{customer_id}/balance`; credit limits and trust levels are customer settings, `PATCH /v1/admin/customers/{customer_id}` |
| `/v1/merchant/customers/{id}/credit-transactions` | `/v1/admin/customers/{customer_id}/balance/transactions` |
| `PUT …/spend-delegations:upsert` | Removed |
| `/v1/merchant/admissions/{id}/…` | `POST /v1/app/admissions/{request_id}/capture`, `POST /v1/app/admissions/release`, `POST /v1/app/admissions/extend`, with the host backend's credential |
| `POST /v1/merchant/usage/report`, `/usage/rollup` | `POST /v1/app/usage-events`, `POST /v1/metrics/query` |
| `/v1/merchant/users/{user_id}/…` | `GET /v1/admin/product-access` with `customer_id` and `product_id` |
| `POST /v1/merchant/customers/entitlements:batch`, `…/effective-tier` | `POST /v1/app/entitlements/check` (your backend) or `GET /v1/admin/entitlements` (staff); the effective tier is removed |
| `GET /v1/merchant/customers/{id}` answered the billing profile | It answers the `Customer`: settings, balances, arrears and default cards; subscriptions, payments, cards, entitlements and product access are their own lists |
| `GET /v1/merchant/customers/{id}/payments` | `GET /v1/admin/payments` with `customer_id` |
| `/v1/me/payment-methods/stripe-setup…`, `/v1/me/payment-method-setups…` | `POST /v1/me/payment-methods` with a Stripe `pm_` as `token`; `/v1/me/payment-methods/{id}/confirm` after 3-D Secure |
| `PUT /v1/me/default-payment-method` with `currency` | `PUT /v1/me/default-payment-methods/{currency}`; a subscription's `payment_method_id` is its own card, `null` follows the default |
| `/v1/me/subscriptions/{id}/solana-cancel…`, `/solana-tier-change…` | `/v1/me/subscriptions/{id}/cancel` and `/v1/me/subscriptions/{id}/change` answer a `next_action`; the wallet signs and the same request is repeated with `signature` |
| `/v1/merchant/webhooks…` | `/v1/merchant/alert-webhooks` |
| `GET /v1/catalog/products` found its merchant by the request's Host or the configured merchant | `GET /v1/products` takes `merchant=<merchant id>` beside `keys`, signed in or out: `400 invalid_query` without one id, `404 resource_not_found` for no live merchant |
| `GET /v1/config` carried the merchant's `payment` and `merchant` | It is the deployment's part alone (`capabilities`, `currencies`, `rails`, `captcha`, `network`). `GET /v1/me/config` answers `{payment, merchant}` to a credential bound to the merchant: a customer's token, or a checkout URL's secret on the checkout host |
| `GET` and `POST /v1/orders/{id}/solana-pay` needed a Host or header naming the merchant on a server of many | The order names its merchant; a public route no longer answers `merchant_not_found` |
| `/v1/merchant/catalog/reprice-all-prior-versions`, `/v1/merchant/reprices/batches`, `/v1/merchant/plan-migrations/{id}`, `/v1/admin/reprice-batches`, `/v1/admin/plan-migrations`, `/v1/admin/reprices` | `/v1/catalog/price-migrations`, `/v1/catalog/price-migrations/{id}` |
| `/v1/merchant/catalog/meters/{key}/overrides`; product and price `activate`, `deactivate`, `key` routes | Removed: the meter's rate card prices every customer; `PATCH` the product or price |
| `/v1/merchant/catalog/applications`, `/v1/admin/catalog/applications` | `POST /v1/catalog/apply` |
| `/v1/merchant/catalog/revision`, `/v1/admin/catalog/revision` | Removed: an apply's receipt reports the catalog revision; each object carries its own |
| A creator's catalog at `/v1/catalog/*`; `/v1/merchant/catalogs`, `/v1/merchant/catalogs/{id}`, `/v1/merchant/catalogs/by-owner`; the `OpenRails-Catalog-Owner` header and `owner_subject` | Removed: a merchant has one catalog, at `/v1/catalog/*` |
| `POST /v1/import/billing` | `POST /v1/merchant/billing-import` |
| `/scim/v2/*`; a client-credentials token with scope `scim` | `/v1/app/scim/v2/*`, in the programmatic routes: an application credential, or the merchant's provisioning token |
| `POST /v1/metrics/query`, `GET /v1/metrics/schema`, `GET /v1/metrics/dashboard` behind `AdminRead`; catalog reads, the catalog ask and price migrations behind `AdminRead` and `AdminWrite` | The same routes in the metrics and catalog groups (`billing.MerchantMetricsRead`, `billing.MerchantCatalogManage`, or their `root:` twins); `GET /v1/admin/access` answers what the caller holds |
| `POST /v1/merchant/catalog/copilot/confirm`; an untyped catalog ask | `POST /v1/catalog/ask` answers `{answer, evidence, drafts}`; there is no confirm route |
| A failed model call answered `502 api_error` | `502 model_unavailable` |

### Shapes

- **Checkout payment.** A checkout option carries `psp` (the PSP key) in place
  of `selector`; `payment.rail` is gone. Billing details are one object on the
  session pay body and a card save:
  `billing_details` with `name`, `email`, `phone` and `address` (`line1`,
  `line2`, `city`, `state`, `postal_code`, `country`). The flat `name_on_card`,
  `address1`, `zip`, `last_four`, `card_type` and `expiry_date` are gone.
- **Cards.** One object wherever a card is shown: `card` with `brand`, `last4`,
  `exp_month`, `exp_year`, each nullable. `last_four`, `card_type` and
  `expiry_date` are gone.
- **Saving a card.** `POST /v1/me/payment-methods` takes `psp_id`, a
  `token` or `card`, and `billing_details` (`name`, `email`, `phone`,
  `address`), and answers 201. A save the PSP refuses is 502
  `payment_provider_rejected`.
- **Product entitlements.** `entitlements_spec` maps are replaced by
  `entitlements: ["course:101", "premium"]`. These are opaque strings with no
  content/service classification or per-key duration. Set access duration on
  the price. A customer holds the product's current keys: editing the list
  changes what every holder has. An omitted product update preserves the
  list; `[]` clears it;
  `null`, maps, duplicates and blank keys are refused. Existing paid grants and
  historical accepted purchase durations are preserved by migration.
- **Prices.** One `Price`: `product_id`, `archived`, `access_duration_hours`
  (null: for good), `billing_interval_hours` (null: one-time), `psps`.
  Price `auto_renew` is removed. Existing recurring prices migrate their former
  access duration into the new billing interval; both durations may then differ
  on new price revisions. The public `type`, `recurring.interval`,
  `active` and `providers` are gone.
- **Subscriptions.** Cancel, resume and the payment-method switch answer the
  `Subscription` in the request (200). A merchant tier change on CCBill or
  Solana is 403 `customer_action_required`. A `SubscriptionChange` has no `mode`, `url`
  or `payment`: a redirect is `next_action.url`.
- **`object`.** The `object` member is gone from the public configuration, checkout
  attempts, the currency registry and tier changes.
- **Invokers and metrics.** `invoker_type` is `customer` or `delegated`. The
  metrics dimension `payer` is `customer`; `active_payers` and
  `payers_at_depletion_risk` are `active_customers` and
  `customers_at_depletion_risk`.
- **Error codes.** Every refusal carries a registered code, and a code always
  answers the same status; a client that matched on a status-derived code reads
  [error-codes.md](api/error-codes.md). An expired checkout attempt is 410
  `checkout_attempt_expired`. A refund refusal is 404 `payment_not_found`,
  400 `payment_not_refundable` or 502 `refund_failed`; approving a finding
  without a recommendation is 422 `finding_not_actionable`, and a failed run
  502 `finding_action_failed`. A tier change answers 404 `price_not_found`,
  404 `product_not_found`, 404 `subscription_not_found`, 409
  `subscription_not_active`, 422 `subscription_change_target_inactive`, 409
  `subscription_change_requires_linked_plan`, 400 `subscription_change_unsupported_on_rail`, 409
  `subscription_change_provider_conflict` or 400 `customer_email_required` where it
  answered a bare 400, 404 or 409, and a declined charge is 402 `card_declined`
  or 502 `payment_provider_rejected` with `decline_reason`. A Solana wallet step
  the chain refuses is 400 `solana_transaction_refused`; an unreachable RPC is
  502 `solana_rpc_unavailable`.
- **Catalog.** `catalog_id` is gone from products, product and price requests
  and list filters, the catalog application document and its receipt; there are
  no `cat_` ids.
- **Payments.** `kind`, `status`, `channel`, `psp_id`, `card`, `failure`; a
  refunded charge reads `refunded` or `partially_refunded` in lists too.
- **Balance.** `balance_amount`, `held_amount`, `available_amount`,
  `owed_amount`, `billing_mode`.
- **Entitlements.** `ent_` ids, `starts_at`, `ends_at`; `/v1/me/entitlements` is
  a page.
- **Absent is null.** `Subscription.rail_subscription_id` (an engine-collected
  subscription has none), `CreditGrant.source_id` and `AlertWebhook.name` are
  `null` when absent, no longer `""`.
- **Instants end in `_at`.** Invoices: `period_starts_at`, `period_ends_at`,
  and the list filters `period_starts_after`, `period_starts_before` (were
  `period_from`, `period_to`). Delinquency, its host event and its
  notifications: `overdue_started_at` (was `overdue_since`). The renewal
  notification: `period_starts_at`, `period_ends_at` (were `period_start`,
  `period_end`). Cost observations: `query_starts_at`, `query_ends_at`;
  lifecycle evidence: `provider_lifetime_starts_at`, `provider_lifetime_ends_at`.
- **Notifications.** Unread is `{unread_count}`; marking read answers the
  notification.
- **Ids.** `psp_`, `chk_`, `cgr_`, `txn_`, `ent_`, `pa_`, `rep_`, `rpb_`, `awh_`,
  `ntf_`, `fnd_`, `hev_`. An import names a PSP by `psp_…` id or by key. A Stripe Checkout session opened before the upgrade
  carries `checkout_session_id` in its metadata: let open ones expire first.

### New limits

- An `Idempotency-Key`, or a usage event's `source_id`, over 255 bytes is
  `400 invalid_param`.
- The period invoice pass issues a statement only for customers with ledger
  movement or metered usage in the period; a dormant customer gets no empty
  statement.

Usage and admissions are stored in monthly partitions that are dropped by the
calendar ([data retention](operations.md#data-retention)):

- A usage event's `occurred_at` is within the last 35 days and not in the
  future; its idempotency key is honoured for 35 days.
- A hold's `expires_at` is at most 30 days past its admission, and a spend
  window at most 31 days.
- An admission is readable, capturable and releasable for 61 days.
- Usage is kept 24 months after it was invoiced; subscription status history
  25 months.

## 5. Frontend (`@openrails/billing-ui`)

| Before | After |
|---|---|
| `createHttpSource`, `CheckoutSourceError`, `<Checkout>`, `<CheckoutModal>` | `<OrderCheckout orderId>` pays an order; `<BuyButton>` creates and pays one |
| `checkoutRails`, `CheckoutRailOffer`, `PaymentRailOption` | The order's `payment_options` |
| `getSolanaConfig()` | `getCustomerConfig()`; the network is `payment.solana.network` |
| `getConfig()` and `useConfig()` with `payment` and `merchant` | `getConfig()` and `useConfig()` are the deployment's part; `getCustomerConfig()` and `useCustomerConfig()` read the merchant's `payment` and `merchant` (`GET /v1/me/config`) |
| `listProducts({ keys })`, `useProducts({ keys })`, `<BuyButton product price>`, `<Offers keys>` | Each takes the merchant's id: `listProducts({ merchant, keys })`, `<BuyButton merchant>`, `<Offers merchant keys>` |
| A `getToken` you refresh yourself | `tokenSource({ fetchToken })` keeps the token in memory and replaces it before it expires; `redeemAssertion` redeems a backend's customer assertion |
| `psps` on `AccountBilling` and `PaymentMethodsPanel` | Removed: they read `/v1/me/config` themselves |
| `getStatus()` | Removed: read `/v1/me/entitlements`, and `listSubscriptions()` |
| Offset pages from `listPaymentMethods`, `listPayments`, `listInvoices`, `listSubscriptions` | Cursor pages: pass `cursor`, read `next_cursor`; invoices are usage orders, `listOrders({ origin: "usage" })` |
| `NewCard` with `provider`, `name_on_card` and address fields | `NewCard` with `psp_id` and `billing_details` |
| `PayRequest` with flat `name_on_card`, `zip`, `country`, `last_four`, … | `PayRequest` with `billing_details` |
| `SavedPaymentMethod` with `brand`, `last_four`, `default` | `SavedPaymentMethod` with `card` |
| Solana cancel and tier-change methods | `cancelSubscription` and `changeTier` take an optional `signature` |
| `cancelSubscription`, `resumeSubscription`, `setSubscriptionPaymentMethod` resolved to nothing | Each resolves to the `Subscription` |
| Generated list fields typed `T[] \| null` | `T[]`: the server always writes `[]` |

A page that posted to `/v1/checkout` renders `<Checkout>` on a session instead.
Stripe cards are saved with Elements and paid on the session when the PSP
declares a `publishable_key`; without one, a one-off Stripe price redirects.

## 6. Reading the database

A host that reads OpenRails' tables finds these renamed; the whole schema is in
[`api/schema.txt`](../api/schema.txt). A host that names an index or constraint
(an `ON CONFLICT ON CONSTRAINT`, a hint, a monitoring query) looks its new name
up there.

| Before | After |
|---|---|
| `rail_intents`, `rail_mutation_logs` | `billing.provider_intents`, `billing.provider_mutation_logs` |
| `rail_customer_accounts`, `rail_refresh_watermarks` | `billing.psp_customers`, `billing.psp_refresh_watermarks` |
| `provider_billing_qualifications`, `provider_billing_observations` | Removed with provider operations: a host charges by the hour with a [running charge](running-charges.md) |
| `checkout_sessions` (engine), `hosted_checkout_sessions` | `billing.checkout_attempts`, `billing.orders` |
| `psps.evidence` (jsonb), `psps.replaced_at` | Columns on `billing.psps`: `settings`, `signer`, `credential_refs`, `revision`, `archived_at`; `key` is NOT NULL |
| `subscriptions.user_email`; `cancelled_at`; status `cancelled` | The directory (`Deps.UserInfo`) or `customer_contacts.email`; `canceled_at`; `canceled` |
| `customers.email`, `customers.username`, `customers.blocked` | `billing.customer_contacts` (`email`, `user_name`, `display_name`, `active`), the copy SCIM and token claims keep when OpenRails is not embedded beside the directory; none with `Deps.UserInfo` |
| `payment_methods.is_default`, `initial_transaction_id`, `last_four`, `card_type`, `expiry_date` | `card_brand`, `card_last4`, `card_exp_month`, `card_exp_year`; `psp_id` is NULL for a card a custodian holds |
| Unique payments and subscriptions by `(merchant_id, rail, psp_id, …)` | By `(merchant_id, psp_id, …)`; an `ON CONFLICT` naming the old columns no longer matches |
| Postgres enums `payment_status`, `subscription_status` | `text` with a CHECK |
| `payments.rail` holding `manual` or `admin` | `payments.channel` (`rail`, `manual`); `rail` is null off-channel |
| `catalogs`; `products.catalog_id`, `catalog_applications.catalog_id` | Removed: a merchant has one catalog, `billing.products` keyed by `merchant_id` |
| `merchants.api_host` and its index `merchants_api_host_key`; `merchant_api_host_claims` | Removed: a merchant has no API host |
| `entitlements.start_at`, `end_at` | `starts_at`, `ends_at` |
| Instant columns without `_at`: `invoices.period_from`, `period_to`; `metered_rating_watermarks.period_from`, `rated_through`; `customer_delinquency.overdue_since`; `maintenance_runs.window_since`, `window_until`; `nmi_bulk_checkpoints.since`, `until`; `nmi_history_months.month`; `payment_method_updates.at`; `product_archive_operations.purchased_since`; `provider_intents.claimed_until`; `solana_pay_references.settle_until`, `watch_until`; `solana_subscriptions.last_pulled_period_start`; `subscription_status_transitions.from_paid_through`, `to_paid_through`; `subscription_verifications.since`; `cost_observations.query_start`, `query_end`; `cost_qualifications.provider_lifetime_start`, `provider_lifetime_end` | In the same order: `period_starts_at`, `period_ends_at`; `period_starts_at`, `rated_through_at`; `overdue_started_at`; `window_starts_at`, `window_ends_at`; `window_starts_at`, `window_ends_at`; `month_at`; `occurred_at`; `purchase_window_starts_at`; `lease_expires_at`; `expires_at`, `watch_ends_at`; `last_pulled_period_starts_at`; `from_current_period_ends_at`, `to_current_period_ends_at`; `unverified_at`; `query_starts_at`, `query_ends_at`; `provider_lifetime_starts_at`, `provider_lifetime_ends_at`. Every `timestamptz` column ends in `_at` |
| Session settings `app.merchant_id`, `app.billing_restore_id`, `app.catalog_batch`, `billing.decision`, `openrails.retention` | One namespace: `openrails.merchant_id`, `openrails.billing_restore_id`, `openrails.catalog_batch_merchant_id`, `openrails.subscription_decision`, `openrails.retention_table`. A host that sets the merchant for `billing.current_merchant_id()` sets `openrails.merchant_id` |
| Column defaults: `CURRENT_TIMESTAMP` beside `now()`; defaults on columns every OpenRails writer sets (statuses, amounts and counters, flags, jsonb documents, fact times such as `payments.purchased_at`, `subscriptions.started_at`, `grants.starts_at`) | Ids default to `uuidv7()` and creation times to `now()`; every other column without a default in `api/schema.txt` must be named by a raw `INSERT` (`merchants.status`, `grants.event`, `subscriptions.status`, `collection_policy`, `started_at`, …). `payments.money_movement` keeps its fail-closed `'none'` |
| `products.tier_group`, `subscriptions.tier_group` as `varchar(100)` | `text`; `products_tier_group_check` bounds a group name to 1–100 characters, and an empty name is stored as NULL (no group) |
| `''` for an absent value, with `DEFAULT ''`: `subscriptions.rail_subscription_id`; `payment_methods.rail_customer_ref`, `rail_method_ref`, `stored_credential_recurring_ref`, `stored_credential_unscheduled_ref`, `fingerprint`, `network_token_id`, `network_token_status`, `network_token_par`, `park_reason`; `checkout_sessions.success_url`, `origin`; `grants.source_id`; `custody_migrations.from_rail_customer_ref`, `from_rail_method_ref`, `reason`; `invoker_spend_limits.provenance`; `maintenance_runs.actor`, `mode`; `merchant_webhooks.name`; `notifications.severity`, `title`, `body`, `link`; `product_archive_operations.reason`; `reconciliation_findings.rail`, `openrails_resource_type`; `reprice_batches.fallback_policy`; `subscription_reprices.blocked_reason`; `account_updater_batches.job_ref`, `failure_reason` | NULL when absent, no default; a CHECK refuses `''`. A query that matched `= ''` matches `IS NULL`, and `<> ''` becomes `IS NOT NULL`. `payment_attempts.step` and `nmi_history_months.reason` keep `''` (part of a key) |
| Mixed index and constraint names | One convention, `<table>_<columns>_<suffix>` (`_pkey`, `_key`, `_fkey`, `_check`, `_idx`): 619 names changed, 22 indexes dropped, 18 foreign keys added. `api/schema.txt` lists every name |
| PostgreSQL-generated and placeholder names: `admission_operations_check`, `_check1`, `_check2`, `admission_operations_merchant_id_customer_id_fkey`, `catalog_applications_check`, `product_archive_operations_check`, `product_archive_operations_merchant_id_idempotency_key_key`, `product_archive_operations_merchant_id_product_id_fkey`, `maintenance_runs_x_check`; indexes and checks named after a renamed column | Named by the same convention, e.g. `admission_operations_state_fields_check`, `maintenance_runs_kind_check` |

## Application cache and feature limits

The unused general-purpose cache dependency and public cache interface are removed.
Hosts no longer supply a cache to construct OpenRails. FX quote caching and the
shared rate-limit/captcha state remain separate runtime features.

Built-in AI dashboard authoring and metrics questions are removed and deferred. Delete `llm.ask_enabled` / `LLM_ASK_ENABLED` and the `dashboard-generate` / `metrics-ask` rate-limit overrides from deployment configuration. `Client.GenerateDashboardWidget`, `Client.AskMetrics`, their request/response types and endpoints, the `dashboard_generation` / `metrics_ask` capabilities and the `nl_widgets_enabled` / `ask_enabled` console flags no longer exist. The console cannot generate or refine widget queries, ask an AI metrics question or turn an answer into a saved widget. Ordinary metrics queries/schema, saved/default dashboards, their layout and presentation controls and the distinct catalog copilot remain available. No saved-dashboard data migration is required.

Catalog questions/drafting
use the normal configurable HTTP feature buckets described in
[rate limiting](rate-limiting.md). The former separate per-merchant AI daily quota
is removed. Standalone and embedded HTTP use the same counters (Redis, else
the process's memory); trusted in-process Client operations retain their normal boundary.

Merchant billing archives use the single current format, v1, and preserve arbitrary
application metadata. Regenerate artifacts from discarded draft formats; see
[merchant portability](merchant-portability.md#archive-format-and-metadata).

### Catalog batches and product-local price keys

Catalog documents carry `schema_version` and their changes. Remove `application_id`,
`expected_revision`, and `catalog_version`: the server remembers the canonical
content hash per merchant. Replays remain no-ops after later edits. A new hash
applies atomically; hashes do not determine which unseen batch is newer.
`Routes.MerchantConfig` controls HTTP catalog-write route exposure only; authorized
in-process client calls always work. `MerchantDeclaration.Catalog` is optional startup shorthand,
not a catalog ownership mode.

A price key is scoped to its product: `ListPrices` with `ProductKey` and `Key`
reads a key's prices, and `ListPriceHistory(ctx, priceID, page)` its history
(`GET /v1/catalog/prices/{id}/history`). Checkout and repricing requests
using `price_key` also supply `product_key`; requests using immutable price IDs
keep their current shape.

A price exposes an automatically assigned `revision`, starting at zero within
its product/key. `UpdatePrice` no longer accepts `key`: create a new offer and
archive the old one. Price terms and parent are immutable; product and price
rows cannot be deleted. Existing UUIDs and purchase references survive the
additive migrations. Reusing historical financial terms preserves their original
revision, while price-key history records the new activation. A product's
`revision` advances on actual edits without retaining full product versions.


### Catalog, merchant configuration and customer-support permissions

Catalog management uses `/v1/catalog/`, including `/products`, `/prices`,
`/meters`, `/apply`, `/ask` and `/price-migrations`. Merchant configuration
uses `/v1/merchant/`, including `/psps`, `/configuration`, `/findings`,
`/alert-webhooks`, `/billing-archive`, `/billing-import` and
`/provisioning-tokens`. `PUT /v1/merchant/dashboard` saves the layout; its
read and metric-generation routes use `/v1/metrics/dashboard`. Metrics query,
schema and ask routes use `/v1/metrics/`; their former `/v1/admin/` paths are removed.
The former configuration paths under `/v1/admin/` are not mounted.

Public product lookup is `GET /v1/products?merchant=<merchant id>`. Update
storefront callers: `/v1/catalog/products` now requires catalog management
permission and never serves as an alias for the public product list.

Support staff use `merchant:customer:read` and `merchant:customer:manage`,
with matching `root:` permissions. Update custom roles, API-key roles and
trusted-issuer grants from `billing` to `customer`; old names grant no access.
Catalog and configuration retain `merchant:catalog:manage` and
`merchant:config:manage`; metrics retains `merchant:metrics:read`. A support
role does not gain those operator capabilities. Hosts retain findings by
enabling `RouteGroups.MerchantConfig` and granting configuration authority.
