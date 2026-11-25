# The Go client

`*openrails.Client` is one merchant's billing, the same type in every path:
in process from an embedded engine, over HTTP from a standalone server or the
hosted platform. Each staff and programmatic route has one method; this page
lists them by what you do, with the rules each flow depends on. Running the
engine is [embedded.md](embedded.md); running a server is
[standalone.md](standalone.md); the HTTP-only path is
[without the Go client](#without-the-go-client).

Import `github.com/open-rails/openrails` and its types from
`github.com/open-rails/openrails/billing` and `/catalog`. Every method takes
`ctx` first, then typed ids (`billing.CustomerID`, `billing.PriceID`, …) and a
params struct. Amounts are `int64` native units: micros for USD
(`4_990_000` is $4.99), each currency's scale in `catalog.Currencies()` (`Currency.Units()` for one). The
methods are generated from the route catalog, and each one's godoc is its
route's documentation; this page is about the tasks.

## Handles

| Handle | From | Is |
|---|---|---|
| `*Engine` | `openrails.New` | the embedded engine: workers, routes, every declared merchant ([embedded.md](embedded.md)) |
| `*Client` | `Engine.Merchant(id)`, `Remote.Merchant(id)` | one merchant's operations, in process or over HTTP |
| `*LocalClient` | `Engine.LocalMerchant(id)` | what only an engine in the same process can do: the `*Tx` methods that commit with your own rows, and notification adoption |
| `*Remote` | `openrails.NewRemote(url, opts)` | the HTTP transport for one or more merchants, each with its credential |
| `*Gate`, `*Ticket` | `openrails.NewGate` | in-process admission against usage limits ([the Gate](#limits-and-the-gate)) |

- `Engine.Merchant(id)` reads the merchant directory on each call: a merchant
  provisioned while running is served at once, a deleted one is
  `403 merchant_unresolved`. `Engine.DeclaredMerchant(slug)` returns the id of
  a merchant `Config.Merchants` declares.
- `NewRemote(url, RemoteOptions{Credentials: …})` takes the service origin
  without `/v1` and one `RemoteCredential` per merchant id. `Remote.Merchant(id)`
  hands out that merchant's `*Client`; every request sends
  `OpenRails-Expected-Merchant`, so a credential bound to another merchant is
  refused (`412 merchant_mismatch`) before the operation, a first write
  included. `Remote.Ready(ctx)` checks reachability; any authenticated call,
  such as `GetMerchantConfiguration`, proves the credential. `Remote.Close`
  stops its background work.
- `Client.MerchantID()` is the merchant the handle acts on.
- `Client.ActingFor(invoker)` returns a handle whose calls act for a signed-in
  person: their destructive-action limits and holds apply exactly as on their
  own HTTP call. In process, a call that names no one is the host's own work.
  On a remote handle it is an error: there the credential names the caller.
- `Client.Uncached()` returns a handle that skips the read caches
  ([caching](#caching)).
- `RemoteOptions.Logger` is the handle's `*slog.Logger`, under the same rule as
  `Deps.Logger`: nil is `slog.Default()`, read once, never changed.
- A remote handle's `RemoteOptions.Timeout` bounds each request; `HTTPClient`
  replaces the transport. In-process calls run as long as their context allows.

```go guide:remote
remote, err := openrails.NewRemote(apiURL, openrails.RemoteOptions{ // the API URL, without /v1
	Credentials: map[billing.MerchantID]openrails.RemoteCredential{merchant: {APIKey: apiKey}},
	Timeout:     10 * time.Second,
	EntitlementCheckCache: openrails.EntitlementCheckCacheOptions{
		Redis: rdb, // optional: replicas share answers; nil keeps them in memory
	},
})
if err != nil {
	return nil, err
}
return remote.Merchant(merchant) // every call checks the server answered for this merchant
```

A customer is your user's UUID (`billing.CustomerID`). A customer who never touched billing holds nothing; that is never an error, but `GetCustomer` answers `404 customer_not_found`.

## Credentials

Who presents which credential in each mode, and why, is [authentication](authentication.md). Embedded, the handle is in process and needs none; over HTTP, each merchant's `RemoteCredential` carries one of the following.

**Standalone.** The server trusts your issuer for one merchant
(`remote_application` in its manifest, [standalone.md](standalone.md)).
Feed `golang.org/x/oauth2/clientcredentials` into `RemoteCredential.Token`; the
token source caches each token until it expires. The token's `aud` is the
server's resource, its `public_url`, and its permissions are what
your issuer grants the client, within the merchant's owner role:

```go guide:standalone-client
tokens := (&clientcredentials.Config{
	ClientID: backendClient, ClientSecret: s.ClientSecret, TokenURL: s.PublicURL + "/oauth2/token",
	Scopes: []string{"openrails:merchant"}, EndpointParams: url.Values{"resource": {s.OpenRailsURL}},
}).TokenSource(context.WithoutCancel(ctx)) // caches each token until it expires
remote, err := openrails.NewRemote(s.OpenRailsURL, openrails.RemoteOptions{Credentials: map[billing.MerchantID]openrails.RemoteCredential{
	s.Merchant: {Token: func(context.Context) (string, error) {
		t, err := tokens.Token()
		if err != nil {
			return "", err
		}
		return t.AccessToken, nil
	}},
}})
if err != nil {
	return nil, err
}
return remote.Merchant(s.Merchant)
```

A merchant API key (`RemoteCredential.APIKey`) works the same way and carries
its role's permissions. On a standalone server, a person signed in to the console with the server's own account mints it under **Access → API keys**, holding at most their own role there. Grant only what the backend needs:

| Purpose | Permissions |
|---|---|
| read or change the catalog | `merchant:catalog:read`, `merchant:catalog:manage` |
| read or change configuration, imports and archives | `merchant:config:read`, `merchant:config:manage` |
| admit and report usage, limits and running charges (a Gate needs both) | `merchant:usage:read`, `merchant:usage:manage` |
| check entitlements | `merchant:entitlements:read` |
| create invoices | `merchant:invoices:manage` |
| read the event log | `merchant:events:read` |
| read or act on customers | `merchant:customer:read`, `merchant:customer:manage` |
| read metrics | `merchant:metrics:read` |
| push the customer directory over SCIM, or mint its provisioning tokens | `merchant:provisioning:read`, `merchant:provisioning:manage` |

Each permission also has a `root:` name, held in your auth's root scope for
every merchant; `billing.Permissions()` lists them and roles should be built
from the `billing.Permission` constants. The standalone server's declared roles
are `viewer`, `support` and `owner`; custom roles hold any `merchant:`
permissions ([standalone.md](standalone.md)).

**SaaS.** On the hosted platform, create the merchant, its PSPs and an API key in the console; the console documents its own pages and routes. Copy **API URL** from **Integration** and the key from **API keys** (shown once). `GetAdminAccess` (`GET /v1/admin/access`) returns the key's `ScopeID`: the merchant UUID `NewRemote` files the key under. The PSP webhook URLs to give each provider are shown under **Integration**. Keep the key on your server.

**Customer tokens.** A backend whose signing key is registered as the merchant's application (standalone: the manifest's `remote_application` or the console's **Access → Signing keys**; SaaS: **Integration → Customer tokens**) sets `RemoteCredential.Assertion`. `CreateCustomerToken` redeems an RFC 7523 assertion for a short `openrails:self` token (about 15 minutes); `CreateCustomerAssertion` only signs one, for the browser to redeem with its own DPoP key. An API key alone cannot mint customer tokens.

The [hosted example](../examples/hosted) files its key as
`AssertionKey{Issuer, KeyID, Signer, TokenURL}` beside the API key and mints a
token for the signed-in user on request:

```go guide:hosted-token
r.POST("/api/billing/token", authkitgin.Required(ak), func(c *gin.Context) {
	claims, _ := verify.ClaimsFromContext(c.Request.Context())
	customer, err := billing.ParseCustomerID(claims.UserID) // each user is their own customer
	if err != nil {
		c.AbortWithStatus(http.StatusForbidden)
		return
	}
	token, err := bill.CreateCustomerToken(c, billing.CustomerTokenParams{
		CustomerID: customer, Email: claims.Email, EmailVerified: claims.EmailVerified,
	})
	if err != nil {
		c.AbortWithStatus(http.StatusServiceUnavailable)
		return
	}
	c.JSON(http.StatusOK, gin.H{"access_token": token.AccessToken, "expires_in": int(time.Until(token.ExpiresAt).Seconds())})
})
```

In the browser, billing-ui's `tokenSource` keeps the token in memory, fetches the
next one shortly before expiry however many calls wait, and resends a call
answered `401 credential_expired` once.

An issuer of your own can mint RFC 9068 tokens (`typ: at+jwt`) for the billing resource with `openrails:self` and the buyer's UUID as `sub`; include verified contact claims to fill in the customer's contact. Clear tokens on sign-out.

## Customers

| Method | Use it to |
|---|---|
| `ListCustomers` | page or search customers (`search` matches email, username or name); `IDs` reads up to 100 |
| `GetCustomer` | read one: settings, balances, arrears, default cards, contact |
| `UpdateCustomer` | set its credit limits, trust levels, billing policy, invoice profile, low-balance lines, usage-billing overrides and limit capacity; merges by currency |
| `GetBalance` | its balance in one currency (`GetBalanceParams.Currency`): available, accrued, rate and when it runs out |
| `ListBalanceTransactions` | its ledger lines, newest first, each usage posting with its per-meter detail |
| `ListPaymentMethods`, `DeletePaymentMethod` | its saved cards; remove one |
| `GetAccessTenure` | how long it has held access (`GetAccessTenureParams`), for loyalty rules |
| `CreateProvisioningToken`, `ListProvisioningTokens`, `DeleteProvisioningToken` | SCIM tokens for a directory that cannot use your issuer |

`UpdateCustomer` creates a customer not billed yet; a credit limit of `"0"` allows no arrears and `Inherit: true` restores the merchant's default.

```go guide:customers
c, err := shop.GetCustomer(ctx, customer) // settings, balances, contact; 404 if it never billed
if err != nil {
	return err
}
_, err = shop.UpdateCustomer(ctx, c.ID, billing.UpdateCustomerParams{
	CreditLimits: []billing.CreditLimit{{Currency: "USD", Amount: 75_000_000}}, // $75 of arrears
	Limits:       []billing.LimitOverride{{Limit: "weekly", Capacity: 8000}},   // replaces the plan's
})
return err
```

**Contacts.** OpenRails holds each customer's contact (email, username, name), from your directory, and emails its billing messages there. Embedded, `Deps.UserInfo` answers each read (AuthKit's `ak.UserInfo()`, or your own `Get` and `Search`). Without it, your directory pushes users over SCIM 2.0 to `{Prefix}/v1/app/scim/v2` with a provisioning token, or an application credential holding `merchant:provisioning:read` for reads and `merchant:provisioning:manage` for writes; `externalId` is the user's UUID, and the newest write wins. A `DELETE` erases the contact within the merchant's own erasure budget (100 an hour and 1000 a day, floors that rise with the directory), which no refund or cancellation shares. The User leaves the directory and its contact's values go at once either way; the customer's message log, notifications and checkouts' attribution, which cannot be restored, are erased within the budget. Past it the erasure is never refused: the answer is still `204`, the rest waits, and the merchant's erasure [hold](#destructive-holds) opens until a person holding `merchant:provisioning:manage` clears it (`ClearErasureHold`), or a root operator holding `root:admin-rate-limits:unlock`. A create dated before the erasure (`meta.lastModified`) is refused `409`. Without `Deps.UserInfo`, a customer's own credential also fills its contact as it is used: the verified email and username your Authenticator reports for it are kept, undated, so they fill a contact your directory never pushed and never replace one it did, nor an erasure. On a standalone server or the hosted platform, your issuer pushes to the server's directory (`/directory/scim/v2`), and verified contact claims in customer tokens fill in as they are used, including the email your backend asserts in `CreateCustomerToken`. OpenRails emails a customer only at an address that was verified.

## Merchant configuration and PSPs

A merchant's configuration is its display name, settings, alert webhooks, PSPs
and custodians, credentials included. It lives in a file (`Config.Merchants`
embedded, the manifest standalone), read at startup and read-only, or in Vault
when the deployment names a KV mount, where staff and these methods edit it
live ([standalone.md](standalone.md#vault)). PostgreSQL never holds it.

| Method | Use it to |
|---|---|
| `GetMerchantConfiguration` | read name, settings and `revision`; never credentials |
| `UpdateMerchantConfiguration` | merge settings (Vault only); `ExpectedRevision` refuses a concurrent edit (`409 revision_mismatch`) |
| `ListPSPs`, `GetPSP` | the merchant's PSPs, with each credential's state, never its value |
| `CreatePSP` | add a PSP (Vault only); credentials are checked with the provider before anything is stored |
| `UpdatePSP` | rotate credentials, change public keys or custodian, or archive (Vault only); the last active PSP of a rail archives only with `AllowLast` (else `409 psp_last_active`) |
| `PreviewPSPRouting` | which PSP a purchase would use, without charging |
| `ListCustodians`, `SetCustodian` | card custodians (Basis Theory, HyperSwitch) a PSP charges through; `SetCustodian` (`SetCustodianParams`) needs Vault |
| `GetPublicConfig` | the deployment's public `GET /v1/config`: route groups, currencies, rails, captcha, and `usage`, whether the deployment serves usage |
| `GetAdminAccess` | what the credential may reach, and its merchant's `ScopeID` |
| `ListChanges` | the configuration history: each change to a product, price, meter, limit or configuration document, who made it and how (`LogListParams` filters by object and time) |

Every catalog and configuration change is kept in the configuration history, never pruned, and `ListChanges` reads it newest first: a `LogEntry` names the object, the action, the actor (`staff:<id>`, or `file` or `vault` for an edit made outside OpenRails, found when the configuration loads), how it arrived and the fields' values before and after. A credential reads `"changed"`, never its value. A product's entries carry its revision, so `ListChanges` with `ObjectType` `product` and its key answers which revision added a key, and when. A price change's payload adds `move: {price_id, archived}` when its key moves, saying which price it names from then, or none once archived. A move without another price change has its own `move` entry; `ListPriceHistory` reads both.

A merchant document, as `Config.Merchants` reads it and the server's manifest
nests under `merchants.<slug>`, with one PSP on each rail:

```yaml guide:psps
psps:
  nmi-main: # your name for the account; any key
    rail: nmi
    environment: sandbox # live (the default) | test | sandbox
    gateway_id: "1234567" # the NMI dashboard's Gateway ID
    tokenization_key: your-public-collect-js-key
    secrets:
      security_key: your-private-security-key
      webhook_signing_secret: your-webhook-signing-key
  stripe:
    rail: stripe
    environment: test # live | test; the keys' prefixes must agree
    account_id: acct_1Example0Account0
    publishable_key: pk_test_your-key # Elements, Apple Pay and Google Pay
    secrets:
      secret_key: sk_test_your-key # or a restricted rk_test_ key
      webhook_signing_secret: whsec_your-secret
  ccbill:
    rail: ccbill
    environment: sandbox # live | sandbox
    client_accnum: "900000" # quoted: YAML reads 0000 as 0
    client_subacc: "0000"
    secrets:
      salt: your-flexform-salt
      datalink_username: your-datalink-user # optional pair: cancels and reads
      datalink_password: your-datalink-password
  solana:
    rail: solana
    environment: devnet # mainnet | devnet | testnet
    treasury: 9hSR6S7WPtxmTojgo6GG3k4yDPecgJY292j7xrsUGWBu # where every payment lands
```

`openrails.ParseMerchantDocument` reads one, with its required `slug`. Unknown or duplicate fields, several documents, anchors and tags are refused, and so are the retired keys `rail_merchant_accounts`, `provider_accounts` and `api_host`. Keep secrets out of the committed file: Vault, or a file mounted from a secret, supplies them ([standalone.md](standalone.md#configuration-reference)).

**Environments.** Each PSP declares whether it moves real money; omitted, it is the rail's real-money one, so production declares none. NMI: `live`, `test` (an account in test mode), `sandbox` (sandbox.nmi.com). Stripe: `live`, `test` (key prefixes must agree). CCBill: `live`, `sandbox`. Solana: `mainnet`, `devnet`, `testnet`. A merchant may mix them. OpenRails asks the provider before arming a PSP: NMI's `test_mode_status` and Solana's genesis hash on every RPC endpoint, again once that proof is an hour old; Stripe's `api.stripe.com/v1/account` against `account_id`, plus `api.stripe.com/v1/balance` for a test key, once per key and account. A PSP whose answer disagrees stays disarmed: reads work, writes fail before any byte is sent, and the `openrails_psp_posture` probe reports it. CCBill's sandbox has no signal, so its writes are always refused.

**Webhooks.** Each provider posts to `/v1/webhooks/{rail}/{account_id}` (beneath the mount when embedded). `{rail}` is `nmi`, `stripe`, `ccbill` or `basistheory`, never a PSP key, and `{account_id}` is the PSP's account fields joined with `-`. The account resolves the merchant, its own secret verifies the delivery, and the payload's account must agree. Rotating a signing secret keeps the old one for 24 hours; `retire_webhook_overlap` on `UpdatePSP` refuses it at once.

A card PSP (NMI, Stripe, CCBill) may declare `dispute_ratio_limit`, its
acquirer's limit on a month's chargebacks over settled sales as a percentage
(`"0.9%"`): it is monitored like the card networks' programs.

### NMI

`gateway_id` is the dashboard's **Gateway ID**: the merchant account itself, not
the reseller, and not readable from the key, so declare it. `security_key` is
the private API key, `tokenization_key` the public Collect.js key (the browser
tokenizes; OpenRails sees only a token). `card_entry: server` takes card
numbers server side instead. A second live NMI PSP holding a key another one
holds is the same account twice: it stays disarmed and
`consistency.duplicate_gateway_account` names it. In the NMI dashboard, add a
webhook to `/v1/webhooks/nmi/{account_id}` (the Gateway ID) signed with `webhook_signing_secret`,
and enable exactly these 14 events:

- `recurring.subscription.add`, `.update`, `.delete`
- `transaction.sale.success`, `.failure`, `.unknown`
- `transaction.refund.success`, `.failure`
- `transaction.void.success`, `.failure`
- `chargeback.batch.complete`
- `acu.summary.automaticallyupdated`, `.closedaccount`, `.contactcustomer` (with
  NMI's Automatic Card Updater)

NMI sends `chargeback.batch.complete` only for processors that report
chargebacks: confirm yours does, or a PSP's dispute ratio reads 0% with no data
behind it (`ListDisputePrograms` says whether any dispute was reported).

Each NMI decline costs a fee; OpenRails retries renewals sparsely. A legacy NMI
schedule keeps billing at NMI; OpenRails mirrors it and retries its declines.

### Stripe

`account_id` is your `acct_…`; the key is checked against it, and a key of another account never arms. `secret_key` may be a restricted key with read access to its own account (`api.stripe.com/v1/account`; Stripe's refusal, which the posture probe reports, names the permission) and these permissions: `customer_write`, `payment_intent_write`, `setup_intent_write`, `payment_method_write`, `charge_write` (refunds), `subscription_write`, `invoice_write`, `checkout_session_write`, `customer_portal_write`, `webhook_write`, `payment_method_domain_write`, `product_read`, `plan_read` (prices), `dispute_read`, `event_read`, and `balance_read` on a test key.

OpenRails registers the
webhook endpoint itself when the configuration is in Vault, pinned to its Stripe
API version, and keeps its events current. With a file it cannot keep the new
signing secret, so register the endpoint once in the dashboard at
`/v1/webhooks/stripe/{account_id}` and declare its `whsec_…`; until then a
`consistency.stripe_webhook_endpoint` finding names what to register. With a
`publishable_key`, cards are taken in Stripe Elements (one PaymentIntent charges
and saves the card; 3-D Secure in the page) and the payment page offers Apple Pay
and Google Pay. Without one, one-off invoices pay on a Stripe-hosted Checkout
session. Saved Stripe cards are managed in Stripe's billing portal.

### CCBill

The PSP key and rail are both `ccbill`. Declare `client_accnum` and
`client_subacc` (quoted), the FlexForm `salt`, and optionally the DataLink pair,
without which merchant cancels and reads cannot run. CCBill owns the vault and the
rebill schedule. Each price links a FlexForm (`flex_id` and
`recurring_billing_option_id` in the catalog's `provider_prices`); an invoice sells
it by redirect. Point CCBill's webhooks at `/v1/webhooks/ccbill/{account_id}`,
the account being `client_accnum-client_subacc`. CCBill signs nothing:
deliveries are admitted from its published source ranges, and the payload's
account must match. Refunds go through CCBill's own admin (`refund_unsupported`).
A card checkout can keep CCBill back as a fallback ([invoices](#invoices-and-checkout)).

### Solana

Solana is self-custody: `treasury` is the wallet every payment lands in, and OpenRails never holds its key. One-time payments use Solana Pay; subscriptions use the on-chain Subscriptions Delegation Program, pulled each period by the deployment's crankers (`solana.crankers`, or `Config.Solana`), which pay the fees and never receive money. Without a cranker, one-time payments work and recurring Solana prices are refused. `rpc_url`, a secret, is tried before the network's public endpoint. Prices are in USD and quoted in the merchant's accepted tokens at purchase; devnet and testnet fall back to $1.00 parity when the price feed is unavailable.

**Settings** (`billing.MerchantSettings`, flat on the merchant in a file): `profile` (`support_url`, `subscribe_url`, `billing_url`, `from_email`, `logo_url`, `terms_url`, `privacy_url`, `statement_descriptor`), `alert_email`, `card_vault` and `charge_psps` (where new cards are saved and which PSPs take new charges), `psp_routing`, `dunning_policy`, `usage_billing` (per currency: `credit_limit`, `minimum_invoice_amount`, `collection_threshold`, the credit limit when absent, and `delinquency_floor`, one unit of the currency when absent), `billing_policies` and their bindings, arrears (`arrears_grace_days`, `billing_period_boundary`), `reprice_notice_window_days`, `balance_notices`, `checkout` (`embed_origins`, and `billing_fields`, [what a card form asks for](#invoices-and-checkout)), `provider_refund_access`, `fraud_warning_refund`, `delegated_invoker_wasted_spend_limits`, `currencies` and `accounting.timezone` (the IANA zone whose days and months MRR reports and usage history group in, and at whose month ends usage posts; default UTC). Rollups are kept in UTC quarter hours and usage history in UTC hours, so a change regroups every day and month, past ones too; a zone whose offset is not whole hours has its usage days cut at the hour. Posted books keep their effective instants and never change. `from_email` and `alert_email` are each one mailbox, bare or named. Beside the settings, the merchant's `messages` document configures its [customer messages](#customer-messages).

`psp_routing` picks one PSP before a purchase is attempted: ordered rules (`currency`, `product`, `price`, `mode`), first match wins, and its `prefer` list ranks the only PSPs it allows. Without a matching rule: the merchant's `charge_psps` (else stripe, nmi), then ccbill, solana, the first that can serve the price; a provider price always uses its own PSP. Fallback is pre-charge only: a decline never moves to a second processor.

`currencies` declares the merchant's own units (`billing.MerchantCurrency`: credits, API tokens, an in-game currency), at most 32: a `code` of 4 to 12 upper-case letters or digits starting with a letter that is not in OpenRails' registry (three-letter codes are ISO 4217's), a `name`, a `symbol` without a currency sign (the code when omitted) and a `scale` from 0 (whole units, the default) to 6, so `{code: GOLD, name: Gold coins, symbol: 🪙}` counts whole coins. A unit joins the registry for that merchant alone; `GET /v1/me/config` lists it beside `GET /v1/config`'s registry, and a balance in it names it in its `unit`. It is bought, given and spent ([credits](#credits)) but never money: the money settings above refuse it with `merchant_unit_unsupported`, a credit limit or running charge in it is `currency_unsupported`, and a declaration that names a registry currency or breaks these rules is refused like any other invalid settings. Once a balance, price, credit grant or rate card counts a unit, an edit may neither remove it nor change its scale.

## Catalog

The database is the catalog. It changes by **documents** an operator applies and
by **edits** through these methods and the console, usable together. Starting
OpenRails never applies one.

```yaml guide:catalog
schema_version: 1 # the file format
products:
  course-101:
    display_name: Course 101 — Intro to CSS
    entitlements: ["course:101"]
    prices:
      purchase:
        amount: 4.99 USD
        access_duration: forever # permanent access
      rent:
        amount: 1.99 USD
        access_duration: 3 days # access expires 3 days after purchase

  channel-membership:
    display_name: Channel membership
    entitlements: ["channel:membership"]
    prices:
      monthly:
        amount: 10 USD
        access_duration: 30 days
        billing_interval: 30 days
      yearly:
        amount: 99 USD
        access_duration: 365 days
        billing_interval: 365 days
```

Products, prices, meters and limits are maps keyed by stable keys you choose. `entitlements` are plain strings your app gives meaning to; `required_entitlements` must already be held to buy (`403 entitlement_required`); `tier_group` and `tier_rank` make a plan family that upgrades and downgrades within itself; `credit_grant` makes a product top up a balance. A price's `amount` is `9.99 USD`; `billing_interval` (`30 days`, or `1 month` for a calendar month that keeps the billing day) makes it recurring; `access_duration` defaults to the interval, or `forever` for a one-off; `quantity: {min, max}` sells seats; `customer_amount` lets the buyer name the amount; `sell_via` restricts it to `card` or `solana`. A price with a trial is not sold in an invoice today.

```go guide:catalog-apply
products, err := catalog.ReadFile(path)
if err != nil {
	return err
}
receipt, err := shop.ApplyCatalog(ctx, products, billing.ApplyCatalogParams{})
if err != nil {
	return err
}
if _, err := io.WriteString(out, receipt.String()); err != nil {
	return err
}
if n := len(receipt.Conflicts); n > 0 {
	return fmt.Errorf("%s skipped %d object(s) an edit changed", path, n)
}
return nil
```

- **Omission preserves.** Omitted objects and fields are untouched;
  `archived: true` retires. `prune: true` makes the document the whole catalog.
- **Replay by content.** A document that applied whole is recorded by its
  content hash and replays its receipt forever: reapplying an old document never
  rolls anything back.
- **Field ownership.** Each field remembers whether a document or an edit set it
  last. An object whose fields an edit changed differently is skipped and
  reported as a conflict in the receipt (`Conflicts`); agree with the live value,
  drop the field, or force it. `CatalogApplicationReceipt.String` prints it.
- **New terms are new versions.** Changing a price's financial terms makes
  `…:v2` and archives `v1`; subscribers keep their accepted terms. Archiving a
  product or price stops new sales and keeps every buyer's access. Moving
  subscribers is a price migration.

| Method | Use it to |
|---|---|
| `ApplyCatalog` | apply a document (`catalog.ReadFile`, `catalog.ParseApplicationYAML`); `Force` overrides conflicts |
| `ListProducts`, `GetProduct` | products with their current prices; filter by `Keys`, `Entitlements`, `ForSale`, `Archived` |
| `CreateProduct`, `UpdateProduct`, `DeleteProduct` | edit products; `ExpectedRevision` refuses an edit or delete of a product changed since it was read (`409 revision_mismatch`); delete only one nothing ever referred to (else `409 product_in_use`: archive it) |
| `ListPrices`, `GetPrice`, `ListPriceHistory` | prices and the movements of a price key |
| `VerifyPrice` | a price with each linked PSP's copy read and drift reported (`GET /v1/catalog/prices/{id}/verification`) |
| `CreatePrice`, `UpdatePrice` | edit prices; `ProductData` creates the product in the same call |
| `CreateProviderPrice` | link a provider's own price (an NMI plan, a Stripe price, a CCBill FlexForm) read-only |
| `ListMeters`, `GetMeter`, `SetMeter` | meters, with their rate cards |
| `ListLimits`, `GetLimit`, `SetLimit` | usage limits ([limits](#limits-and-the-gate)) |
| `PreviewPriceMigration`, `CreatePriceMigration`, `ListPriceMigrations`, `GetPriceMigration`, `CancelPriceMigration` | move existing subscribers to another price at their next renewal |

```go guide:create-price
return shop.CreatePrice(ctx, billing.CreatePriceParams{
	ProductData: &billing.CreatePriceProduct{Key: "post-" + slug, DisplayName: title},
	Key:         "usd",     // the price key; the id becomes post-<slug>:usd:v1
	UnitAmount:  4_990_000, // $4.99 in USD micros
	Currency:    "USD",
})
```

A price id is `product-key:price-key:v1` (`billing.ParsePriceID`). Edits are merge patches (`catalog.Field`, `catalog.Null` clears); send the `Revision` you read as `ExpectedRevision` to refuse a concurrent change. No public route reads the catalog: your server reads it and puts products in its own pages with `billing.BuyerPrices`. A price migration moves each subscription at its first renewal on or after `effective_at`, skips rises inside the merchant's `reprice_notice_window_days` unless acknowledged, and blocks subscriptions a provider cannot move (CCBill, Solana). Creating one admits each subscriber it counts and each source it archives as one destructive command before reading any, archives the sources, then reaches the subscribers 100 at a time: `preparing` is true until the last; a source it could not archive is in `warnings`; a preview classifies the first 1,000 (`classified`). `openrails billing export` keeps the persisted catalog with ids and history in the merchant's billing archive ([standalone.md](standalone.md#commands)).

## Invoices and checkout

Every bill is an invoice: a purchase, a renewal OpenRails or a provider runs, a usage bill. An invoice's lines are priced and frozen; its payments are the money that moved. `status` is `open`, `processing`, `complete`, `canceled`, `expired`, `void` (a renewal left unowed, or a voided usage invoice) or `uncollectible` (a usage invoice written off); `collection_owner` says who collects (`engine`, `nmi_schedule`, `provider`).

**In the browser** the signed-in customer buys with billing-ui. Your server reads
the products it sells (`ListProducts`, `billing.BuyerPrices`) into its page:

```go guide:buyer-prices
listed, err := bill.ListProducts(c, billing.ProductListParams{Archived: new(false), ForSale: new(true), Keys: keys, PageRequest: billing.PageRequest{Limit: len(keys)}})
if err != nil {
	c.AbortWithStatus(http.StatusServiceUnavailable)
	return
}
// Each product's prices as a buyer sees them, for the page's BuyButtons.
prices := map[string][]billing.Price{} // by product key
for _, product := range listed.Items {
	prices[product.Key] = billing.BuyerPrices(product.Prices)
}
```

```tsx
<BuyButton price={course.prices[0]} signedIn={signedIn}
  onSignInRequired={openSignIn} onPaid={() => refetchAccess()} />
<ProductList products={products} signedIn={signedIn}
  onSignInRequired={openSignIn} onPaid={() => refetchAccess()}
  fallbacks={{ ccbill: { for: ["stripe"], afterFailures: 2 } }} />
```

A button creates the customer's invoice (`POST /v1/me/invoices`) and pays it in a
dialog (`<InvoicePayment>`), or, on a site in `checkout.embed_origins`, frames its
pay URL (`<EmbeddedPayment>`). Asked again for the same lines, OpenRails answers
the customer's open invoice, so a reload or a second click pays one invoice.
`fallbacks` keeps a PSP back until the PSPs it stands in for failed
`afterFailures` times on the invoice. `onPaid` is a hint: confirm access from your
own gate.

**From your server** (and the SaaS path), create the invoice for the customer with
a pay URL and send them there:

```go guide:checkout
// key is made when the buyer decides, stored, and resent on every retry.
invoice, err := shop.CreateInvoice(ctx, key, billing.CreateInvoiceParams{
	CustomerID: customer,
	Lines:      []billing.InvoiceLineParams{{PriceID: price}},
	PayURL: &billing.InvoicePayURLParams{
		SuccessURL: "https://shop.example/thanks?invoice={INVOICE_ID}",
		CancelURL:  new("https://shop.example/cart"),
	},
})
if err != nil {
	return "", err
}
return *invoice.PayURL, nil // redirect the buyer there
```

**Checkout facts.** Pass where the customer came from as `Attribution` (`billing.InvoiceAttribution`: source, medium, campaign, term and content as your `utm_*`, referrer, affiliate; term and content are kept for your own reads, never metrics dimensions) on `CreateInvoice`, or as billing-ui's `attribution` prop on `BuyButton`, `ProductList` and `Cart`. Attribution never refuses an invoice: a field over 100 bytes, unprintable, or carrying an email address or card number is left out (null on the invoice); a referrer keeps only its host. Browser-supplied attribution is unverified: set an affiliate you pay on invoices your server creates. Staff and application reads of a checkout carry `checkout`: `customer_type` (`new`, `returning`, or `reactivated` for a former subscriber with no subscription now), `purchase_number` (the customer's paid purchases before it, plus one), `opened_at` (the payment form first loaded), `submitted_at` (the first pay request), `billing_country`, `ip_country` (`CF-IPCountry`, read only from Cloudflare's published ranges; the address is never stored), `card_country` (null until cards' issuing countries are recorded), `country` with its `country_source`, `language` and `attribution`. The customer's own reads show `checkout: null`. billing-ui sends its `BillingProvider` `locale` (or the client's `language`) as `Accept-Language`, which the invoice records. A renewal names the checkout that bought its subscription (`renewal.checkout_invoice_id`).

Generate the idempotency key when the buyer decides to buy, store it with the
cart, and resend it on every retry: the same key recovers the same invoice and URL;
a new key is a new purchase (a duplicate subscription or permanent product is
refused, a timed pass or credit pack sells again). Fulfil on the `invoice.completed`
event ([events](#events)) or a read of the invoice, never on billing-ui's `onPaid`
or the redirect to `success_url`.

- The pay URL is `/pay/<invoice>#<secret>` on the deployment's **checkout origin**,
  which must differ from the API, issuer and frontend origins: the page runs the
  PSPs' scripts. Without one, an invoice asking for a pay URL is
  `400 pay_url_unavailable` ([embedded.md](embedded.md),
  [standalone.md](standalone.md#configuration-reference)).
- `success_url` and `cancel_url` may name any https site; `{INVOICE_ID}` becomes the
  invoice's id. The URL lasts an hour (30 minutes to 24 hours with `ExpiresAt`);
  an invoice with no expiry of its own expires with it. Ordinary invoice reads show
  `pay_url: null`.
- `SavedPaymentMethods` offers the customer's saved cards; set it only when your
  site signed them in. `Embedded: true` makes a URL your registered sites frame.
- The page takes cards in the PSP's own fields, runs a bank's challenge in place,
  offers Apple Pay and Google Pay with a Stripe `publishable_key`, and stops after
  ten declined cards (`409 pay_url_attempts_exhausted`).

Paying in the customer's own API (`POST /v1/me/invoices` with `payment`, or `/{id}/pay`) takes exactly one of a saved card `{payment_method_id}`, a new one `{token, psp_id}`, a Solana wallet's token `{token_symbol}` or a hosted page's `{return_url}`; a card charge naming no card is `400 payment_method_required`. One provider request charges and saves a new card; a declined card is not saved. In the EEA and UK a new card is kept for later only with `reusable: true`, the customer's unticked opt-in. `requires_action` carries `next_action`: `authenticate` (run the PSP's script, then `confirm`), `redirect_to_url` (CCBill, Stripe-hosted Checkout), or `solana_pay` (a QR code or wallet link; the wallet signs a transaction OpenRails builds, and the invoice completes when it lands). A decline is `402` with the invoice open again for another card.

**Billing fields.** As Stripe does, a new card's form asks for the billing country always and the postal code only in the US, where it is required. The merchant's `checkout.billing_fields` changes that: `country` is `required` (the default), `optional` or `hidden`; `postal_code` is `auto` (the default: the US only), `required`, `optional` or `hidden`. OpenRails enforces them on a new NMI or platform-vault card, saved (`POST /v1/me/payment-methods`) or paid with (`payment.billing_details`): a required detail that is missing is `400 billing_details_required`, its `param` naming it, and a hidden one sent is dropped before the provider sees it. Stripe's Payment Element collects a Stripe card's address by Stripe's own rules. billing-ui's card forms read the fields from `GET /v1/me/config` (`payment.billing_fields`).

| Method | Use it to |
|---|---|
| `PreviewInvoice` | price lines, with refusals and the `payment_options` that can take them |
| `CreateInvoice` | an unpaid invoice for a customer, open 30 days unless `ExpiresAt`; only that customer pays it, signed in or through `pay_url`; takes `idempotencyKey` |
| `GetInvoice`, `ListInvoices` | read invoices: filter by customer, price, subscription, `origin` (`customer`, `merchant`, `subscription`, `usage`), status, overdue |
| `GetInvoicePaymentAttempt` | the outcome of one pay attempt by its idempotency key, after a lost response |
| `CancelInvoice` | withdraw an open invoice `CreateInvoice` made (settle an unresolved payment first) |
| `WaiveInvoiceBalance` | void a usage invoice and write off what it owes |
| `MarkInvoiceUncollectible` | stop collecting and keep the debt |
| `ListRenewals`, `GetRenewal` | renewal bills OpenRails runs |

### Content-Security-Policy

The payment page sends its own policy: its scripts, the PSPs' field origins, the captcha provider's, and `frame-ancestors` naming only the merchant's `checkout.embed_origins` (`'none'` without them). It needs nothing from you.

A page of yours that renders billing-ui runs the same PSP scripts in your origin. Beside your own sources, its policy allows the PSPs your merchants use:

| Directive | Sources |
|---|---|
| `script-src` | Stripe `https://js.stripe.com https://*.js.stripe.com`; NMI Collect.js `https://secure.nmi.com https://secure.networkmerchants.com`; the Basis Theory card vault `https://*.basistheory.com` |
| `frame-src` | the same, Stripe's `https://hooks.stripe.com` (a bank's challenge), and the checkout origin when the page frames a pay URL (`<EmbeddedPayment>`) |
| `connect-src` | the billing client's `baseUrl` origin, `https://api.stripe.com`, the NMI origins, `https://*.basistheory.com` |
| `style-src`, `font-src` | the NMI origins: Collect.js styles its fields |
| `frame-ancestors` | `'self'`: no other site frames a page that takes cards |

A page that frames a pay URL and sends `Permissions-Policy` lists the checkout origin under `payment`, so Apple Pay and Google Pay work in the frame.

## Subscriptions

A subscription's `status` is its billing lifecycle (`active`, `past_due`,
`awaiting_method`, `pending`, `unverified`, `canceled`), never its access: an
active one can have an expired short window, a canceled one can keep paid
access. Check entitlements. `unverified` means OpenRails could not confirm the
provider's state; it keeps access until evidence arrives.

| Method | Use it to |
|---|---|
| `ListSubscriptions`, `GetSubscription` | read them, with price, access, scheduled change, card and recovery; `Dunning: true` lists open renewal cases |
| `GetSubscriptionHistory` | its status and billing changes |
| `CancelSubscription` | stop renewal at period end; `RevokeAccess` ends access now; `AccountDeletion` cancels locally and holds the provider stop |
| `ResumeSubscription` | undo a scheduled cancel where the rail can (CCBill cannot) |
| `ListSubscriptionChangeOptions`, `PreviewSubscriptionChange` | the tiers it can move to, priced |
| `ChangeSubscription` | change tier or seats; takes `idempotencyKey`; staff changes never charge and take effect at the next renewal |
| `SetSubscriptionPaymentMethod` | its own card, or none to follow the customer's default |

```go guide:subscriptions
page, err := shop.ListSubscriptions(ctx, billing.SubscriptionListParams{
	CustomerID: customer, Status: billing.SubscriptionActive,
})
if err != nil {
	return err
}
for _, sub := range page.Items {
	_, err := shop.CancelSubscription(ctx, sub.ID, billing.CancelSubscriptionParams{
		Reason: "customer request", // RevokeAccess: true would also end access now
	})
	if err != nil {
		return err
	}
}
return nil
```

- **Changes.** An upgrade or more seats charge now (the new price less unused
  credit); a downgrade or fewer seats apply at period end as its
  `scheduled_change`. Another change while one is scheduled is
  `409 subscription_change_already_scheduled`. An NMI schedule keeps its billing
  date, so only a price of the same cycle qualifies
  (`409 subscription_change_cadence_unsupported`), and a named NMI plan needs a
  linked plan on the target (`409 subscription_change_requires_linked_plan`).
  CCBill upgrades redirect; downgrades are refused.
- **Cancels.** A staff cancel of an NMI or CCBill schedule is refused `409 provider_cancel_held` while the merchant's destructive actions are disarmed: OpenRails will not cancel locally while the provider keeps charging. A customer's own cancel is never held; only the customer budget refuses it (`429 rate_limit_exceeded`). A renewal in flight refuses a cancel (`409 payment_in_progress`). A Solana cancel needs no signature.
- **Dunning.** OpenRails retries the renewals it collects on a schedule sized to
  the cycle (a monthly one on days 3, 5, 7 and 10; the fifth failure cancels);
  `dunning_policy` replaces it. Stripe- and CCBill-billed subscriptions use the
  provider's own retries. A declined renewal is an open invoice the customer pays
  with a saved card (`POST /v1/me/subscriptions/{id}/retry-now` for a
  provider-scheduled one); a retry of a card past its limit is refused
  `payment_not_retryable` before any provider call.

## Payments and refunds

| Method | Use it to |
|---|---|
| `ListPayments`, `GetPayment` | payments and refunds, with their invoice |
| `ListPaymentAttempts`, `GetPaymentAttempt` | each provider attempt, with its decline |
| `ListDisputes`, `GetDispute` | each provider dispute (inquiry, chargeback or prevented), with its network, reason family, code, dates and outcome |
| `RefundPayment` | refund through the PSP, in full or in part; `RefundReason` says why (default `requested_by_customer`), `Reason` in the merchant's words; `RevokeAccess` ends what the payment bought; takes `idempotencyKey` |
| `RecordExternalPayment` | money received outside OpenRails for one invoice, idempotent on its transaction id: part or all of what a usage invoice owes, or another invoice's total (else `409 payment_exceeds_due`); an invoice with a recurring line is refused |

```go guide:refund
// key: one per refund decision; a retry with it refunds once
return shop.RefundPayment(ctx, key, payment, billing.RefundPaymentParams{
	Full:         true,
	RefundReason: billing.RefundRequestedByCustomer,
	Reason:       "cancelled within the first day",
	RevokeAccess: true,
})
```

A refund through CCBill or Solana is `refund_unsupported`. A chargeback revokes the access its payment bought; a refund does with `RevokeAccess`, and one made in the provider's own dashboard follows `provider_refund_access`. Staff never start a card charge.

**Disputes.** A chargeback ends what its charge paid for and the card is never charged again (`disputed`): the customer's other subscriptions on it wait for another card, or end at period end where a provider bills them. An inquiry moves no money and ends nothing. A Stripe early fraud warning refunds its charge in full and ends what it paid for, unless the merchant setting `fraud_warning_refund` is `record`. Each chargeback raises a `life.disputes.customer_review` finding.

**Saved cards.** A method is `{id, rail, psp_id, status, card, billing_details, health, mandates, default_currencies, …}`; only `active` ones are charged. Renewals and usage collection charge the subscription's own card, else the customer's default card for that currency; with neither, or with no active recurring agreement, the renewal waits for the customer (`awaiting_method`) and never charges another card. A charge agreement is the customer's permission to charge a card without them present (`recurring` for one subscription, `unscheduled` for usage); the customer revokes one at `POST /v1/me/agreements/{id}/revoke`, which cancels the subscription at period end or stops usage collection, and the merchant gets `mandate.revoked`. A card reissued under another brand waits for the customer's consent before it is charged again. A HyperSwitch-custodied card is saved in two steps: `POST /v1/me/payment-methods` answers a `capture` next action, then `/{id}/confirm` with the custodian's token.

## Credits

A product with a `credit_grant` tops up a currency balance that usage draws down.
It is money, not a counter: gates call it the built-in limit `balance`.

```yaml guide:gate-catalog
schema_version: 1
meters:
  work: {event_type: work, value_property: units, aggregation: sum}
limits:
  hourly:
    unit: units
    reset: {kind: rolling, window_seconds: 3600}
    overrun: admit_while_positive
    counts: [{meter: work}] # adds quantity × weight ÷ per; both default to 1
  monthly:
    unit: units
    reset: {kind: billing_period}
    counts: [{meter: work}]
products:
  starter: {display_name: Starter, limits: {hourly: "1000", monthly: "100000"}}
  usage:
    display_name: Usage
    rate_cards: [{meter: work, price: {model: per_unit, currency: USD, per_unit: {unit_amount: "100"}}}]
  credits-10:
    display_name: $10 of usage
    credit_grant: {currency: USD, amount: "10000000"} # a balance usage draws down; expires after 365 days
    prices:
      purchase: {amount: 10 USD}
```

`credits-10` above is a pack: each purchase is one credit lot, its amount and
expiry (`expires_after_days`, default 365) fixed at purchase. `from_payment: true`
grants what was paid, with a `customer_amount` price whose amount the buyer names
on the invoice line. A recurring price with a `credit_grant` grants its credit after
each paid renewal. A voluntary refund needs the lot unused and unheld.

A `credit_grant` may count one of the merchant's own units (`settings.currencies`) instead of money: it then names its `amount` (never `from_payment`), and every price of the product buys that many units, in whatever currency the price is in, so a $50 pack of 55 credits is a product beside a $20 pack of 20. Each lot records its units, what was paid and in what currency. `CreateCreditGrants` gives units, and a recurring price's `credit_grant` in a unit is a plan's allowance, delivered each paid cycle. Units are spent by usage a rate card prices in the unit and by a one-off price in it: `POST /v1/me/invoices` (with an `Idempotency-Key` and no `payment`) or `CreateInvoice` pays such an invoice from the customer's balance as it is created, soonest-expiring lot first, or refuses it with `insufficient_credits`. A unit is never owed: a gate decides a request in a unit against the unit balance, and views and lease syncs take the units the merchant declares. Its posting takes what the lots hold and forgives the rest, shown as `usage_charged` with `charge: forgiven`. `GET /v1/me/usage` reads usage in a unit at its price. Refunding a pack's payment takes the lot's share of its units, those left first. The amount shorthand (`50 GOLD`) cannot know a unit's scale: a price in a unit names `currency` and `unit_amount` in native units.

A bought lot's price is deferred revenue, recognised at the lot's rate (paid ÷ credits, a pack's bonus included) as its credit is spent; what lapses or is revoked is breakage, and a reinstatement reverses it. Granted credit is promotional: spending it counts as usage revenue offset by promotional credit. See [credits](architecture/credits.md).

| Method | Use it to |
|---|---|
| `CreateCreditGrants` | fund balances directly, idempotent per customer on `SourceID` (`Source` is a label); `FundingSource` marks a grant, a promotion or an import |
| `ListCreditGrants`, `GetCreditGrant` | the lots: what remains, what was spent, expired, revoked or refunded, and what was paid for each |
| `RevokeCreditGrant` | take back what remains of a lot; a retry under the same idempotency key replays, even after a reinstatement |
| `ReinstateCreditGrant` | reverse a revocation: what it took returns to the balance; a retry under the same idempotency key replays, even after a later revocation |

```go guide:credit-grant
_, err := shop.CreateCreditGrants(ctx, []billing.CreateCreditGrantParams{{
	CustomerID: customer,
	Currency:   "USD",
	Amount:     5_000_000, // $5
	Source:     "support", // with SourceID, the grant's identity: a retry grants nothing again
	SourceID:   ticket,
}})
return err
```

## Entitlements and access

An entitlement is a plain string a product grants. A customer holds a key at an
instant when a product they hold then grants it; nothing stores a customer's
keys. Editing a product's keys changes every holder at once. Staff grant
products, never keys: a key with no product for sale gets a product with no live
price.

| Method | Use it to |
|---|---|
| `CheckEntitlements` | which of up to 100 keys (and 10 prefixes) a customer holds now or `At` an instant, with seat quantities |
| `ListEntitlements` | keys held, by customer or by key: a page of users in one call |
| `ListProductAccess` | the windows of products a customer holds, bought, subscribed or granted |
| `CreateProductAccess` | grant up to 100 products, all or none, for `Hours`, until `EndsAt`, or indefinitely; takes `idempotencyKey` |
| `RevokeProductAccess` | revoke one window, with a reason |

Gate the feature where you serve it:

```go guide:entitlement-check
claims, signedIn := verify.ClaimsFromContext(c.Request.Context()) // set by authkitgin.Optional
if !signedIn {
	return map[string]bool{}, nil
}
customer, err := billing.ParseCustomerID(claims.UserID)
if err != nil {
	return nil, err
}
check, err := bill.CheckEntitlements(c, billing.CheckEntitlementsParams{CustomerID: customer, Entitlements: entitlements})
if err != nil {
	return nil, err
}
return check.Entitlements, nil
```

```go guide:grant-access
_, err := shop.CreateProductAccess(ctx, key, []billing.CreateProductAccessParams{{
	CustomerID: customer,
	ProductID:  product,
	Hours:      new(7 * 24), // after its latest live window; nil with no EndsAt is indefinite
	Reason:     billing.GrantReasonComp,
}})
return err
```

Never infer access from subscription status. `subscription_canceled` means renewal stopped while paid access remains; `premium_ended` means access closed. A remote handle with an API key caches allowed keys ([caching](#caching)); a check for the present can ask for `IncludeValidity` (`ValidUntil`, the next scheduled change).

## Limits and the Gate

The catalog declares **limits**: what a customer may use per window, in a named
unit or a currency, apart from what it pays. Products give capacity (in
`gate-catalog` above, `starter` gives `hourly` and `monthly`); a customer's is the
sum over its live products, and one with no capacity is not limited. `reset.kind`
is `fixed`, `billing_period`, `session`, `rolling`, `rate` or `lifetime`;
`overrun: admit_while_positive` lets the request that crosses the limit finish,
`require_estimate` admits only an estimate that fits. Late usage settles in full,
never clamped.

A named-unit limit counts meters: each of its `counts` (`catalog.LimitCount`) adds an event's quantity × `weight` ÷ `per`, rounded half up, when its meter reads the event and its optional `filter` admits the event's dimensions; `weight` and `per` default to 1, so `counts: [{meter: tokens}]` counts tokens one to one. Several counts weight a mix: `[{meter: input-tokens, per: 1000}, {meter: output-tokens, weight: 4, per: 1000}, {meter: output-tokens, weight: 6, per: 1000, filter: {model: [large]}}]` counts thousands of input tokens, output at four times that and a large model's output at ten. A currency limit counts the rated amount and has no counts; rate cards price money only.

Refusals are verdicts, not errors:

| Deny code | Means |
|---|---|
| `limit_reached` | the limit is spent until `resets_at` |
| `insufficient_balance`, `insufficient_credit`, `outstanding_cap_reached` | the built-in `balance` limit: prepaid balance, credit line, unpaid arrears |
| `budget_exceeded`, `accrual_rate_cap_reached` | a billing policy's spend window or accrual rate |
| `delinquent_unpaid_invoice` | an unpaid usage bill past grace |
| `failure_rate_limited` | a delegated invoker's failed usage passed its limit |
| `delegated_spend_not_allowed` | only the customer's own credential spends its balance |

**The Gate** admits in your process over any `*Client` whose credential holds
`merchant:usage:manage` and `merchant:usage:read`. Each sync of its lease answers
views: where each customer it serves stands on its limits, reserving nothing. A
decision takes the request's cost from the view, less what the gate has in
flight, with no network call; nothing is decided or held on OpenRails. A
customer with no view waits for one `SyncLeases` round trip. Nothing tunes it.

A limit may be overshot by a small, bounded amount: what other gates (and batch jobs) admit for the customer within about four sync intervals, plus one request per gate. While OpenRails is unreachable a gate keeps deciding on the views it had when the outage began, spending at most its share of what each had left (the remaining under the request's credit line and caps, less what running charges drain, divided by the merchant's live gates), so the gates together never spend more than was left; a request no share covers, or for a customer with no such view, is `openrails.ErrUnavailable`. A gate with nothing to do stops syncing and its lease lapses; its next request is decided on its last view, and the next waits for a sync. A request whose view OpenRails refuses (a currency or invoker type it does not take, or a billing policy it cannot resolve now) gets that error alone; the gate's other customers and usage go on.

```go guide:gate-new
gate, err := openrails.NewGate(client, openrails.GateDeps{})
if err != nil {
	log.Fatal(err)
}
```

```go guide:gate-admit
ticket, err := gate.Admit(r.Context(), billing.AdmitParams{
	RequestID: uuid.Must(uuid.NewV7()).String(), CustomerID: customer, Invoker: customer.String(), Currency: "USD",
	Usage: &billing.UsageEstimate{EventType: "work", Dimensions: map[string]int64{"units": estimate}},
})
if err != nil {
	http.Error(w, err.Error(), status(err))
	return
}
if refusal := ticket.Refusal(); refusal != nil {
	if refusal.RetryAfterSeconds != nil {
		w.Header().Set("Retry-After", strconv.FormatInt(*refusal.RetryAfterSeconds, 10))
	} else if refusal.ResetsAt != nil {
		w.Header().Set("Retry-After", strconv.FormatInt(int64(time.Until(*refusal.ResetsAt).Seconds())+1, 10))
	}
	w.WriteHeader(http.StatusTooManyRequests)
	_ = json.NewEncoder(w).Encode(refusal)
	return
}
used, err := doWork(r.Context(), estimate)
if err != nil {
	_ = ticket.Release(r.Context())
	http.Error(w, err.Error(), http.StatusInternalServerError)
	return
}
if err := ticket.Capture(r.Context(), billing.CaptureUsage{Dimensions: map[string]int64{"units": used}}); err != nil {
	log.Print(err)
}
```

| Method | Use it to |
|---|---|
| `NewGate(client, GateDeps{…})` | start a gate; a handle made with `ActingFor` is refused |
| `Gate.Admit` | admit a request's estimate; returns a `*Ticket` |
| `Ticket.Allowed`, `Ticket.Refusal` | the verdict, and the `billing.Refusal` with its deny code, limit and reset |
| `Ticket.Capture` | report what the request used, sent on the next sync and priced exactly |
| `Ticket.Release` | give the request's take back when the work failed |
| `Gate.Consume` | admit a known cost and capture it at once |
| `Gate.Record` | report usage no ticket admitted |
| `Gate.ListCustomerLimits` | a customer's lines less usage not yet sent, for display |
| `Gate.Flush` | sync now and wait for settlement |
| `Gate.Close` | send pending usage and close the lease, clearing its in-flight report |

Usage is never refused for money: the work happened, and admission is the check
before it. A Gate without an outbox keeps captured usage in memory until it is
sent: a crash loses it, never billed. `GateDeps.Outbox` stores it before
`Capture` returns, and a restarted process resends it. Each process is its own
gate with its own lease.

| Method | Use it to |
|---|---|
| `ListCustomerLimits` | where the customer `CustomerLimitListParams` names stands on each limit, then a `balance` line per currency (`GET /v1/me/limits` is the customer's own) |
| `AdjustCustomerLimit` | add to the current window only, idempotent on `SourceID`; a rate limit takes none |
| `UpdateCustomer` (`Limits`) | replace a customer's capacity; `Inherit` restores its products' |

**Spend limits.** A billing policy (merchant settings) caps unpaid arrears (`outstanding_cap`), spend per window (`window_spend_cap`) or the accrual rate (`accrual_rate_cap`, your usage and running charges). Its amounts, and a window without a currency, are in its `policy_currency`. Its outstanding cap and `collection_threshold_amount` bind only money in that currency, in gates' decisions and in usage billing alike; other currencies keep the account's own limits. Its windows and accrual-rate cap are converted to the request's currency; bindings choose the merchant default or a trust tier, and a customer's own `billing_policy` wins. **Delinquency**: an unpaid usage invoice past `arrears_grace_days` and above its currency's `delinquency_floor` (a policy's `delinquency_amount_floor` in its `policy_currency`) refuses requests; OpenRails never revokes access or cancels for it. Act on the `delinquency.grace`, `delinquency.entered` and `delinquency.cleared` events.

## Usage and running charges

Usage needs the deployment's event store (ClickHouse). Without one `GET /v1/config` reports `usage: false`, and lease syncs, `GET /v1/me/usage`, customer limits and their adjustments, meter and limit edits and usage metrics answer `409 event_store_required` (`billing.ErrEventStoreRequired`); running charges and usage invoices still work.

`SyncLeases` (`POST /v1/app/leases/sync`) is the only usage intake; a gate sends it for you. A batch job picks one `lse_` lease id for its stream and sends up to 1,000 numbered items per sync, in order, and nothing else; the lease's highest settled number makes a resend harmless, a changed item is `422 idempotency_key_reused`, a gap `409 usage_out_of_order`. A sync settles as much as its time allows, at least its first chunk (up to 100 customers and 250 items); items past `usage.through_n` that it did not refuse are sent again. An item may be up to 35 days old, and its `metadata` at most 1,024 bytes as compact JSON (`billing.MaxUsageMetadataBytes`; a longer one is refused alone, `400 invalid_param` naming `metadata`). Usage counts toward every limit that applies to it: in the windows current when its request was admitted (`admitted_at`, else when it arrives), and in the spend windows of the invoker attributes it reports (`trust_level`, `roles`, `resource`; the customer's own trust level when absent; none while their billing policy cannot be resolved, as in an FX outage). It is priced by the rate cards in force when it arrives. A priced meter that cannot price an item refuses it alone (`422 usage_not_priced`). `outcome: failed` states its own cost: forgiven up to the policy's `bad_spend_windows`, never charged to a delegated invoker, and what grace leaves posts at once.

Each item is one event in the event store, written before the sync answers: once it answers, every view, limit and balance reads it. A resend after a lost reply inserts nothing twice. While the store is out of reach a sync answers `503 service_unavailable` and settles nothing; resend it.

Usage posts to the ledger at each month's end in the merchant's `accounting.timezone` and before any other movement of the customer's balance in its currency: a top-up or grant, a credit lot's expiry, revocation or refund, a usage invoice. A posting takes, per pricing period, the price of the period's whole usage before it, less what earlier postings took: one deduction per credit lot drawn, then owed for the rest. So a tiered price is the price of the period's total however its usage arrived, and late usage posts with the next posting. A volume price that falls as the period grows credits the difference back once the period's 35-day late window closes. Until it posts the balance already holds the usage (`GetBalance`'s `HeldAmount`), so gates refuse as if it had posted. Read what was used with `GET /v1/me/usage` (by event type, invoker or meter, over the days of the merchant's `accounting.timezone`: each day's share of its period's price, so days sum to what posts), the posting on `ListBalanceTransactions` (its period, `through`, and what it added per meter), or `QueryMetrics`.

**Usage billing** follows the merchant's `usage_billing` terms: a merchant with
them turns owed usage and running charges into a usage invoice on the customer's
monthly date, or when debt reaches the credit limit, and charges the default card
for the currency; one without has none. No host setting turns it on or off, and
it needs only PostgreSQL: metered prices need the event store, running charges
do not. A
customer spends on credit only with a valid default card and its agreement.
Declined collection follows the renewal retry schedule; `send_invoice` invoices
are never charged.

A **running charge** bills a rate per hour from start to stop: a rented machine, a
reserved seat. The `reference` owns replay. Nothing is written while it runs: the
accrued amount is computed on read and posts when it stops and once a day.
`GetBalance` answers `AvailableAmount`, the accrual rate and `DepletesAt`, so a
page can count down without reading again; `balance_notices` and per-customer
low-balance lines tell you when a balance runs low.

| Method | Use it to |
|---|---|
| `SyncLeases` | report usage from a batch job, or a gate's lease |
| `StartRunningCharge`, `StopRunningCharge` | start and stop billing by the hour |
| `GetRunningCharge`, `ListRunningCharges` | what one has charged so far; which are depleted |

```go guide:running-charge
_, err := shop.StartRunningCharge(ctx, billing.StartRunningChargeParams{
	Reference:     "machine-" + machine, // the identity: a retry with the same terms answers the same charge
	CustomerID:    customer,
	Currency:      "USD",
	RatePerHour:   1_200_000, // $1.20 an hour, accrued per second
	MinimumAmount: 1_200_000, // refused (402) unless an hour is covered
})
if err != nil {
	return nil, err
}
stop := func(ctx context.Context) error {
	_, err := shop.StopRunningCharge(ctx, "machine-"+machine, billing.StopRunningChargeParams{})
	return err
}
```

## Events

OpenRails records billing facts in a retained, ordered log. It emails customers
the facts the merchant turns on ([customer messages](#customer-messages)); your
app reads the log for everything else: inboxes, other email, preferences. `ListEvents` (or
`GET /v1/app/events` with `merchant:events:read`) pages it by cursor; OpenRails
keeps no readers or acknowledgments.

```go guide:events
for {
	cursor, err := loadCursor(ctx, db) // "" starts at the oldest retained event
	if err != nil {
		return err
	}
	page, err := shop.ListEvents(ctx, billing.EventListParams{After: cursor, Limit: 100,
		Types: []string{string(billing.HostEventInvoiceCompleted)}})
	var status *billing.StatusError
	if errors.As(err, &status) && status.Code == billing.CodeEventCursorExpired {
		return rebuild(ctx, db) // 410: re-derive your state, then start a new cursor
	}
	if err != nil {
		return err
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	for _, event := range page.Events {
		if err := fulfil(ctx, tx, event); err != nil { // idempotent by event.ID
			_ = tx.Rollback(ctx)
			return err
		}
	}
	if err := saveCursor(ctx, tx, page.NextCursor); err != nil { // with the effects, empty pages too
		_ = tx.Rollback(ctx)
		return err
	}
	if err := tx.Commit(ctx); err != nil || !page.HasMore {
		return err
	}
}
```

Process each page idempotently by event id and save the cursor in the same
transaction as your effects, empty pages included; a reader never skips an
earlier uncommitted fact. The event log keeps 90 days; reading does not extend
it. `410 event_cursor_expired` means the cursor predates
retention: rebuild your state, then start a new cursor. `400 invalid_cursor` is
malformed or another merchant's or filter's cursor; filters are fixed for a
cursor. `EventReader()` adapts the client to the helpers module's
`eventlog.Reader`, so one consumer runs in process and remote.

The envelope: `id`, `merchant_id`, `source`, `type`, `version`, `occurred_at`,
`recorded_at`, `subject` and `data`, money as decimal strings. Host events are
the `billing.HostEventType` constants: `invoice.completed`, `invoice.expired`,
`invoice.payment_failed`, `mandate.revoked`, `delinquency.*`, `balance.low`,
`balance.depleted`, `product.entitlements_changed`, `destructive.hold_opened`
and `destructive.hold_cleared`, among others. Customer notices
(`ntf_` ids) carry the recipient: match its `account_issuer` as well as the UUID.

- `subscription_canceled` is renewal stopped with access remaining;
  `premium_ended` with `data.access_ended: true` is access closed.
- `payment_method_failed`, `payment_method_update_required`,
  `payment_authentication_required` and `invoice_collection_stopped` carry the
  retry decision and a `recovery_action` you map to a page. Re-read before
  acting.
- `MerchantProfile.NotificationURL(type)` picks the profile URL for a notice.
- Embedded, `Deps.Notifications` publishes selected notices to an AuthKit
  account inbox in the billing transaction ([embedded.md](embedded.md)).

## Customer messages

OpenRails emails customers their receipts, payment problems, cancellation
confirmations and price-change notices; the merchant's `messages` document turns
each event type on or off and replaces its template
([guide](guide.md#customer-messages)).

| Method | Use it to |
|---|---|
| `PreviewMessage` | render an event type's message, or a candidate template, over sample data and the merchant's profile; nothing is saved (`merchant:config:read`) |
| `CreateTestMessage` | send that rendering to the merchant's `alert_email`, the only address a test goes to, ten an hour (`merchant:config:manage`) |
| `ListCustomerMessages` | read a customer's delivery log, newest first: event, status, attempts, last error, recipient; no body (`merchant:customer:read`) |

```go guide:messages-check
candidate := &billing.MessageTemplate{
	Subject: "Your {{.Merchant.Name}} receipt",
	Text:    "You paid {{.Event.Amount}} for {{.Event.ProductName}}.",
}
preview, err := shop.PreviewMessage(ctx, billing.PreviewMessageParams{EventType: "one_off_purchase_completed", Template: candidate})
if err != nil {
	return err // 400 message_template_invalid names what does not render
}
_ = preview.Subject
if _, err := shop.CreateTestMessage(ctx, billing.CreateTestMessageParams{EventType: "one_off_purchase_completed", Template: candidate}); err != nil {
	return err // sent to alert_email only, ten an hour
}
log, err := shop.ListCustomerMessages(ctx, customer, billing.CustomerMessageListParams{})
if err != nil {
	return err
}
for _, m := range log.Items { // newest first; no body
	_, _, _ = m.EventType, m.Status, m.LastError
}
return nil
```

## Imports and archives

| Method | Use it to |
|---|---|
| `CreatePSPImport` | pull a PSP's customers, cards, subscriptions and charges, read-only, into staging |
| `ListPSPImports`, `GetPSPImport`, `ListPSPImportRecords` | follow it and review the staged records |
| `SetPSPImportMappings` | map your own identifiers (a legacy user or order id) to customers, for the import's rules |
| `LinkPSPImport` | link records to customers (and set a CCBill subscription's price) |
| `UpdatePSPImport`, `ApplyPSPImport`, `DeletePSPImport` | change its rules, land the linked records (a person or the in-process host), or discard it |
| `ListProviderCatalog` | the provider prices OpenRails observed |
| `ListParkedSubscriptions` | provider subscriptions that could not land: map the price or link the customer |
| `ImportBilling` | post a legacy book as facts at one `AsOf`; a re-post reports `skipped` |
| `ExportBillingArchive`, `ImportBillingArchive` | move one merchant's billing to another database |

A **PSP import** needs only the PSP's credentials: the provider is the source of
truth. Its differences from what OpenRails holds become proposals. A **billing
import** is for a book only your legacy system has: hand over facts (who, which
price, the provider's subscription id, paid-through evidence, explicit cancels,
vault references), not classifications; OpenRails derives status and access, an
evidence-starved subscription lands `unverified` with access kept, and missed
periods are never back-billed. Imported members are entitled when the call
returns. Provider schedules keep billing at the provider after the cutover.

An **archive** is one merchant's portable copy, ledger and access included and
the event log excluded. Every writer must be stopped first; the source stays
readonly afterwards and the import lands readonly until an operator arms it
([standalone.md](standalone.md#backup-and-restore)).

```go guide:export-archive
f, err := os.Create(path) // a JSON Lines stream; every writer must be stopped first
if err != nil {
	return err
}
if err := shop.ExportBillingArchive(ctx, f); err != nil {
	_ = f.Close()
	return err
}
return f.Close()
```

## Proposals and findings

A finding is a divergence OpenRails detected: `pull.*` (a local mirror against the provider), `derive.*`, `life.*` (a record against its clock), `consistency.*`. A `pull.*` finding with a change is a proposal: approving it applies that change. Findings need merchant configuration authority. Approving a finding whose recommendation refunds, cancels, or grants or revokes a product, or a proposal whose change refunds, revokes, cancels, collects a renewal or corrects a subscription's price or terms, also needs `merchant:customer:manage`, as that action's own route does, and spends that route's operation limit once per request, however many changes it carries; a proposal that only records what the provider already did and billed needs the configuration alone. Applying a PSP import approves only the corrections its approver may approve, and is refused while one they may not is open.

| Method | Use it to |
|---|---|
| `ListFindings`, `GetFinding`, `ResolveFinding` | the queue; resolve one by hand |
| `ListProposals` | proposals by PSP, change kind or import |
| `ApproveProposals`, `DismissProposals` | apply or dismiss up to 100, each by a person or the in-process host; approving takes `idempotencyKey`, dismissing needs notes |

```go guide:proposals
page, err := shop.ListProposals(ctx, billing.ProposalListParams{Pull: pull})
if err != nil {
	return err
}
ids := make([]billing.FindingID, 0, len(page.Items))
for _, p := range page.Items {
	ids = append(ids, p.ID) // each carries its Change: what applying it does
}
_, err = shop.ApproveProposals(ctx, key, billing.ApproveProposalsParams{IDs: ids, Notes: "reviewed"})
return err
```

Payment health findings stay open while their condition holds:
`life.payments.new_card_decline_spike`, `life.payments.rebill_failure_spike`,
`life.payments.system_errors`, `life.decline.unmapped` and `life.webhooks.silent`.
A proposal whose basis moved is superseded; one that ends access waits for the
merchant's destructive actions to be armed ([standalone.md](standalone.md#merchant-controls)).

## Metrics

| Method | Use it to |
|---|---|
| `GetMetricsSchema` | the measures, dimensions and grains a query may name, with each measure's group (`business` or `ops`), direction and thresholds |
| `QueryMetrics` | cash, payment, recurring revenue, retention, renewal, usage and ops measures, grouped and bucketed |
| `ListDisputePrograms` | each card PSP's standing in the dispute monitoring programs (Visa VAMP, Mastercard's excessive chargeback and fraud programs, Stripe's guidance, the PSP's `dispute_ratio_limit`) for a month: ratio so far, projected, level and thresholds |
| `GetDashboard`, `SetDashboard` | the console dashboard's saved layout, or the health overview built for the merchant's profile |

```go guide:metrics
return shop.QueryMetrics(ctx, billing.QueryMetricsParams{
	Measures: []string{"cash", "mrr"},
	By:       []string{"time"},
	Grain:    "month",
	Range:    &billing.MetricsRange{Last: "12m"},
})
```

A fact is one measure split by its outcome or kind: `cash` by `cash_kind`, `payments` by `payment_kind`, `attempts`, `purchases` and `rebills` by `outcome`, `mrr_movements` and `subscriber_movements` by `movement`; rates such as `refund_rate` and `rebill_rate` divide its slices. Recurring measures (`mrr`, `subscribers`, `subscriber_churn_rate`, `nrr`, `grr`, `cohort_retention`, `arpa`, `ltv`, `scheduled_mrr`) read the MRR rollups; `subscriber_churn_rate` is the period's own churn as a monthly rate at any grain, `ltv` divides ARPA by the monthly churn of the trailing three months, NRR and GRR read monthly with grain `month`, `subscribers` without a currency counts each customer once, and `scheduled_mrr` looks ahead with `range.next` (`{"next": "12w"}`). `grain: "auto"` picks day, week or month from the range, and `compare` is `previous_period` or `previous_year`. Money is split by currency unless one is named.

An unknown measure or a bad combination is a `400` naming what is wrong and what is allowed, so a caller (an agent included) can correct itself; a removed measure's error names its replacement.

## Alerts

A finding that needs review goes, at medium severity and higher, to the merchant's alert webhooks, at high and critical also to `alert_email`, once per episode unless the severity rises; a low one stays in the console. A destructive hold goes to both. Each destination retries on its own; a webhook retry carries the same `Idempotency-Key`.

| Method | Use it to |
|---|---|
| `ListAlertWebhooks`, `CreateAlertWebhook`, `UpdateAlertWebhook`, `DeleteAlertWebhook` | manage destinations (writes need Vault): `generic` JSON, `discord` or `slack`; a URL is write-only and a new URL keeps the webhook's identity |

```go guide:alert-webhook
_, err := shop.CreateAlertWebhook(ctx, billing.CreateAlertWebhookParams{
	Name:   "ops",
	URL:    url, // write-only: reads show only its host
	Format: billing.AlertWebhookSlack,
})
return err
```

## Destructive holds

Refunds, cancellations, card removals, access and credit revocations, write-offs and substantive catalog or configuration changes share one admission policy per staff member or application: by default 5 an hour and 15 a day per actor, with merchant and deployment ceilings that grow with active customers. A breach is a **hold**: denied `429` with `Retry-After`, and held until a person reviews and clears it; waiting, restarting or rotating a key does not release it. An admitted action that never starts stops counting after 15 minutes. SCIM contact erasures keep their own per-merchant budget and hold, apart from all of these ([contacts](#customers)). The in-process host's own calls spend nothing; a call made for a person through `ActingFor` is theirs.

| Method | Use it to |
|---|---|
| `ListDestructiveHolds` | the merchant's incidents |
| `ClearDestructiveHold` | clear one after review, with a reason |
| `ClearErasureHold` | clear the merchant's contact-erasure hold, with `merchant:provisioning:manage` alone |

```go guide:destructive-holds
page, err := reviewer.ListDestructiveHolds(ctx, billing.DestructiveHoldListParams{})
if err != nil {
	return err
}
for _, hold := range page.Items {
	_, err := reviewer.ClearDestructiveHold(ctx, hold.ID, billing.ClearDestructiveHoldParams{
		Reason: "credential revoked; pending effects reviewed",
	})
	if err != nil {
		return err
	}
}
return nil
```

Clearing needs merchant configuration authority plus
`root:admin-rate-limits:unlock` (`billing.RootAdminRateLimitsUnlock`), from a
person acting in person who is not the one held; in process, a handle made with
`ActingFor` for that person. Hosts receive `destructive.hold_opened` and
`destructive.hold_cleared`.

## Errors

An unsuccessful request is a `*billing.StatusError`: the HTTP `Status`, the `RetryAfter` header and the envelope's `ErrorDetails` (`Type`, `Code`, `Message`, `Param`, `Metadata`, `RequestID`, and on a decline the `Invoice` left open). Every code is registered once with its status and type (`billing.ErrorCodes()`, [error-codes.md](api/error-codes.md)). Each code is a `billing.ErrorCode` constant (`billing.CodeCardDeclined`, …), and `billing.ErrorCodeFrom(err)` reads it from a `StatusError` or an in-process refusal; `billing.LookupErrorCode` returns its `ErrorCodeInfo`. Classify by `errors.Is` and the code, never the message: comparing strings, or an interface declaring `ErrorCode() string`, does not match. The wire carries the same string. The client validates nothing the server checks: a bad request is the server's `400 invalid_param`.

```go guide:errors
var status *billing.StatusError
switch {
case err == nil:
	return "done"
case errors.Is(err, billing.ErrPaymentRefused): // 402: nothing charged; another card may pay the same invoice
	return "declined"
case errors.Is(err, billing.ErrIdempotencyKeyReused): // the key was sent with other terms: a bug
	return "bug"
case errors.As(err, &status) && status.Status < 500: // a refusal: act on its code, never Message
	return string(status.Code) // a billing.ErrorCode, e.g. billing.CodeCardDeclined
default: // no answer, or a 5xx: it may have committed; retry with the same key and terms
	return "retry"
}
```

- A transport failure is not proof the request was rejected: it may have
  committed. Retry a financial operation with the same idempotency key and terms;
  the client never retries one for you. `errors.Is(err, billing.ErrUnreachable)`
  matches a lost response and any 5xx; `errors.As` tells them apart.
- A provider's refusal is `402` with `billing.ErrPaymentRefused`: `card_declined`
  with `metadata.decline_reason` (`billing.DeclineReasonFrom`) and the
  buyer-facing failure (`billing.PaymentFailureFrom`), or `payment_method_stale`.
  `payment_provider_rejected` (502) is a gateway or configuration problem another
  card will not fix. `409 payment_duplicate_refused` asks you to wait a few
  minutes.
- `insufficient_credits` (402) is the balance denial; other 402s are not.
  `idempotency_key_reused` (422) is a key resent with other terms.
- `403 merchant_unresolved` is a valid credential bound to no live merchant: fix
  the credential. `401 credential_expired`: get a new token, retry once.
  `403 captcha_required`: solve and send `X-Captcha-Token`.
- `409 idempotency_key_in_use`, `payment_in_progress`: the original is still
  running. `provider_outcome_unknown`: read before retrying.
- `409 event_store_required` (`billing.ErrEventStoreRequired`): the deployment has no event store, so usage is off ([usage](#usage-and-running-charges)); `param` and `metadata.paths` name what a refused document or setting declared. Retrying does not help.
- `429` with `Retry-After`: by default the customer routes allow 10 purchases, 20 subscription writes and 40 payment-method writes a minute; never retry purchases in a loop. `429 card_attempts_blocked`: too many recent declines.

## Idempotency

A route that can move money or grant something takes a key, and its method takes it as `idempotencyKey` after `ctx`: `CreateInvoice`, `ChangeSubscription`, `RefundPayment`, `CreateProductAccess`, `ApproveProposals`; `GetInvoicePaymentAttempt` reads an attempt by it. The same key and terms recover the operation; other terms are `422 idempotency_key_reused`. Make the key when the person decides, store it with what it is for, and reuse it until the operation resolves. Other operations own their replay by a durable id instead: running charges by `Reference`, credit grants by customer and `SourceID`, external payments by `TransactionID`, lease usage by lease and item number, limit adjustments by `SourceID`. There is no response cache.

## Pagination

Every `List*` method returns a `*billing.ListPage[T]`: `Items` and `Next`, empty on the last page. Pass `Next` back as `PageRequest.Cursor`; `Limit` is 1–500 (default 50). There is no offset or total. Merchant lists of records take `IDs` (1–100) in place of a page. A `PSP` filter (`ListPSPImports`, `ListProviderCatalog`, `ListParkedSubscriptions`) takes a PSP's id (`psp_…`) or key; a `PSPID` filter takes the id. `ListEvents` is the exception: its own cursor and `HasMore`.

## Caching

A handle serves catalog and configuration reads from memory and refreshes them in
the background, serving the last good answer while refreshes fail; customer and
money reads always go to the server.

| Read | Embedded | Remote |
|---|---|---|
| `ReadCatalog` (products, prices), `ReadMeters` (meters, limits) | 1 m, stale ≤ 10 m | 10 m, stale ≤ 1 h |
| `ReadPublicConfig` | live | 10 m, stale ≤ 1 h |
| `ReadMerchantConfiguration` | live | 30 s, stale ≤ 10 m |

A handle's own write, and any change the embedded engine commits, drops the cache
at once; another replica's shows within its refresh interval. Override with
`Config.ReadCache` or `RemoteOptions.ReadCache` (a zero `ReadPolicy` reads live),
or read through `Uncached()`.

**Entitlement checks.** A remote handle with an API key caches `CheckEntitlements` answers for allowed keys only, never a denial: a check is answered from the cache only when every key is allowed there, so a newly bought key shows at once. A revocation or seat change can take up to `MaxAge` (2 minutes, `DefaultEntitlementCheckMaxAge`), and an allowed answer also expires at its next scheduled change, such as a rental's end. `Uncached().CheckEntitlements` always asks and drops the keys it finds denied. `EntitlementCheckCacheOptions.Redis`, your own client, shares entries between replicas with the same endpoint, merchant, credential and `MaxAge`; `Disabled` turns the cache off. Prefix and historical checks always ask.

## Without the Go client

Any stack calls the same HTTP API. Every route, with its permission, request and
response, is in [routes.md](api/routes.md); every error code in
[error-codes.md](api/error-codes.md); the schemas in
[`api/openapi.json`](../api/openapi.json).

| Path | Base URL | Backend credential |
|---|---|---|
| Embedded | your mount plus `/v1`, e.g. `https://shop.example/billing/v1` | whatever your `Routes.Auth` admits |
| Standalone | the server's origin plus `/v1` | `Authorization: Bearer` with your issuer's token or a merchant API key |
| SaaS | the console's **API URL** plus `/v1` | `Authorization: Bearer <API key>` |

```sh
curl --fail-with-body -X POST "$OPENRAILS_API_URL/v1/app/invoices" \
  -H "Authorization: Bearer $OPENRAILS_API_KEY" -H "Idempotency-Key: $CART_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"customer_id":"4d1c5f2e-…","lines":[{"price_id":"course-101:purchase:v1"}],
       "pay_url":{"success_url":"https://shop.example/thanks?invoice={INVOICE_ID}"}}'
curl --fail-with-body "$OPENRAILS_API_URL/v1/app/events?type=invoice.completed&after=$CURSOR" \
  -H "Authorization: Bearer $OPENRAILS_API_KEY"
curl --fail-with-body -X POST "$OPENRAILS_API_URL/v1/app/entitlements/check" \
  -H "Authorization: Bearer $OPENRAILS_API_KEY" -H 'Content-Type: application/json' \
  -d '{"customer_id":"4d1c5f2e-…","entitlements":["course:101"]}'
```

- **Merchant binding.** A request acts on the merchant its credential is bound
  to; nothing in the request selects one. Answers carry
  `OpenRails-Bound-Merchant`; send `OpenRails-Expected-Merchant: <uuid>` to refuse
  any other before the operation (`412 merchant_mismatch`).
- **Bodies.** JSON with only declared fields: an unknown field is
  `400 unknown_field`, a malformed query `400 invalid_query`, a body over 1 MiB
  `413 request_body_too_large`.
- **Lists** are `{"data": [...], "next_cursor": "..."}`; pass the cursor back as
  `?cursor=`, `null` on the last page. `?limit=` 1–500.
- **Money** is a decimal string of the currency's native units named `amount`
  beside a `currency` (`{"amount": "9990000", "currency": "USD"}`); a numeric
  amount is refused. Parse with `BigInt` or an exact decimal. `GET /v1/config`'s
  `currencies` gives each scale; a merchant's own units are in `GET /v1/me/config`'s `currencies`.
- **Ids** are prefixed text accepted only in their own spelling (`prod_`, `sub_`,
  `pay_`, `pm_`, `in_`, `psp_`, `iat_`, `lse_`, …); customers and merchants are
  UUIDs; a price id is URL-encoded as a path segment.
- **Times** are RFC 3339 UTC; absent values are `null`, empty lists `[]`.
  `DELETE` answers `204`.
- **Idempotency.** Routes marked `Idempotency-Key` in routes.md read the header.
- **Customer routes** (`/v1/me`) take the customer's own credential: embedded,
  your app's sign-in; otherwise an `openrails:self` token. Browser routes answer
  any origin without credentials (`Access-Control-Allow-Origin: *`); staff routes
  send no CORS headers.
- **Errors** are `{"error": {"type", "code", "message", "param", "metadata", "request_id"}}` ([errors](#errors)).
