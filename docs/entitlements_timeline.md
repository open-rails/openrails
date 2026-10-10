# Entitlements follow the product

An entitlement is a plain string key (`"premium"`, `"course:101"`). Keys belong to
**products**; customers hold **products**. A customer holds a key at instant T when a
product they hold at T granted that key at T. Nothing stores a customer's keys: they
are derived at check time.

- **Edit a product, and every holder follows.** Adding a key grants it to every current
  holder at once; removing one takes it away unless another product they hold grants it.
- **There are no per-key grants.** Staff grant products, free (see below). A key with no
  natural product gets a product that is not for sale (one with no live price).

## The row model

- `billing.product_entitlements`: a product's keys with valid-time history
  (`added_at`, `removed_at`; at most 10,000 live keys per product). A past `at` reads
  the keys the product had then.
- `billing.product_access`: the customer's windows of a product, `[starts_at, ends_at)`
  (`ends_at` NULL = indefinite), with `source_type` (`purchase`, `subscription`, `grace`,
  `grant`), `source_id`, `revoked_at`. Each window projects one access grant of
  `billing.grants`, the append-only grant ledger.

A window is live at T iff `starts_at <= T AND (ends_at IS NULL OR ends_at > T) AND
revoked_at IS NULL AND deleted_at IS NULL`. An archived product keeps deriving its keys
for its holders.

## Reading access

Admin API (`Permissions.AdminRead`):

- `GET /v1/admin/entitlements?customer_id=&entitlement=&prefix=&at=&cursor=&limit=` —
  the keys customers hold at one instant, by customer then key in byte order
  (`Client.ListEntitlements`). `customer_id` names up to 100 customers, comma
  separated; `entitlement`, repeated, keeps up to 100 keys, so a key absent from the
  answer is not held; `prefix` keeps the keys under one prefix. Without `customer_id`,
  one `entitlement` lists the customers holding it. Each row's `quantity` is the most
  seats a held per-seat product grants the key (null when none is per seat). A host
  minting tokens for a page of users reads all of them in one call.
- `GET /v1/admin/product-access?customer_id=&product_id=&live=` — products held,
  bought, subscribed or granted (`Client.ListProductAccess`); `live=true` with a
  `product_id` is a product check, and each window's `quantity` is the seats it gives.
- `GET /v1/me/entitlements` and `GET /v1/me/product-access` — the signed-in customer's own.

A prefix is bytes OpenRails gives no meaning; its last byte must be printable ASCII. Every
list is `{data, next_cursor}` with keyset cursors.

## Writing access

- **Purchases and subscriptions** open windows of the product bought: a purchase for its
  price's `access_duration_hours` (stacked after the customer's live window of the same
  product), a subscription period for its accepted duration, renewal grace while a
  renewal is retried. Accepted orders freeze price and terms, not keys.
- **Free grants**: `POST /v1/admin/product-access` (`Client.CreateProductAccess`)
  grants up to 100 products across customers, all or none, each with `hours` (extends
  after the customer's latest live window of the product), `ends_at`, or neither
  (indefinite), a `reason` and a `note`. The grant records who granted it. A retry with the same `Idempotency-Key` header answers
  the first grants.
- **Revoke** one window with `DELETE /v1/admin/customers/{customer_id}/product-access/{id}`
  (`Client.DeleteProductAccess`). Refunds and chargebacks revoke their payment's window.
- **Catalog edits**: `UpdateProduct` and catalog applications set a product's keys; an
  application's `entitlement_replacements` (`[{from, to}]`; an empty `to` removes `from`)
  moves a key to another across every product in the same edit. Each edit reports, per product, the keys
  added and removed and how many customers held it, and queues a
  `product.entitlements_changed` host event.

## Never infer access from subscription rows

Subscription `status` is billing lifecycle. An active subscription can have an expired
short access window; a canceled one can keep long or indefinite access. Read product access
and the keys it derives.
