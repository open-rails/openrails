# Customer contacts

A customer is your user's id (the subject UUID your auth reports). Who it is —
email, username, display name — belongs to your directory. OpenRails reads it
for receipts and payment notices, the admin customer read
(`GET /v1/admin/customers/{customer_id}`, its `contact`) and search (`GET /v1/admin/customers?search=`), from one of two
sources:

| Deployment | Source | Copy |
|---|---|---|
| Embedded beside your AuthKit | `Deps.UserInfo`: an `openrails.UserInfo` (AuthKit's `ak.UserInfo()`) asked on every read | none |
| Standalone or hosted | the server's AuthKit: its own users, and each merchant's directory of its trusted issuers' users, asked on every read | in AuthKit |
| Embedded without `Deps.UserInfo` | SCIM 2.0 pushes from your directory | what was pushed, and when (`contact.synced_at`) |

Without either, OpenRails sends customers no email; in-app notices and host
events still arrive. With `Deps.UserInfo`, the SCIM routes are not mounted: one
source of truth.

Customers get, through `Config.SMTP` or `Deps.Email`: a receipt for every
one-off purchase (an order, a checkout session, a credit deposit) once its
payment settles, naming what was bought and the amount ("12.99 USD"), once
per payment however often the provider reports it; membership confirmations
and renewals; and payment, invoice and dunning notices. Each is also in
`GET /v1/me/notifications`.

## In process

```go
bill, err := openrails.New(ctx, cfg, openrails.Deps{Postgres: db, UserInfo: ak.UserInfo()})
```

Implement `openrails.UserInfo` yourself over your own user table if you do not
use AuthKit: `Get(ctx, ids)` returns the users it holds, keyed by id;
`Search(ctx, query, limit)` matches email, username or name, ignoring case.
`userinfotest.Check` (helpers) checks an implementation.

## SCIM provisioning

An embedded engine is a SCIM 2.0 service provider (RFC 7643, RFC 7644). Its
routes live under `{Prefix}/v1/app/scim/v2`, in the programmatic route group:
they mount with `RouteGroups.Programmatic`, unless `Deps.UserInfo` asks your
AuthKit directly. Point AuthKit's provisioning, Okta or Entra ID at them.

The standalone server mounts none: its customers' contacts are its AuthKit's.
A customer of the server's own users is read from them; a trusted issuer's
customer from the merchant's directory in the server's AuthKit, which the
issuer pushes to over SCIM (`{issuer}/directory/scim/v2`, AuthKit's
[directory](https://github.com/open-rails/authkit/blob/master/docs/scim.md#directory))
and its tokens' verified contact claims fill as they are used. Each customer
is read from its own issuer's directory, never another's.

**Authentication** is per merchant, one of:

- An application credential your `Routes.Auth` admits, as on every
  programmatic route: AuthKit's provisioning client, for one. A person is
  refused.
- A provisioning token as `Authorization: Bearer`, for directories like Okta
  or Entra ID that cannot get a token from your issuer; it opens nothing else. Mint one with
  `client.CreateProvisioningToken` (`POST /v1/admin/provisioning-tokens`; the
  token is answered once), list and revoke them with `ListProvisioningTokens`
  and `DeleteProvisioningToken`, or declare one in the merchant declaration's
  `secrets.scim_token` (at least 32 characters; replaced or removed with the
  declaration). OpenRails keeps only its SHA-256.

An embedded mount admits only its own merchant's tokens; another's is no
credential there, and your `Auth` refuses it.

**Users.** `externalId` is your user's id and must be a UUID; it is the
customer id and the SCIM `id`. OpenRails keeps `userName` (required, unique per
merchant, case-insensitive), `displayName` (else `name.formatted`, else given and
family names), the primary email (else the first) and `active` (default `true`;
shown, never enforced). Other attributes are accepted and ignored.

| Route | |
|---|---|
| `POST /Users` | create; `409 uniqueness` when the User or its `userName` exists |
| `GET /Users/{id}`, `GET /Users` | one User; the list pages the whole directory, oldest first, by `startIndex` and `count` (at most 200), or filters by one of `externalId`, `id`, `userName`, `emails.value` `eq "…"` |
| `PUT /Users/{id}` | replace; `externalId` cannot change |
| `PATCH /Users/{id}` | `add`, `replace`, `remove` on `userName`, `displayName`, `name`, `emails`, `emails[…].value`, `active` |
| `DELETE /Users/{id}` | erases the contact; the customer's billing stays |
| `POST /Bulk` | up to 1000 operations or 1 MiB; each applies on its own, `failOnErrors` stops early; a `POST` needs a `bulkId` |
| `GET /ServiceProviderConfig`, `/ResourceTypes`, `/Schemas` | discovery |

Errors are RFC 7644 error bodies, a credential your `Auth` refuses among them. `bill.SCIMHandler()` serves the same routes
in process, rooted at the SCIM root, for a directory in the same binary.

## Newest wins

A SCIM write applies only when it is newer than what is held: it is dated by
its resource's `meta.lastModified`, else when it arrives. After a `DELETE`,
older reports cannot bring the contact back. On a standalone server the
server's AuthKit also records each verified customer token's contact claims,
newest first against pushes, so a user who registers and buys at once gets a
receipt.
