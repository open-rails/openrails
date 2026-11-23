# Your own auth

A backend selling for two merchants, `acme` and `globex`, on one engine. Its
own `shop.users`, `shop.sessions` and `shop.roles` tables (`schema.sql`)
answer OpenRails through authkit/auth's interfaces (`auth.go`); its roles are
built from `billing.Permission` constants, and `directory` is `Deps.UserInfo`.

- `Authenticate` reads the session live; `Identity` names the shop's issuer and
  the user as subject and invoker.
- `BoundScope` is the merchant the session acts at (`POST /switch` changes it);
  `Can` answers exactly one permission in one scope, from `shop.roles`.
- One merchant needs no binding: declare the shop's issuer on it and return a
  zero `BoundScope`.

The [embedded guide](../../docs/embedded.md#auth) has the contract and what
each error answers. Run it with `DATABASE_URL` naming a PostgreSQL 18 database.
