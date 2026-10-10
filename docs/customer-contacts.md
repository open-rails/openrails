# Customer contacts

A customer is your user's id (the subject UUID your auth reports). Who it is —
email, username, display name — belongs to your directory. OpenRails reads it
for receipts and payment notices, the admin customer read
(`GET /v1/admin/customers/{customer_id}`, its `contact`) and search (`GET /v1/admin/customers?search=`), from one of two
sources:

| Deployment | Source | Copy |
|---|---|---|
| Embedded beside your AuthKit | `Deps.UserInfo`: an `openrails.UserInfo` (AuthKit's `ak.UserInfo()`) asked on every read | none |
| Standalone, hosted, or embedded with `Routes.Provisioning` | SCIM 2.0 pushes from your directory, plus verified token claims | what was pushed, and when (`contact.synced_at`) |

Without either, OpenRails sends customers no email; in-app notices and host
events still arrive. `Deps.UserInfo` and `Routes.Provisioning` are exclusive:
mounting both fails.

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

OpenRails is a SCIM 2.0 service provider (RFC 7643, RFC 7644). Provisioning
routes live under `{Prefix}/scim/v2`; the standalone server and hosted
deployments always mount them. Embedded, they are off by default because
`Deps.UserInfo` asks your AuthKit directly; turn them on
(`Routes.Provisioning: true`) only to keep a pushed copy instead (one or the
other: `Mount` refuses both). Point AuthKit's provisioning, Okta or Entra ID at
them.

**Authentication** is per merchant, one of:

- A provisioning token as `Authorization: Bearer`. Mint one with
  `client.CreateProvisioningToken` (`POST /v1/admin/provisioning-tokens`; the
  token is answered once), list and revoke them with `ListProvisioningTokens`
  and `DeleteProvisioningToken`, or declare one in the merchant declaration's
  `secrets.scim_token` (at least 32 characters; replaced or removed with the
  declaration). OpenRails keeps only its SHA-256.
- On a standalone server, a client-credentials `at+jwt` with scope `scim` from
  one of the merchant's trusted issuers (`OpenRails-Merchant` names the merchant
  when the issuer serves several).

An embedded mount admits only its own merchant's tokens.

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

Errors are RFC 7644 error bodies. `bill.SCIMHandler()` serves the same routes
in process, rooted at the SCIM root, for a directory in the same binary.

## Newest wins

A standalone server also records the contact claims of each verified customer
access token (`email` when `email_verified`, `name`, `preferred_username`,
`updated_at`), so a user who registers and buys at once gets a receipt. A
report applies only when it is newer than what is held: a SCIM write is dated
by its resource's `meta.lastModified`, else when it arrives; claims by
`updated_at`. Claims without `updated_at` fill a contact only when none is held.
After a `DELETE`, older reports cannot bring the contact back.
