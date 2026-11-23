# HTTP API conventions

The API is served under `/v1`: at the root of a standalone server, beneath the
host's prefix (usually `/billing`) when embedded. Generated from the route
catalog: [every route](routes.md) with its tier, permission, request and
response; [every error code](error-codes.md); shapes in
[`api/openapi.json`](../../api/openapi.json). From v1.0.0 the API
[changes only by addition](../compatibility.md).

## Requests and responses

- **Bodies.** JSON. Only declared fields: an unknown one is `400 unknown_field`,
  a malformed query parameter `400 invalid_query`, a non-JSON body
  `415 unsupported_media_type`, one over 1 MiB `413 request_body_too_large`.
- **Lists.** `{"data": [...], "next_cursor": "..."}`; pass it back as `?cursor=`
  (`null` on the last page). `?limit=` 1–500, default 50. No offset, no total.
  Merchant lists of records take `?ids=a,b,c` (1–100, alone): one page in the
  list's order, unknown or foreign ids absent.
- **Batches.** 1–100 items (usage events and admissions: 1,000). A batch either
  applies all or none, naming the offending `items[i].field`, or answers one
  `{status, …, error}` per item.
- **Statuses.** `200` result, `201` created, `202` accepted and unresolved, `204`
  no body (every `DELETE`).
- **Errors.** `{"error": {"type", "code", "message", "param", "metadata"}}`; the
  `code` is stable, the `message` is not ([errors](errors.md)).
- **Idempotency.** `Idempotency-Key` only where a route reads it (marked in
  routes.md): orders and their payment, card saves, subscription changes, product
  access grants, refunds, proposal approval. The same key and terms recover the operation; other
  terms are `422 idempotency_key_reused`. Generate one when the buyer decides to
  buy, store it with the order and resend it on every retry; a new key is a new
  purchase (a duplicate subscription or permanent product is refused, a timed pass
  or credit pack sells again). Admissions use `request_id`, usage events
  `(source, source_id)`, running charges `reference`. There is no response cache.
- **Discovery.** A route the deployment does not serve is not registered
  (`404 route_not_found`).

## Money, ids and times

Money is an `int64` of the currency's native units in Go and a decimal string of
the same integer on the wire, named `amount` (or `<thing>_amount`) beside a
`currency`; no field name carries a currency.

```json
{"amount": "9990000", "currency": "USD"}
```

Native units are the registry's scale: micros for USD and EUR, 10^4 per yen.
`GET /v1/config`'s `currencies` (and `billing.Currencies()` in Go) give each
currency's `decimals` and `minor_decimals` (what providers settle in). Every
monetary value is a string, prices to metrics cells; a numeric amount in a request
is refused, and JavaScript should use `BigInt` or an exact decimal. Currency codes
are upper-case. Counts are numbers, durations integer seconds, day buckets
`YYYY-MM-DD`. Every timestamp is RFC 3339 UTC; absent values are `null`, empty
lists `[]`.

Ids are prefixed text, accepted only in their own spelling (a bare UUID or another
prefix is `400 invalid_param`): `prod_`, `sub_`, `pay_`, `pm_`, `ord_`, `psp_`,
`oat_` (order attempt), `att_` (payment attempt), `cyc_` (rebill cycle), `cgr_`,
`txn_`, `ent_`, `pa_`, `pmig_`, `lse_` (lease), `hev_`, `ntf_`, `fnd_`, `awh_`. Customer and
merchant ids are UUIDs. A price id is `product-key:price-key:v1` (versions start at
one; reserved bytes percent-encoded, `:` as `%3A`); URL-encode it as a path
segment. A product or price *key* is a separate handle, never accepted where an id
is. In Go ids are types (`billing.PriceID`, `billing.CustomerID`); a Client
refuses a zero id before any request.

## Who may call what

| Routes | Credential |
|---|---|
| `GET /v1/config` | none |
| `/v1/orders/{id}/solana-pay` | none: the order names its merchant |
| Customer `/v1/me/*` | embedded: the host's user credential; standalone: a trusted issuer's token with scope `openrails:self` |
| `/v1/admin`, `/v1/metrics`, `/v1/catalog`, `/v1/merchant` | a credential holding the route's permission: embedded, whatever the host's auth admits; standalone, a session of the server's AuthKit, a trusted issuer's token with `openrails:merchant`, or an API key of the merchant's group |
| Programmatic `/v1/app/*` | an application credential, never a person (`403 application_required`); SCIM also takes the merchant's provisioning token when embedded |
| `/v1/webhooks/{rail}/{account_id}` | the provider's signature |

Each staff and programmatic route checks one permission pair: its `merchant:`
name at the bound merchant or its `root:` name in the root scope
([auth](../auth.md#permissions)). Findings need `merchant_config` and
`merchant:config:manage`; customer-support permissions do not reach them. No admin
route requires a recent sign-in. A `/v1/me` route acts on the credential's own
customer.

A request acts on the merchant its credential is bound to; nothing in the request
selects one ([auth](../auth.md#merchant-binding)). Merchant-bound answers carry
`OpenRails-Bound-Merchant`; `OpenRails-Expected-Merchant: <uuid>` is an optional
precondition (`412 merchant_mismatch`), sent by the Go client. Browser routes
(public, purchase, `/v1/me`) answer any origin without credentials; others send no
CORS headers.

## Public configuration

`GET /v1/config` is public, the same for every caller and cacheable for five
minutes (`ETag`):

- `capabilities`: the route groups served (`admin`, `catalog`, `merchant_config`,
  `metrics`, `app`) and features (`stripe_billing_portal`,
  `solana_one_time_payments`, `solana_subscription_management`,
  `merchant_config_edits` (configuration is in Vault), `catalog_copilot`).
- `currencies` and `rails` (each rail's environments, account fields, other fields
  and credential keys).
- `captcha`: `{provider, site_key, script_url, action, token_header}`, or null.

`GET /v1/me/config` is the bound merchant's part (`Cache-Control: private`):
`merchant` (`display_name`, `logo_url`, `support_url`, `embed_origins`) and
`payment.psps`, each `{psp_id, key, rail, custodian, display_name, flow, routed,
config}` with `flow` one of `tokenize`, `card`, `elements`, `redirect`, `wallet`
and only public `config` (NMI `tokenization_key`, Stripe `publishable_key`, Basis
Theory `public_api_key`). A PSP whose credentials could not be checked just now has
`status: "temporarily_unavailable"` and `retry_after`, and the document is
`no-store`. With Solana armed, `solana` carries the network and tokens.
`GET /v1/admin/access` is what a signed-in staff member may reach.

## Provider webhooks

`POST /v1/webhooks/{rail}/{account_id}` (beneath the prefix when embedded).
`{rail}` is `nmi`, `stripe`, `ccbill` or `basistheory`, never a PSP key: two NMI
PSPs post to the same rail and differ by account. The account resolves the
merchant, then its own secret verifies the request and the payload's account must
agree:

| Rail | Verification |
|---|---|
| NMI | `Webhook-Signature` (`t=…,s=…`), HMAC over timestamp and body |
| Stripe | `Stripe-Signature` with the endpoint's secret |
| CCBill | CCBill's published source ranges, plus the payload's account |

The path names no money class. A merchant may declare a live and a non-live PSP
on one account (a Stripe `acct_…` is both): a delivery belongs to the one whose
webhook signing secret verifies it, Stripe's `livemode` deciding one both
verify. One neither verifies is refused, and so is one both verify on another
rail: give them different secrets. CCBill signs nothing, so every delivery to a
CCBill account declared in both classes is refused. Another merchant cannot
claim an account one of its PSPs or custodians holds: in the same class ever,
in the other class while that PSP or custodian is declared and not archived.

## Not routes

No route registers a merchant, lists a user's merchants, or manages a team, API
keys or the merchant directory: those are the `server` package's methods, the
server's AuthKit and the `openrails` CLI ([server package](../server-package.md)).
The operator's `/metrics` is on the private listener.
