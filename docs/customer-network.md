# Customer network

On openrails.dev a shopper has one account across every merchant. A **link**
connects that account to one merchant's customer, so the shopper can see and
manage their billing everywhere in one place. The network runs on a standalone
server whose AuthKit declares the `network-terms` agreement; an embedded engine
has none and mounts none of these routes.

A link is made only when both sides are proven in one exchange: the merchant's
credential for its customer, and a fresh openrails.dev sign-in tied to it. An
email or phone match never links, and neither does a merchant's
`email_verified`.

## Turning it on

```yaml
auth:
  token: {issuer: https://openrails.dev}
  resource: {id: https://api.openrails.dev, public_url: https://api.openrails.dev}
  agreements:
    - {key: network-terms, version: "2026-10", url: https://openrails.dev/terms}
```

The server needs its resource (`auth.resource.id`). It then adds the network's
scopes to that resource and to what merchants' clients may ask for:
`openrails:self` (act as the customer at that merchant) and `openrails:link`
(link the merchant's own customer). An account accepts `network-terms` before
it approves any merchant's client. Merchants register their OpenID clients in
AuthKit (group OAuth clients); the consent screen names the merchant.

## What a link is

- One account is one customer per merchant, and one customer has one account:
  each side holds one active link.
- The merchant sees `network` on its customer, `{account_id, linked_at,
  method}`, in `GET /v1/admin/customers/{customer_id}` and the lists. It never
  sees the account's other merchants, cards or links. `account_id` is the
  account's stable public id, the `sub` every merchant's client receives.
- `method` is how it was proven: `connect` (from the merchant's site),
  `sign_in` (the merchant signs its customers in with openrails.dev) or
  `checkout` (on the checkout page).
- Links are openrails.dev's, not the merchant's: `billing.customer_links` is not
  in the merchant archive, and a deleted merchant's links end.

## Linking from the merchant's site

A merchant with its own login links its signed-in customer:

1. With the customer's credential at the merchant, ask for a challenge:
   `POST /v1/me/network-link/challenges` answers `201 {id, nonce, expires_at}`.
   It lasts 5 minutes, is spent once, and is bound to that merchant and
   customer.
2. Send the shopper to openrails.dev with an authorization request
   ([OIDC Core §3.1](https://openid.net/specs/openid-connect-core-1_0.html#CodeFlowAuth),
   code and PKCE) for the merchant's client: `scope=openid openrails:link`,
   `nonce` = the challenge's nonce and `prompt=login`. `login_hint` may carry
   the email the merchant knows; it pre-fills the form and proves nothing.
3. Redeem the code at your backend for the ID token, and send it with the same
   customer credential: `POST /v1/me/network-link` with `{challenge_id,
   id_token}` answers `201` with the link, or `200` when the customer was
   already linked to that account.

The server verifies the ID token in process: openrails.dev issued it for one of
this merchant's clients, it carries the challenge's nonce, the sign-in happened
after the challenge, the account's consent to that client holds
`openrails:link`, and the account accepted the network terms.

## Sign in with openrails.dev

A merchant without its own login uses openrails.dev as its login: its client
asks for `openid openrails:self` with the server's resource, and the access
token is the shopper at that merchant. The customer's id is the account's, and
the first request links it (`method: sign_in`) once the account accepted the
network terms. A token granted only `openrails:link` is no customer: `403
permission_required`. An account linked to another customer of the merchant
signing in this way is refused `409 network_account_linked`.

## At checkout

On the hosted checkout host, the order's checkout URL is the merchant's side:
its backend created the order for its customer. The URL's secret asks for the
challenge and redeems it (the same two routes), and the shopper signs in with
the checkout page's own client, `openrails-checkout`, which the operator
declares in `auth.authorization_server.clients` with the checkout host's
redirect URI. Only that client answers a checkout URL, and a merchant's client
never does.

## Refusals

| Status and code | Why |
|---|---|
| `400 network_link_challenge_invalid` | the challenge is unknown, expired or spent, or the ID token does not answer it: another client or merchant, another nonce, a sign-in older than the challenge, no `openrails:link` consent |
| `409 network_link_conflict` | the customer is linked to another account: sign in with that one |
| `409 network_account_linked` | the account is linked to another customer of this merchant |
| `403 network_terms_required` | the account has not accepted the network terms |
| `409 network_link_in_use` | unlinking while the customer's subscriptions at the merchant still charge its cards |

## Finding the customer of an account

A merchant whose customer signs in with openrails.dev finds its own customer by
the account's id: `GET /v1/app/customers?network_account_id=` (the `Customers`
permission, `merchant:customers:read` on the server; `Client.ListNetworkCustomers`)
answers a page of at most one.

## Unlinking

- The account ends a link with `server.Server.UnlinkCustomer`; a portal lists
  them with `server.Server.CustomerLinks`. It is refused while the customer has
  a subscription at that merchant that is not canceled: cancel it or move it
  to another card first. The merchant's clients lose the account's consent and
  refresh tokens ([RFC 7009](https://www.rfc-editor.org/rfc/rfc7009)), so a
  merchant that signs its customers in with openrails.dev asks again. The
  customer, its orders and subscriptions stay.
- Withdrawing the merchant's consent in the account's settings unlinks too,
  and is refused the same way.
- Deleting the account is refused while a linked customer's subscriptions
  still charge its cards; once the account is purged its links end.
- Deleting a merchant ends its links.

A revoked link is kept 24 months, then deleted.

## Host events

| Type | `customer_network` |
|---|---|
| `customer.network_linked` | `customer_id`, `account_id`, `method` |
| `customer.network_unlinked` | `customer_id`, `account_id`, `revoked_by`: `customer`, `account_deleted` or `merchant_left` |

Read and acknowledge them like every host event (`GET /v1/app/host-events`).
