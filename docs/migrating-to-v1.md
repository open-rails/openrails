# Migrating to v1

v1.0.0 is a hard cut from every v0 release: no aliases, no deprecated paths, no
compatibility fields. A host moves once, at v1.0.0, and from then on
[v1.x only adds](compatibility.md). This page lists what changed, grouped by
what a host does. A row a host never used needs nothing.

Before the code:

- **Database.** v1 installs one new baseline and does not upgrade a v0 schema.
  Start from an empty schema (`openrails.Migrate`) and bring billing facts in
  with `ImportBilling` ([batch import](batch-import.md)). A merchant archive
  written by v0 is not restored.
- **Versions.** Pin `github.com/open-rails/openrails` and
  `@openrails/billing-ui` to the same release; two apps sharing one billing
  schema move together.
- **Vocabulary.** A rail is a gateway kind (`nmi`); a PSP is an account on a
  rail (`mobius`). The payer is a *customer*, the opaque caller an *invoker*,
  and a stopped subscription is `canceled`.

## 1. Configuration

### `openrails.Config` and `Deps`

| Before | After |
|---|---|
| `HTTPConfig.MerchantAdmin`, `MerchantAPI`, `MerchantConfig`, `Catalog`; `Config.MerchantConfigHTTP` | `HTTPConfig.Merchant` mounts the one merchant group; each route's permission gates it |
| `Config.Catalog *billing.CatalogApplyParams`, `billing.ParseCatalogApplicationYAML` | `Config.Catalog` is a `*catalog.Application` from `catalog.ParseApplicationYAML` or `catalog.ParseApplicationJSON` |
| `PSPConfig` as a one-entry map keyed by rail; `CustodianConfig` keyed by kind | `openrails.PSPConfig` with `Rail`, `AccountID`, `Archived`, `Custodian`, `Signer`, `Secrets`, `Settings`; `openrails.CustodianConfig` with `Kind`. `openrails.PSPFromEnv` is unchanged |
| `MerchantDeclaration` with `Profile`, `Invoice`, `BillingPolicies`, `CheckoutRouting` and their `*Config` types | `MerchantDeclaration.Settings` is the `billing.MerchantSettings` document the configuration API reads and applies |
| `Deps.EmailSender` for control-plane mail beside `Config.SendGrid` for billing mail | One sender for both: `Deps.EmailSender` (an `openrails.EmailSender`: `Send` an `openrails.Email`, `CheckHealth`) or `Config.SendGrid` with `APIKey` and `From`. Setting both is refused |
| A hand-written `ALTER … OWNER` pass after `Migrate` for a shared schema | `Config.SchemaOwner`: `Migrate` hands the schema, and a managed River schema, to that existing role |
| `CustomerRoutesConfig{Authenticate: fn}` per profile | `CustomerRoutesConfig{Delegated: true}` and one `Deps.AuthenticateCustomer`, which receives the profile's `Prefix` |
| `Deps.CheckoutCustomer(ctx, string)` | `Deps.CheckoutCustomer` takes a `billing.CustomerID` |
| `Identity.CustomerID`, the `Deps.CustomerFor` result and `DelegatedPrincipal.MerchantID` as strings | `billing.CustomerID` and `billing.MerchantID` |
| `Deps.ProviderCredentials`, `ProviderCredentialSnapshot` | Removed: PSP secrets come from `Config.Merchant` (`PSPConfig.Secrets`) or the secret store |
| No parser for a merchant's YAML | `openrails.ParseMerchantDeclaration` |
| `HTTP.Checkout` needed `Deps.Authenticate` | It needs none: the routes are public or addressed by session id |
| Helper methods on `Config` and its nested types (`IsTestMode`, `SchemaName`, `Validate`, …) | Removed. `openrails.New` validates; compare fields (`cfg.River == openrails.RiverHostOwned`) |
| `Config.Port`, `Config.Host`, `Config.MerchantManifestOverlays`; `koanf` struct tags | Removed: they are the standalone server's own settings. A host that decoded a file into an OpenRails type declares its own struct |
| `AuthConfig.Naming merchant.NamingConfig` | `openrails.NamingConfig`, `openrails.FormerNamesConfig`, `openrails.FormerNamesMode` |
| `openrails.New` ignored `WithAPIKey`, `WithTokenProvider`, `WithCredentialProvider`, `WithHTTPClient` | `openrails.New` returns an error for them; they belong to `openrails.NewRemote` |
| `Close` on a client from `With` closed the engine | It returns an error; close the client `openrails.New` returned |
| `openrails.WithCurrency`; `client.Balance` | Removed: every request names its currency |

### Rate limits and health

| Before | After |
|---|---|
| `rate_limits.default`: 300 a minute per address on every other route | Removed. `rate_limits` takes `checkout`, `payment`, `subscribe` and `webhook`; any other key refuses boot. A per-address ceiling belongs to the proxy |
| `GET /`, `/healthz`, `/readyz`, `/health/ready?verbose=1`, 503 `not_ready` | `/health/live` and `/health/ready`; a failed readiness check is 503 `service_unavailable` and the failing dependency is logged, not answered |
| Capability `hosted_checkout`; route groups `merchant_admin`, `merchant_api`, `merchant_config`, `catalog` | Capability `checkout_sessions`; `/v1/capabilities` lists the groups `checkout`, `customer`, `merchant`, `webhooks` |
| Permissions `merchant:payment-providers:read`, `merchant:payment-providers:update` | `merchant:psps:read`, `merchant:psps:update` |
| CLI flag `--provider-account` | `--psp` |

## 2. YAML manifests

The catalog document is unchanged. The merchant declaration
(`merchant_config.yaml`, `merchants.<slug>` in a standalone manifest) changes
in two places.

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

### Entitlements and access

| Before | After |
|---|---|
| `HasEntitlement(ctx, subject string, key, at)` | `client.HasEntitlement(` with a `billing.CustomerID` |
| `ListActiveEntitlements(ctx, subjects, at)`, `ListEntitlements(ctx, subject, at)` | `client.ListEntitlements(` with `billing.EntitlementListParams` (`CustomerIDs`, `At`), answering `billing.EntitlementLookup` |
| `ListCustomersWithEntitlement` | `client.ListEntitlementCustomers(` |
| `GrantEntitlement`, `RevokeEntitlement` | `client.CreateEntitlement(`, `client.DeleteEntitlement(` |
| `ResolveEffectiveTier`, `CheckEntitlements` | `client.GetEffectiveTier(`; `Tier` is nil when the customer holds none |
| `client.ProductAccess.Check`, `CheckMany`, `List` | `client.CheckProductAccess(`, `client.ListProductAccess(`, `client.CreateProductAccess(`, `client.DeleteProductAccess(` |

Every entitlement now derives from a grant; `source_type` is `purchase`,
`subscription`, `admin` or `grace`.

### Catalog

| Before | After |
|---|---|
| `client.Products.Create`, `Ensure`, `Retrieve`, `RetrieveByKey`, `Update`, `List` | `client.CreateProduct(`, `client.EnsureProduct(`, `client.GetProduct(`, `client.GetProductByKey(`, `client.UpdateProduct(`, `client.ListProducts(` |
| `client.Prices.Create`, `Retrieve`, `RetrieveByKey`, `Update`, `List`, `SetKey` | `client.CreatePrice(`, `client.GetPrice(`, `client.GetPriceByKey(`, `client.UpdatePrice(` (a merge patch: `archived`, `psp_links`), `client.ListPrices(`, `client.ListPriceKeyHistory(` |
| `client.Catalog.Apply`, `Revision` | `client.ApplyCatalog(`, `client.GetCatalogRevision(` |
| `ProductCreateParams`, `PriceCreateParams`, … | `billing.CreateProductParams`, `billing.CreatePriceParams`, `billing.UpdateProductParams`, `billing.UpdatePriceParams` |
| `EnsureUsageMeter`, `GetUsageMeter`, `ListUsageMeters`, `SetDefaultUsageRateCard`, `DeleteDefaultUsageRateCard` | `client.SetMeter(`, `client.GetMeter(`, `client.ListMeters(`, `client.SetMeterRateCard(`, `client.DeleteMeterRateCard(`, and `client.ListRateOverrides(`, `client.SetRateOverride(`, `client.DeleteRateOverride(` for a customer's negotiated price |
| `ListOffersForEntitlements` | `client.ListOffers(` with `billing.OfferListParams` |
| Creator-owned catalogs: `EnsureOwnCatalog`, `EnsureCatalogForOwner`, `GetCatalogForOwner`, `WithOwnCatalog()`, `ForCatalogOwner`, `EnsureCatalog`, `GetCatalog`, `ListCatalogs`; `billing.Catalog`, `billing.CatalogID`, `billing.EnsureCatalogParams`, `billing.CatalogListParams` | Removed: a merchant has one catalog |
| `CatalogID` on `billing.Product`, `CreateProductParams`, `CreatePriceProduct`, `ProductListParams`, `PriceListParams` and `CatalogApplicationReceipt`; `catalog.Application.CatalogID` | Removed with the catalogs |
| Product and price activate and deactivate; price `providers` | `archived` in the update; `psps` |
| `CheckCatalogDrift` | `client.RefreshCatalogDrift(` |
| `ListPurchaseReviews`, `ResolvePurchaseReview`; `ArchiveProductParams` with `Action` and `Window` | A purchase an archive leaves for review is a finding (`finding_id`), resolved with `client.ResolveFinding(`; `billing.ArchiveProductParams` takes `PurchaseAction`, `PurchaseWindowStartsAt` and `WindowSeconds` |

### Checkout

| Before | After |
|---|---|
| `CreateHostedCheckoutSession` | `client.CreateCheckoutSession(` with `billing.CreateCheckoutSessionParams` (`Customer`, `PriceID` or `PriceKey`, `SuccessURL`), answering `billing.CheckoutSessionLink` |
| `CreateCheckoutSession(…{PaymentOptions, Confirm, IdempotencyKey})`, `GetCheckoutSession`, `ConfirmCheckoutSession` | `client.CreateCheckoutAttempt(` (creating it accepts the terms), `client.GetCheckoutAttempt(`, `client.ConfirmCheckoutAttempt(` (Solana) |
| `LookupCheckoutSession`, `GetCheckoutSessionByKey` | Removed: repeat the same request with the same `IdempotencyKey` |
| `ListCheckoutRailOptions`, `ListCheckoutRailOptionsByKey`; `GetCheckoutConfig(ctx)` | `client.GetCheckoutConfig(` with `billing.GetCheckoutConfigParams`; a price in the query fills `options` |
| `billing.CheckoutSessionID`, `cs_` ids, `CheckoutSession` | `billing.CheckoutAttemptID`, `chk_` ids, `billing.CheckoutAttempt` |
| `next_action` as `redirect_to_url`, `solana_qr`, `solana_transaction`, with a top-level `url` | `billing.NextAction` with `type` (`redirect_to_url`, `solana_pay`, `solana_sign_transactions`), `url` and `transactions` |
| `CheckoutPaymentOptions.Card` | Removed: a Client never carries a card |
| `CheckoutPaymentOptions.Rail` (a PSP key or a rail kind) and its flat `NameOnCard`, `Address1`, `Zip`, … | `billing.CheckoutPaymentOptions` with `PSP` (the PSP key; a rail kind is refused) and `BillingDetails` (`billing.BillingDetails`) |
| `CreatePaymentMethodSession`, `CreateSolanaCancelSession`, `CreateSolanaTierChangeSession` | Removed |

### Subscriptions

| Before | After |
|---|---|
| `CancelSubscription`, `ResumeSubscription`, `UpdateSubscriptionPaymentMethod` returned `error` | `client.CancelSubscription(`, `client.ResumeSubscription(` and `client.SetSubscriptionPaymentMethod(` each return the `billing.Subscription` |
| `ChangeTier(ctx, id, idempotencyKey, ChangeTierRequest)` | `client.ChangeTier(` and `client.PreviewTierChange(` take `billing.ChangeTierParams` (`PriceID`, `IdempotencyKey`) |
| `ListSubscriptions(ctx, SubscriptionFilter)` with a total | `client.ListSubscriptions(` with `billing.SubscriptionListParams`, a cursor page, newest first |
| `Subscription.CustomerID`, `ProductID`, `PriceID` as strings; `SubscriptionPrice` | Typed ids; `price` is the catalog `billing.Price` |
| `models.StatusCancelled`, `cancelled_at` | `billing.SubscriptionCanceled`, `canceled_at` |
| `SubscriptionAccess.StartAt`, `EndAt` (`start_at`, `end_at`) | `StartsAt`, `EndsAt` (`starts_at`, `ends_at`) |
| `CutoverProvider`, `GetProviderCutover`, `PreviewProviderCutover`; engine takeover (`TakeOverBilling`, …) | Removed: a provider-owned subscription stays on its PSP until it ends |
| `CancelPlanMigration`; reprice-all routes | `client.CreateRepriceBatch(`, `client.PreviewRepriceBatch(`, `client.ListRepriceBatches(`, `client.GetRepriceBatch(`, `client.CancelRepriceBatch(`; a plan migration is a `plan_change` batch |
| `GetMyInvoice`, `GetMySubscription`, `PayInvoiceNow`, `RetrySubscriptionNow` | Removed from the Client: `/v1/me` is for browsers |

### Credits, usage and admissions

| Before | After |
|---|---|
| `DepositCredits`, `GetDeposit` | `client.CreateCreditGrant(` with `billing.CreateCreditGrantParams`; `client.ListCreditGrants(` by `source_id` answers what a key did |
| Revoke with a body on `DELETE` | `client.RevokeCreditGrant(` |
| `GetCreditAccount` | `client.GetBalance(` answers `billing.Balance` (`balance_amount`, `held_amount`, `available_amount`, `owed_amount`) |
| `Admit`, `AdmitBatch`, `Capture(ctx, id, amount, usage)`, `Release`, `ExtendHold` | `client.Admit(` takes a slice of `billing.AdmitParams` and answers one `billing.AdmissionVerdict` each; `client.GetAdmission(`, `client.CaptureAdmission(` (`billing.CaptureAdmissionParams`, amount required), `client.ReleaseAdmission(`, `client.ExtendAdmission(`. The caller's `request_id` is the id |
| `RecordUsage(RecordUsageInput)`, `UsageRollup`, `ResourceRevenueDaily` | `client.RecordUsage(` with `billing.RecordUsageParams`; `client.GetUsage(`. Resource revenue is removed |
| `SetCustomerSpendDelegations`, `SetCustomerSpendDelegation`, `DeleteCustomerSpendDelegation` | `client.ListSpendDelegations(`, `client.SetSpendDelegations(`, `client.SetSpendDelegation(`, `client.DeleteSpendDelegation(` |
| `InvokerTypePayer` (`invoker_type: "payer"`) | `billing.InvokerTypeCustomer` (`"customer"`) |
| `DeclaredTransaction.AmountCents` | `Amount`, in native units (micros for USD) |
| `DeclaredSubscription.UserEmail`; `DeclaredPaymentMethod.LastFour`, `CardType`, `ExpiryDate`, `InitialTransactionID` | `DeclaredCustomer.Email`; `DeclaredPaymentMethod.Card` (`billing.CardDetails`); `InitialTransactionID` is removed |

### Payments and invoices

| Before | After |
|---|---|
| `ListPayments(ctx, PaymentFilter)` | `client.ListPayments(` with `billing.PaymentListParams`; no status filter |
| `Payment` with `Object`, `Refunded`, `Captured`, string ids | `billing.Payment` with `Kind`, a typed `Status`, `Channel`, `PSPID`, `Card`, `Failure`, and `Refunds` as a slice |
| `ListPaymentMethods(ctx, customerID string, PageOptions)` | `client.ListPaymentMethods(` with a `billing.CustomerID` and `billing.PageRequest` |
| `SetDefaultPaymentMethod` | Removed: a charge names its card; invoices use the per-currency collection card |
| `ListMerchantInvoices`, `GetMerchantInvoice`, `ListInvoicePaymentAttempts`, `RecordInvoicePayment` | `client.ListInvoices(`, `client.GetInvoice(`, `client.ListInvoicePayments(`, `client.CreateInvoicePayment(` |
| `EnsureCustomerInvoiceProfile`, `GetCustomerInvoiceProfile` | `client.SetInvoiceProfile(` with `IfAbsent`; `client.GetInvoiceProfile(` |
| `HasSettledPayment` | `client.GetPaymentSettlementStatus(` |
| `billing.ChannelAdmin` | Removed: a payment's channel is `billing.ChannelRail` or `billing.ChannelManual` |
| `CreateOffChannelPayment` answered `{payment_id, status, entitlements}` | It answers the `billing.Payment`; changed terms under the same transaction id are `billing.ErrIdempotencyKeyReused` |
| `Subscription.RailSubscriptionID`, `SpendDelegation.Provenance`, `CreditGrant.SourceID`, `AlertWebhook.Name` as `string` (`""` when absent) | `*string`, nil when absent |
| `Invoice.PeriodFrom`, `PeriodTo`; `InvoiceListParams.PeriodFrom`, `PeriodTo` | `PeriodStartsAt`, `PeriodEndsAt`; the list filters are `PeriodStartsAfter` (inclusive) and `PeriodStartsBefore` (exclusive) |

### PSPs, configuration and operations

| Before | After |
|---|---|
| `client.PaymentProviders.List`, `Retrieve`, `Upsert`, `Archive`; `RefreshProviders` | `client.ListPSPs(`, `client.GetPSP(`, `client.CreatePSP(`, `client.UpdatePSP(`, `client.ArchivePSP(`, `client.PreviewPSPRouting(`, `client.RefreshPSPs(`, `client.ListRails(` |
| `psp_id` as a UUID string; `DeclarePSP` returned a `uuid.UUID` | `billing.PSPID` (`psp_…` on the wire); `client.DeclarePSP(` returns the `billing.PSP` |
| `GetMerchantSettings`, `SetMerchantSettings`, `Verify`, `client.MerchantConfiguration` | `client.GetMerchantConfiguration(`, `client.ApplyMerchantConfiguration(`. `client.Ready(` checks reachability; any authenticated call proves the credential |
| `ListHostEvents(ctx, HostEventListOptions)` returned a slice; `AcknowledgeHostEvent(ctx, uuid.UUID)` | `client.ListHostEvents(` with `billing.HostEventListParams`, a page; `client.AcknowledgeHostEvent(` takes a `billing.HostEventID` and returns the event |
| Merchant webhooks | `client.ListAlertWebhooks(`, `client.CreateAlertWebhook(`, `client.SetAlertWebhookURL(`, `client.DeleteAlertWebhook(` |
| `FleetAnalytics` and `FleetTimeseries` clamped an out-of-range window | `billing.ErrInvalid` |
| `ExportMerchantBilling`, `ImportMerchantBilling` | `client.ExportBillingArchive(`, `client.ImportBillingArchive(` |
| `GetMerchantAPIHost`, `SetMerchantDisplayName`, `RenameMerchant`, `ListUserMerchants` on the in-process Client | Removed. The API host is `client.GetAPIHost(`; the display name is `billing.ProvisionMerchantParams` at provisioning, then `client.ApplyMerchantConfiguration(`; a rename is `PUT /v1/merchant/name` and a user's merchants `GET /v1/merchants` |
| `GetUnreadNotificationCount` returned `int64` | It returns `billing.UnreadCount` |
| `ListRepairAlerts` | Removed: ledger repairs and worker stalls are critical entries of `client.ListMerchantNotifications(` |
| `ListActiveMerchantIDs(ctx, limit, offset)` | `client.ListActiveMerchantIDs(` takes a `billing.PageRequest` and returns a page |
| `billing.Page`, `billing.PageOptions`, `billing.UserDirectory`, `billing.UsernameResolver` | Removed |
| Permission `merchant:repair-alerts:read`; the merchant inbox under `merchant:metrics:read` | `merchant:operations:read` gates the inbox, findings and worker health |
| Permissions `merchant:catalog:read-own`, `merchant:catalog:update-own`; the control plane's `creator` role | Removed with creator-owned catalogs; a teammate or API key holds `viewer`, `support` or `owner` |

## 4. HTTP routes and shapes

One merchant selector header, `OpenRails-Merchant: <slug>` or `id:<uuid>`; the
`/v2` mirror is gone. Every list is `{data, next_cursor}` with `?cursor=` and
`?limit=`; there is no `offset`, `total` or `has_more`. A `DELETE` answers 204.
Optional members are present as `null`. A request body may hold only declared
fields (`400 unknown_field`), and every error code is in
[error-codes.md](api/error-codes.md).

### Removed with no replacement

- Customer treasury: every `/v1/customers/{customer_id}/…` route.
- Browser checkout: `POST /v1/checkout`, `/v1/checkout/{id}`, `/v1/me/checkout`.
  A browser buys through a checkout session (below).
- `GET /v1/me/status`, `/v1/me/tier`, `/v1/me/products`: read
  `/v1/me/entitlements` and `/v1/me/subscriptions`.
- Default card: `PUT /v1/me/default-payment-method` and its merchant twin.
- PSP cutover and engine takeover routes.
- `GET|PUT /v1/merchant/settings`, `GET /v1/solana/config`,
  `GET /v1/merchant/checkout-options`.
- The purchase-review routes (`/v1/merchant/purchase-reviews`): a purchase under
  review is a finding.
- `api_host` in a configuration application: release a host with
  `PUT /v1/merchant/api-host` and an empty `api_host`.
- `GET /v1/merchant/repair-alerts`: read `/v1/merchant/notifications`.

### Renamed or reshaped

| Before | After |
|---|---|
| `/v1/merchant/payment-providers…` | `/v1/merchant/psps`, `/v1/merchant/psps/{id}` (`PATCH` with `expected_revision`), `/v1/merchant/psps/{id}/archive`, `/v1/merchant/psps/routing-preview`, `/v1/merchant/psps/refresh`; `/v1/merchant/rails` |
| `POST /v1/merchant/hosted-checkout-sessions`, `POST /v1/me/checkout/sessions` | `POST /v1/merchant/checkout-sessions`, `POST /v1/me/checkout-sessions` |
| `/v1/merchant/checkout-sessions…` (engine checkout) | `/v1/merchant/checkout-attempts`, `/v1/merchant/checkout-attempts/{id}`, `/v1/merchant/checkout-attempts/{id}/confirm` |
| `/v1/merchant/credits/deposit`, `/v1/merchant/customers/{id}/credits` | `/v1/merchant/customers/{customer_id}/credit-grants`, `/v1/merchant/customers/{customer_id}/credit-grants/{id}/revoke` |
| `/v1/merchant/credits/balance`, `/v1/merchant/credit-limit`, `/v1/merchant/trust-level` | `/v1/merchant/customers/{customer_id}/balance`, `/v1/merchant/customers/{customer_id}/credit-limit`, `/v1/merchant/customers/{customer_id}/trust-level` |
| `/v1/merchant/customers/{id}/credit-transactions` | `/v1/merchant/customers/{customer_id}/transactions` |
| `PUT …/spend-delegations:upsert` | `/v1/merchant/customers/{customer_id}/spend-delegations` and `/v1/merchant/customers/{customer_id}/spend-delegations/{scope}/{scope_key}` |
| `/v1/merchant/admissions/{id}/…` | `/v1/merchant/admissions/{request_id}` |
| `POST /v1/merchant/usage/report`, `/usage/rollup` | `POST /v1/merchant/usage-events`, `GET /v1/merchant/customers/{customer_id}/usage` |
| `/v1/merchant/users/{user_id}/…` | `/v1/merchant/customers/{customer_id}/product-access` and the checks beneath it |
| `POST /v1/merchant/customers/entitlements:batch`, `…/effective-tier` | `POST /v1/merchant/entitlements/lookup`, `/v1/merchant/customers/{customer_id}/tier` |
| `GET /v1/merchant/customers/{id}` answered the billing profile | It answers the `Customer`; the profile is `/v1/merchant/customers/{customer_id}/billing-profile` |
| `GET /v1/merchant/customers/{id}/payments` | `GET /v1/merchant/payments` with `customer_id` |
| `/v1/me/payment-methods/stripe-setup…` | `/v1/me/payment-method-setups`, `/v1/me/payment-method-setups/{id}/confirm` |
| `/v1/me/subscriptions/{id}/solana-cancel…`, `/solana-tier-change…` | `/v1/me/subscriptions/{id}/cancel` and `/v1/me/subscriptions/{id}/change-tier` answer a `next_action`; the wallet signs and the same request is repeated with `signature` |
| `/v1/merchant/webhooks…` | `/v1/merchant/alert-webhooks` |
| `/v1/merchant/catalog/reprice-all-prior-versions`, `/v1/merchant/reprices/batches`, `/v1/merchant/plan-migrations/{id}` | `/v1/merchant/reprice-batches`, `/v1/merchant/reprice-batches/{id}` |
| `/v1/merchant/catalog/meters/{key}/overrides`; product and price `activate`, `deactivate`, `key` routes | `/v1/merchant/catalog/meters/{key}/rate-overrides`; `PATCH` the product or price |
| A creator's catalog at `/v1/catalog/*`; `/v1/merchant/catalogs`, `/v1/merchant/catalogs/{id}`, `/v1/merchant/catalogs/by-owner`; the `OpenRails-Catalog-Owner` header and `owner_subject` | Removed: a merchant has one catalog, at `/v1/merchant/catalog/*` |
| `POST /v1/import/billing` | `POST /v1/merchant/billing-import` |
| `POST /v1/merchant/catalog/copilot/confirm`; an untyped catalog ask | `POST /v1/merchant/catalog/ask` answers `{answer, evidence, drafts}`; there is no confirm route |
| A failed model call answered `502 api_error` | `502 model_unavailable` |
| `GET /v1/platform/merchants` with `limit` and `offset` | A cursor page (`limit`, `cursor`) |

### Shapes

- **Checkout payment.** A merchant checkout attempt names its PSP as
  `payment.psp` (the PSP key; `payment.rail` is gone and a rail kind is
  refused), and a checkout option carries `psp` in place of `selector`. Billing
  details are one object on the session pay body, the attempt and a card save:
  `billing_details` with `name`, `email`, `phone` and `address` (`line1`,
  `line2`, `city`, `state`, `postal_code`, `country`). The flat `name_on_card`,
  `address1`, `zip`, `last_four`, `card_type` and `expiry_date` are gone.
- **Cards.** One object wherever a card is shown: `card` with `brand`, `last4`,
  `exp_month`, `exp_year`, each nullable. `last_four`, `card_type` and
  `expiry_date` are gone.
- **Saving a card.** `POST /v1/me/payment-methods` takes `psp_id`, a
  `payment_token` or `card`, and `billing_details` (`name`, `email`, `phone`,
  `address`), and answers 201. A save the PSP refuses is 502
  `payment_provider_rejected`.
- **Prices.** One `Price`: `product_id`, `archived`, `access_duration_hours`
  (null: for good), `billing_interval_hours` (null: one-time), `psps`.
  Price `auto_renew` is removed. Existing recurring prices migrate their former
  access duration into the new billing interval; both durations may then differ
  on new price revisions. The public `type`, `recurring.interval`,
  `active` and `providers` are gone.
- **Subscriptions.** Cancel, resume and the payment-method switch answer the
  `Subscription` in the request (200). A merchant tier change on CCBill or
  Solana is 403 `customer_action_required`. A `TierChange` has no `mode`, `url`
  or `payment`: a redirect is `next_action.url`.
- **`object`.** The `object` member is gone from the checkout config, checkout
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
  `subscription_not_active`, 422 `tier_change_target_inactive`, 409
  `tier_change_requires_linked_plan`, 400 `tier_change_unsupported_on_rail`, 409
  `tier_change_provider_conflict` or 400 `customer_email_required` where it
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
  subscription has none), `SpendDelegation.provenance`, `CreditGrant.source_id`
  and `AlertWebhook.name` are `null` when absent, no longer `""`.
- **Instants end in `_at`.** Invoices: `period_starts_at`, `period_ends_at`,
  and the list filters `period_starts_after`, `period_starts_before` (were
  `period_from`, `period_to`). Delinquency, its host event and its
  notifications: `overdue_started_at` (was `overdue_since`). The renewal
  notification: `period_starts_at`, `period_ends_at` (were `period_start`,
  `period_end`). Product archives: `purchase_window_starts_at` (was
  `purchased_since`). Cost observations: `query_starts_at`, `query_ends_at`;
  lifecycle evidence: `provider_lifetime_starts_at`, `provider_lifetime_ends_at`.
- **Notifications.** Unread is `{unread_count}`; marking read answers the
  notification.
- **Ids.** `psp_`, `chk_`, `cgr_`, `txn_`, `ent_`, `pa_`, `rep_`, `rpb_`, `awh_`,
  `ntf_`, `fnd_`, `hev_`, `par_`, `pop_` (payment operations, as in
  `TierChange.operation_id`). An import names a PSP by `psp_…` id or by key. A Stripe Checkout session opened before the upgrade
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
| `createHttpSource`, `CheckoutSourceError` | `client.checkoutSource(id)` is the source of `<Checkout>`, `<CheckoutModal>` and `<CheckoutPage>` |
| `checkoutRails`, `CheckoutRailOffer`, `PaymentRailOption` | The session's `options`; `PaymentOption` |
| `getSolanaConfig()` | `getCheckoutConfig()`; the network is `solana.network` |
| `getStatus()` | Removed: read `/v1/me/entitlements`, and `listSubscriptions()` |
| Offset pages from `listPaymentMethods`, `listPayments`, `listInvoices`, `listSubscriptions` | Cursor pages: pass `cursor`, read `next_cursor` |
| `setDefaultPaymentMethod`, `usePaymentMethods().setDefault`, `defaultCurrency` | `setCollectionPaymentMethod`, `setCollection`, `collectionCurrency` |
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
| `provider_billing_qualifications`, `provider_billing_observations` | `billing.cost_qualifications`, `billing.cost_observations` |
| `checkout_sessions` (engine), `hosted_checkout_sessions` | `billing.checkout_attempts`, `billing.checkout_sessions` |
| `psps.evidence` (jsonb), `psps.replaced_at` | Columns on `billing.psps`: `settings`, `signer`, `credential_refs`, `revision`, `archived_at`; `key` is NOT NULL |
| `subscriptions.user_email`; `cancelled_at`; status `cancelled` | `customers.email`; `canceled_at`; `canceled` |
| `payment_methods.is_default`, `initial_transaction_id`, `last_four`, `card_type`, `expiry_date` | `card_brand`, `card_last4`, `card_exp_month`, `card_exp_year`; `psp_id` is NULL for a card a custodian holds |
| Unique payments and subscriptions by `(merchant_id, rail, psp_id, …)` | By `(merchant_id, psp_id, …)`; an `ON CONFLICT` naming the old columns no longer matches |
| Postgres enums `payment_status`, `subscription_status` | `text` with a CHECK |
| `payments.rail` holding `manual` or `admin` | `payments.channel` (`rail`, `manual`); `rail` is null off-channel |
| `catalogs`; `products.catalog_id`, `catalog_applications.catalog_id` | Removed: a merchant has one catalog, `billing.products` keyed by `merchant_id` |
| `entitlements.start_at`, `end_at` | `starts_at`, `ends_at` |
| Instant columns without `_at`: `invoices.period_from`, `period_to`; `metered_rating_watermarks.period_from`, `rated_through`; `customer_delinquency.overdue_since`; `maintenance_runs.window_since`, `window_until`; `nmi_bulk_checkpoints.since`, `until`; `nmi_history_months.month`; `payment_method_updates.at`; `product_archive_operations.purchased_since`; `provider_intents.claimed_until`; `solana_pay_references.settle_until`, `watch_until`; `solana_subscriptions.last_pulled_period_start`; `subscription_status_transitions.from_paid_through`, `to_paid_through`; `subscription_verifications.since`; `cost_observations.query_start`, `query_end`; `cost_qualifications.provider_lifetime_start`, `provider_lifetime_end` | In the same order: `period_starts_at`, `period_ends_at`; `period_starts_at`, `rated_through_at`; `overdue_started_at`; `window_starts_at`, `window_ends_at`; `window_starts_at`, `window_ends_at`; `month_at`; `occurred_at`; `purchase_window_starts_at`; `lease_expires_at`; `expires_at`, `watch_ends_at`; `last_pulled_period_starts_at`; `from_current_period_ends_at`, `to_current_period_ends_at`; `unverified_at`; `query_starts_at`, `query_ends_at`; `provider_lifetime_starts_at`, `provider_lifetime_ends_at`. Every `timestamptz` column ends in `_at` |
| Session settings `app.merchant_id`, `app.billing_restore_id`, `app.catalog_batch`, `billing.decision`, `openrails.retention` | One namespace: `openrails.merchant_id`, `openrails.billing_restore_id`, `openrails.catalog_batch_merchant_id`, `openrails.subscription_decision`, `openrails.retention_table`. A host that sets the merchant for `billing.current_merchant_id()` sets `openrails.merchant_id` |
| Column defaults: `CURRENT_TIMESTAMP` beside `now()`; defaults on columns every OpenRails writer sets (statuses, amounts and counters, flags, jsonb documents, fact times such as `payments.purchased_at`, `subscriptions.started_at`, `grants.starts_at`) | Ids default to `uuidv7()` and creation times to `now()`; every other column without a default in `api/schema.txt` must be named by a raw `INSERT` (`merchants.status`, `grants.event`, `subscriptions.status`, `collection_policy`, `started_at`, …). `payments.money_movement` keeps its fail-closed `'none'` |
| `products.tier_group`, `subscriptions.tier_group` as `varchar(100)` | `text`; `products_tier_group_check` bounds a group name to 1–100 characters, and an empty name is stored as NULL (no group) |
| `''` for an absent value, with `DEFAULT ''`: `subscriptions.rail_subscription_id`; `payment_methods.rail_customer_ref`, `rail_method_ref`, `stored_credential_recurring_ref`, `stored_credential_unscheduled_ref`, `fingerprint`, `network_token_id`, `network_token_status`, `network_token_par`, `park_reason`; `checkout_sessions.success_url`, `origin`; `grants.source_id`; `custody_migrations.from_rail_customer_ref`, `from_rail_method_ref`, `reason`; `invoker_spend_limits.provenance`; `maintenance_runs.actor`, `mode`; `merchant_webhooks.name`; `notifications.severity`, `title`, `body`, `link`; `product_archive_operations.reason`; `reconciliation_findings.rail`, `openrails_resource_type`; `reprice_batches.fallback_policy`; `subscription_reprices.blocked_reason`; `account_updater_batches.job_ref`, `failure_reason` | NULL when absent, no default; a CHECK refuses `''`. A query that matched `= ''` matches `IS NULL`, and `<> ''` becomes `IS NOT NULL`. `payment_attempts.step` and `nmi_history_months.reason` keep `''` (part of a key) |
| Mixed index and constraint names | One convention, `<table>_<columns>_<suffix>` (`_pkey`, `_key`, `_fkey`, `_check`, `_idx`): 619 names changed, 22 indexes dropped, 18 foreign keys added. `api/schema.txt` lists every name |
| PostgreSQL-generated and placeholder names: `admission_operations_check`, `_check1`, `_check2`, `admission_operations_merchant_id_customer_id_fkey`, `catalog_applications_check`, `product_archive_operations_check`, `product_archive_operations_merchant_id_idempotency_key_key`, `product_archive_operations_merchant_id_product_id_fkey`, `maintenance_runs_x_check`; indexes and checks named after a renamed column | Named by the same convention, e.g. `admission_operations_state_fields_check`, `product_archive_operations_idempotency_key_key`, `maintenance_runs_kind_check`, `invoices_customer_id_period_starts_at_id_idx` |

## Application cache and feature limits

The unused general-purpose cache dependency and public cache interface are removed.
Hosts no longer supply a cache to construct OpenRails. FX quote caching and the
shared rate-limit/captcha state remain separate runtime features.

Metrics questions, catalog questions/drafting and dashboard widget generation
use the normal configurable HTTP feature buckets described in
[rate limiting](rate-limiting.md). The former separate per-merchant AI daily quota
is removed. Standalone and embedded HTTP use the same Redis counters and memory
fallback; trusted in-process Client operations retain their normal boundary.

Merchant billing archives use the single current format, v1, and preserve arbitrary
application metadata. Regenerate artifacts from discarded draft formats; see
[merchant portability](merchant-portability.md#archive-format-and-metadata).

### Catalog batches and product-local price keys

Catalog documents carry `schema_version` and their changes. Remove `application_id`,
`expected_revision`, and `catalog_version`: the server remembers the canonical
content hash per merchant. Replays remain no-ops after later edits. A new hash
applies atomically; hashes do not determine which unseen batch is newer.
`AllowCatalogUpdates` controls HTTP catalog-write route exposure only; authorized
in-process client calls always work. `Config.Catalog` is optional startup shorthand,
not a catalog ownership mode.

`GetPriceByKey(ctx, productKey, priceKey)` and
`ListPriceKeyHistory(ctx, productKey, priceKey, page)` now scope the key to its
product. The HTTP paths are `/v1/merchant/catalog/products/by-key/{product_key}/prices/by-key/{key}`
and its `/history` child. Checkout and repricing requests using `price_key` also
supply `product_key`; requests using immutable price IDs keep their current shape.

A price exposes an automatically assigned `revision`, starting at zero within
its product/key. `UpdatePrice` no longer accepts `key`: create a new offer and
archive the old one. Price terms and parent are immutable; product and price
rows cannot be deleted. Existing UUIDs and purchase references survive the
additive migrations. Reusing historical financial terms preserves their original
revision, while price-key history records the new activation. A product's
`revision` advances on actual edits without retaining full product versions.
