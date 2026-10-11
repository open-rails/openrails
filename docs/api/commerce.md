# Checkout

A browser buys through an **order**: priced lines read and paid by their own
customer on `/v1/me/orders` ([frontend integration](../frontend-integration.md#orders)).
The merchant's backend may create an unpaid order for a customer (`CreateOrder`);
only that customer pays it. A **checkout attempt** (`chk_`) is one charge of an
order on one PSP; each pay creates one.

## Merchant client

| Client method | HTTP operation | Permission |
| --- | --- | --- |
| PreviewOrder | POST /v1/app/orders/preview | `merchant:orders:manage` or `root:orders:manage` |
| CreateOrder | POST /v1/app/orders | `merchant:orders:manage` or `root:orders:manage` |
| GetOrder, ListOrders | GET /v1/admin/orders[/{id}] | `merchant:customer:read` or `root:customer:read` |
| CancelOrder | POST /v1/app/orders/{id}/cancel | `merchant:orders:manage` or `root:orders:manage` |

`PreviewOrder` prices lines for one customer and lists the `payment_options`
that can take them, in routing order; options report local readiness and never
probe a gateway. `CreateOrder` freezes the lines into an `open` order; the
customer pays it with `POST /v1/me/orders/{id}/pay`, signed in, or with
billing-ui's `<OrderCheckout orderId>`.

A pay answers the order. `payment.status` is `succeeded`,
`requires_payment_method` (a decline: `402` with `last_payment_error`),
`processing` (read it again) or `requires_action` with a `next_action`:

| `next_action.type` | The buyer |
| --- | --- |
| `redirect_to_url` | opens `url` (Stripe's hosted page) in the top window; it returns to `return_url` |
| `solana_pay` | scans or opens `url`, a `solana:` Solana Pay link served by `/v1/orders/{id}/solana-pay` |
| `authenticate` | completes the bank's challenge in the PSP's script, then `POST /v1/me/orders/{id}/confirm` |

A declined card leaves no subscription and no saved card. A provider refusal
is a coded 402/502 (see [errors](errors.md#payment-refusals)); the attempt is
recorded as failed and the customer pays again with another instrument.

Amounts use native currency units (micros for fiat) as decimal strings in JSON
and `int64` in Go. `created_at`/`expires_at` are RFC3339 instants
([errors and wire rules](errors.md)).
