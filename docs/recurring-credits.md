# Recurring service credits

An engine-owned recurring price can grant a fixed amount of service credit after
its first successful payment and every successful renewal. Payment and grant
amounts are independent: this example charges USD 20 and delivers 25 credits, using
millionths of the USD-denominated usage unit.

```yaml
schema_version: 1
products:
  credit-plan:
    display_name: 25 credits every 30 days
    ownership: unique
    credit_grant: {currency: USD, amount: 25000000, expires_after_days: 365}
    prices:
      every-30-days:
        currency: USD
        unit_amount: 20000000
        billing_interval_hours: 720
        access_duration_hours: 720
```

The customer's order freezes the promised grant. Each paid renewal retains that
promise even when the product's current grant changes. Its lot becomes spendable
when payment settles; expiry is 365 days from that delivery, independent of the
billing period. Payment, credit and the renewal period commit together. Failed
payments issue no credit; duplicate settlement issues no second lot.

OpenRails schedules these renewals and Stripe/NMI processes the engine's card
charges. Use the existing OrderPayment and AccountBilling components for in-page
payment, card management and cancellation. A Stripe account needs its matching
publishable key and configured card vault. Provider-owned subscriptions and other
rails are not supported for recurring-credit delivery.

Cancellation stops future collections and preserves paid credit lots until their
expiry. Refunds return only cash actually collected and proportionally reverse the
linked credit/promotional lot through purchased-credit accounting. Promotional
credit never repays a cash debt. Purchased credit is not additional cash received.

Credit prices do not support trials or per-seat quantities, and credit subscriptions
cannot change plans or seats. Cancel and buy a new plan instead. A product with
recurring prices cannot switch between access and credit delivery: create another
product, preserving its existing subscribers' accepted terms. Ordinary updates to
the credit amount/expiry affect new purchases, not an existing recurring agreement.

A 30-day period is 720 hours; it is not a calendar month. Calendar-month support is
independent. Apply the catalog explicitly; deploying a new binary does not itself
change the catalog or charge a customer.
