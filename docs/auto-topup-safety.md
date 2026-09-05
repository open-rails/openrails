# Automatic top-up safety

Auto-topup remains opt-in for each customer and billing currency: when available prepaid funds fall below the configured threshold, charge the saved method for the configured fixed amount. The existing one-hour cooldown still applies.

The merchant settings API and merchant manifest accept `auto_topup_safety`:

```json
{
  "auto_topup_safety": {
    "max_daily": 3,
    "max_weekly": 10,
    "max_monthly": 30,
    "declines_before_disable": 3
  }
}
```

Omission uses these defaults. An explicit object sets all four positive integers. Windows are rolling 24 hours, 7 days, and 30 days, evaluated using the billing clock. Limits count submitted charge episodes, including declines, per merchant/customer/currency. They do not sum currencies or perform FX conversion. Retrying the same episode does not consume another slot. The amount is fixed by the account configuration and snapshotted before submission.

A reservation commits before the provider request. Its existing provider-intent UUID identifies the episode. Unknown outcomes retain their reservation and prevent another episode, even after the rolling windows have elapsed. No response, a timeout, or an empty provider search is not proof of a decline. Verification requires an exact provider request/transaction receipt; it never matches by amount and never automatically resends an uncertain submission. A crash after reservation but before sending may therefore need operator verification.

A confirmed success atomically deposits the reserved amount, finalizes the episode, advances the cooldown, and resets consecutive declines. A definitive decline finalizes once and increments the decline counter. At the configured threshold, auto-topup is disabled and one notification is queued for the customer. The existing notification email worker delivers it when email is configured. Unknown or infrastructure errors do not increment the decline counter.

Changing settings or disabling auto-topup blocks a new first send. Already-submitted money is still reconciled and deposited, including after the saved method is deleted. Explicitly re-enabling a disabled account resets its decline streak; it does not erase rolling-window reservations or resolve unknown payments. Updating unrelated settings does not re-enable charging.

Customer billing profiles expose enabled state, consecutive declines, pending verification, rolling counts, and effective limits next to each currency balance. The same failure count is included in account settings. The credit ledger remains the accounting authority; reservation rows preserve safety and exact receipt facts only.
