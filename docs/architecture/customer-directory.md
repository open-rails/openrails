# Customer directory (SCIM)

Who a merchant's customers are: their email, name and username. OpenRails bills
a customer by id, the subject UUID the host's auth reports, and reads contacts
only to show customers to staff, to match them in reconciliation and imports, and
to name the buyer to a provider's own page. Contacts come from the host's
directory, read in process on every use, or from a copy the merchant's directory
pushes over SCIM 2.0 (RFC 7643, RFC 7644). Integration guide:
[customer contacts](../customer-contacts.md).

## Concepts

- **Customer**: a `billing.customers` row `(merchant_id, issuer, id)`; `id` is the
  issuer's subject UUID, `issuer` NULL the host's own users.
- **Contact**: `identity.Contact`: email, name, username; `Active` and `SyncedAt`
  only for the kept copy. The API shape is `billing.CustomerContact`.
- **Directory**: `identity.Directory`, what every reader calls: `Contacts(ctx,
  merchantID, ids)` and `Search(ctx, merchantID, query, limit)`. A customer
  without a contact is absent, never an error.
- **Live directory**: `identity.Live` over `Deps.UserInfo` (`openrails.UserInfo`,
  AuthKit's `userinfo.Lookup`). Nothing is copied.
- **Kept copy**: `identity.Kept` over `billing.customer_contacts`, written by SCIM.
- **Provisioning token**: a merchant's SCIM bearer secret (`orscim_…`), kept as its
  SHA-256 in `billing.provisioning_tokens`.
- **Customer scope**: `customerscope.Scope`, the customer a `/v1/me` route acts
  as, bound by the route gate only.

## How it works

**The source is chosen once.** `internal/app/build_runtime.go` builds
`identity.Live` when `Deps.UserInfo` is set, else `identity.Kept`, and records the
choice as `Runtime.HostUserInfo`. The SCIM and provisioning-token routes mount
only for the kept copy with `RouteGroups.Programmatic` on
(`FeatureCustomerProvisioning` in `internal/http/routes/mount.go`).

| Deployment | Directory | Copy |
|---|---|---|
| Embedded with `Deps.UserInfo` | `identity.Live` over the host's lookup | none; SCIM routes are 404 |
| Embedded without | `identity.Kept` | `customer_contacts`, pushed over SCIM |
| Standalone server | `identity.Live` over `customerDirectory` (`server/server.go`) | in the server's AuthKit |

The standalone server refuses a host `Engine.UserInfo` and installs
`customerDirectory`, an `identity.IssuerLookup`. Its own users are its AuthKit's
(`ControlPlane.Customers` with issuer `""`); a trusted issuer's are that merchant's
group directory in AuthKit, which the issuer fills over AuthKit's SCIM
(`/directory/scim/v2`). OpenRails' own SCIM routes are not mounted there.

**Live reads.** `Live.Contacts` reads each id's issuer from `billing.customers`
(`ListCustomersByIDs`), asks each issuer's directory for its own subjects, 500 at
a time (`liveBatch`), and keeps an answer only from the customer's own issuer.
`Live.Search` searches the host's directory, then each issuer the merchant trusts
(`IssuerLookup.Issuers`).

**SCIM writes.** `scim.Server` (`internal/scim`) serves Users only. `externalId`
must be a UUID; it is the customer id and the SCIM `id`. Each write is one merchant
transaction; `createUser` first ensures the customer row (`db.EnsureCustomerRowQ`),
and updates go through `write` (`UpdateCustomerContact`).

- Newest wins: `directory_updated_at` holds the directory's date
  (`meta.lastModified`, else arrival). A POST or PUT older than it keeps the stored
  values (`newer`). A PATCH is dated by its arrival.
- `DELETE /Users/{id}` clears the values and `provisioned_at`, keeps the row and
  dates it by its arrival, so an older report cannot restore them. It also erases
  the customer's notification events (`EraseCustomerNotificationEvents`); billing stays.
- `userName` is unique per merchant, ignoring case, among provisioned rows
  (`customer_contacts_user_name_key`); a clash is `409 uniqueness`.
- Bulk applies each operation alone, in order, up to `MaxOperations` (1000) and
  `MaxPayloadSize` (1 MiB); `failOnErrors` stops early. A list pages by
  `startIndex` and `count` (at most `MaxResults`, 200) with one `eq` filter.
- `active` is stored and shown, never enforced.

**Authentication.** `provisioningGate` (`internal/http/routes/gate.go`) admits a
provisioning token, resolved by its SHA-256 (`scim.Tokens.Resolve`) and bound to
its row's merchant, or else what the programmatic gates admit: an application
bound to the merchant, never a person. Both pass `boundMerchant`, which requires a
live merchant. `scimMerchant` takes the merchant from the token or the
application's binding, never from the request. A refusal before the SCIM server
runs is rewritten as a SCIM error (`scimRefusals`).

**Tokens.** `Tokens.Create` mints `TokenPrefix` plus 32 random bytes and answers it
once; `Tokens.Delete` revokes. A declaration's `secrets.scim_token` (at least
`MinDeclaredToken`, 32 characters) is applied by `scim.Declare` whenever the
merchant's configuration loads (`syncMerchantConfig`); a shorter one rejects the
merchant document. `TouchProvisioningToken` records `last_used_at`, at most once a
minute.

**Readers.** Each reads `Runtime.Contacts` within one merchant.

| Reader | Use | Directory down |
|---|---|---|
| `Service.GetCustomer`, `Service.ListCustomers` | `GET /v1/admin/customers/{customer_id}` and `?search=` | `503 service_unavailable` |
| `customerContacts` (`GetAdminSubscriptions`, `ListOrders`, `writePaymentPage`) | contacts beside staff lists | the list loads without them |
| `adminChangeCustomer` | email for `POST /v1/admin/subscriptions/{id}/change` | `503 service_unavailable` |
| `PGLocalStateLoader.fillCustomerEmails` | email matching in reconciliation | the load fails |
| `Service.identify` | buyer's email and username for a provider page, after the credential's verified claims | the call fails |
| `Customers.GetUserIDByUsername` | the CCBill username bridge | the webhook fails |
| `linker.directory` (`internal/pspimport/link.go`) | PSP import linking by email or username | the link fails |

## Interfaces

- Go: `Deps.UserInfo`; `Client.GetCustomer` and `Client.ListCustomers` answer
  `Contact`; `Client.CreateProvisioningToken`, `ListProvisioningTokens`,
  `DeleteProvisioningToken`.
- HTTP: `{Prefix}/v1/app/scim/v2` (`/Users`, `/Users/{id}`, `/Bulk`,
  `/ServiceProviderConfig`, `/ResourceTypes`, `/Schemas`) and
  `/v1/merchant/provisioning-tokens` (`merchant:config:read`,
  `merchant:config:manage`). [Routes](../api/routes.md).
- Configuration: `secrets.scim_token` in a merchant declaration or Vault document
  ([merchant configuration](../merchant-configuration.md)).

## Guarantees

### 1. Exactly-once provider calls
Does not apply: the directory makes no provider calls.

### 2. Agreement with providers
The directory holds no provider record, but provider-facing work reads it, and an
unanswered or ambiguous read never stands in for a match.

Checks:
- `PGLocalStateLoader.fillCustomerEmails` returns the directory's error instead of
  matching without emails.
- `Customers.GetUserIDByUsername` answers `ErrUsernameUnknown` unless exactly one
  contact holds the username.
- `linker.directory` keeps only contacts whose email or username equals the key
  (`strings.EqualFold`).

### 3. Exact money
Does not apply: the directory holds no amounts.

### 4. Access control and tenant isolation
Every contact query names its merchant, and the merchant comes from the
credential. `customer_contacts` has `PRIMARY KEY (merchant_id, customer_id)` and a
composite foreign key to `customers`. A provisioning token names no subject
(`provisioningToken.Identity`), so it opens only the SCIM routes. A live read
keeps only the customer's own issuer's answer. Gap: the SCIM routes declare no
permission, so any application credential bound to the merchant may write its copy
([routes](../api/routes.md) states it).

Checks:
- Every query in `internal/db/queries/customer_contacts.sql` and
  `provisioning_tokens.sql` filters by `merchant_id`, except
  `ResolveProvisioningToken`, which finds the merchant by the hash.
- `Live.Contacts` and `Live.Search` keep a contact only when
  `issuers[contact.CustomerID] == issuer`.
- OR-J07-S03: a provisioning token is refused on `/v1/admin/customers` and is
  `401` on `/v1/app/scim/v2` once deleted.
- `customerscope.Bind` is called only from `internal/http/routes`.

### 5. Contracts match the code
The routes are in the route catalog (`provisioningRoutes`); their bodies are the
RFC's (`scimBody`), so `api/openapi.json` leaves them out and `docs/api/routes.md`
lists them.

Checks:
- `api/go.txt` lists the three token methods on `*Client`.
- `Catalog()` includes `provisioningRoutes`; no second route table exists.
- `api/openapi.json` has no `/v1/app/scim` path.

### 6. Migrations and retention
Both tables are in the baseline and are `State` in
`internal/retention/tables.go`. Erasure keeps an empty row on purpose. Neither table
moves with the billing archive. Gap: a merchant purge deletes neither, so a purged
merchant's contacts and token hashes stay.

Checks:
- `internal/retention/tables.go` has `customer_contacts` and
  `provisioning_tokens`.
- `internal/merchantarchive/checks.go` excludes both, with reasons and column lists.
- `merchantOwnedTables` in `internal/merchants/purge.go` lists neither (the gap).

### 7. Work scales with activity
No job runs: the copy changes only when a directory pushes, and reads are per
request. Live reads are batched, kept searches capped (`maxSearch`, 1000), and a
token's `last_used_at` written at most once a minute.

Checks:
- `internal/scim` and `internal/identity` register no River job.
- `TouchProvisioningToken` only updates a row older than one minute.
- `customer_contacts_provisioned_idx` matches `ListProvisionedContacts`' filter
  and order.

### 8. Abuse protection
SCIM routes are in no rate-limit bucket ([limits](../limits.md): a per-address
ceiling belongs to the proxy). They are bounded by size instead, and a minted
token carries 256 random bits.

Checks:
- `readBody` caps every SCIM body at `MaxPayloadSize`; Bulk refuses more than
  `MaxOperations`; lists clamp `count` to `MaxResults`.
- `token_sha256` is unique and 32 bytes; only the hash is stored.
- `SearchCustomerContacts` receives a LIKE pattern escaped by `likeEscaper`.

### 9. Destructive-action safeguards
A `DELETE` erases only contact values and notification events; the customer, its
ledger and subscriptions stay. Gap: no ceiling or hold bounds erasures (a Bulk
request may carry 1000), and nothing records which token or application made one
beyond `last_used_at`.

Checks:
- `deleteUser` writes only `customer_contacts` and `notification_events`.
- `internal/scim` makes no `destructive` admission (the gap).

## Tests

- OR-J07-S01, OR-J07-S02 and the first part of OR-J07-S03 run against the
  standalone server fixture: they push to AuthKit's directory
  (`/directory/scim/v2`) and read contacts through `identity.Live`. They cover
  create, patch (an invalid operation rolls back), replace, delete, another
  merchant's `404`, issuer separation, `409 uniqueness`, Bulk results and
  revocation.
- The rest of OR-J07-S03 exercises `internal/scim`: a token from
  `CreateProvisioningToken` creates a User at `/v1/app/scim/v2/Users`, opens no
  admin route, and is `401` once deleted.
- OR-J07-S04: with `Deps.UserInfo`, contacts are live and both route families are
  `404`.
- OR-J07-S05: customer reads keep their contact and billing without hosted
  link storage.
- OR-J38-S41 (console, live directory): staff lists name the customer, still list
  by id with the directory down, and find a customer by name.
- Gaps: no scenario covers `internal/scim`'s newest-wins dating, PUT, PATCH,
  Bulk, filters, `secrets.scim_token`, or erasure blocking an older replay; nor a
  search across trusted issuers. `internal/scim` and `internal/identity` have no
  tests of their own.
