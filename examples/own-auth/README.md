# Your own auth

A shop at `https://shop.example` selling for two merchants, `acme` and
`globex`, on one OpenRails engine, with no AuthKit: its own `shop.users`,
`shop.sessions` and `shop.roles` tables (`schema.sql`) answer OpenRails
through helpers/auth's interfaces (`auth.go`). A backend only.

## What the host implements

| | In `auth.go` |
|---|---|
| `Authenticate(r)` | the session the `Authorization: Bearer` token names, read live (OpenRails passes no cookies) |
| `Identity()` | issuer `https://shop.example`, the user's UUID as subject and invoker, `CredentialSession` |
| `RootScope` | `{https://shop.example, root}`: where root roles are held. A merchant's scope is `{https://shop.example, <merchant id>}`, the shop's name for that merchant's staff |
| `BoundScope()` | the merchant the session acts at (`sessions.merchant_id`): the store a shopper signed in at, or the one staff switched to with `POST /switch`; zero for none |
| `Can(scope, permission)` | exactly that permission in exactly that scope, from `shop.roles`: a root role's `root:` names in the root scope, a merchant role's `merchant:` names in that merchant's scope; a session bound to one merchant holds nothing at another and keeps its root roles |
| `CheckRecentSignIn` | signed in within 15 minutes, else a step-up `auth.Challenge` |
| `KnownPermission` | `billing.Permissions()` |

Roles (`admin`, `support`, `reader`) are built from `billing.Permission`
constants; the persona is where the role is held. `directory` is
`Deps.UserInfo`, so the admin routes show each customer's email.

## Which errors mean what

| `Authenticate` or `Can` returns | OpenRails answers |
|---|---|
| `auth.ErrUnauthenticated` (no token, unknown session) | 401 `authentication_required` |
| it joined with `auth.ErrRevoked` (signed out) | 401 `credential_revoked` |
| `auth.ErrUnavailable` (the database failed) | 503 `authentication_unavailable`; from `Can`, `authorization_unavailable` |
| `Can` false | 403 `permission_required` |
| `auth.ErrStepUpRequired` from `CheckRecentSignIn` | 401 `step_up_required` |
| a session bound to no merchant | 403 `merchant_unresolved` |

## One merchant

A shop serving one merchant needs no binding: declare its issuer on the
merchant (`MerchantDeclaration{Issuer: "https://shop.example"}`) and return a
zero `BoundScope`. Every session then acts at that merchant.

## Tests

`go test ./own-auth/` checks the example imports no AuthKit. The end-to-end
tests need a PostgreSQL 18 server:

```sh
OPENRAILS_E2E_DSN=postgres://… go test -tags e2e,integration ./own-auth/
```

`TestOwnAuthConforms` runs `openrailstest.CheckAuth` against the real
tables; `TestOwnAuthGates` drives the mounted engine over HTTP: switching
merchants, a merchant's support refused another merchant and the catalog,
`merchant_unresolved`, a step-up on a refund, a signed-out session, and a
shopper who sees only their store's merchant.
