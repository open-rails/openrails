# Prepaid credits

A product with a `credit_grant` tops up a currency balance that priced usage draws
down ([usage billing](usage-billing.md)). It is a monetary balance, not a request
or token counter: holds and reads call it the built-in limit `balance`, and it is
never declared as a limit. Show it in your own unit (1 credit = 1¢) if you like;
a cap in requests or tokens is a [limit](usage-billing.md#limits).

Usage draws the balance once a day: a customer's day of usage in a currency posts
as one deduction after the day ends, or earlier before the balance next moves
(a top-up, an expiry, a revocation or refund). Until then the balance holds it.

```yaml
schema_version: 1
products:
  api-credit-10:
    display_name: $10 prepaid API balance
    credit_grant:
      currency: USD
      amount: 10000000          # $10 of balance, in native units
    prices:
      purchase: { amount: 10 USD, billing_interval: null }
  api-credit-100:
    display_name: $100 prepaid API balance
    credit_grant: { currency: USD, amount: 100000000, expires_after_days: 365 }
    prices:
      purchase: { amount: 80 USD, billing_interval: null }   # a bulk discount
  api-deposit:
    display_name: Top up your prepaid API balance
    credit_grant: { currency: USD, from_payment: true, expires_after_days: 365 }
    prices:
      deposit:
        amount: 0 USD           # not free: the buyer names the amount
        customer_amount: { min_amount: 1000000, max_amount: 1000000000 }
        billing_interval: null
```

`credit_grant.amount` and `customer_amount` bounds are numeric native units, not
the `amount` alias. `from_payment: true` grants exactly what was paid.

- Each successful purchase creates one credit lot, idempotently; buying again
  makes another. Credit-only products grant no ownership that would block a
  repeat purchase.
- The amount and expiry are fixed at purchase. Expiry runs from payment
  (`expires_after_days`, default 365). Catalog changes affect new purchases only;
  archiving a pack stops sales and leaves granted credit alone.
- A customer-amount price is ordered with the amount on its line, in the browser
  (`POST /v1/me/orders` with `{"lines": [{"price_id": "…", "amount": "100000000"}]}`)
  or for the customer from your backend, unpaid until they pay it:

  ```go
  amount := int64(100_000_000) // $100
  order, err := client.CreateOrder(ctx, billing.CreateOrderParams{
      CustomerID: customerID,
      Lines:      []billing.OrderLineParams{{PriceID: depositPriceID, Amount: &amount}},
  })
  ```

  The amount cannot change once ordered; fixed-price packs refuse one.
  Customer-chosen amounts sell on Stripe and NMI. Recurring credit benefits are not
  supported.
- A voluntary refund needs the lot's credits unused and unheld (the day's usage
  posts first); a forced reversal uses the source lot and keeps the rules for usage
  already authorized.
- Staff fund balances directly with `POST /v1/admin/credit-grants`
  ([merchant operations](merchant-operations.md#credit-grants)).

To change what new purchases get, apply a new `credit_grant` (it replaces the whole
policy; omitted, it is preserved):

```yaml
schema_version: 1
products:
  api-credit-100:
    credit_grant: { currency: USD, amount: 100000000, expires_after_days: 180 }
```
