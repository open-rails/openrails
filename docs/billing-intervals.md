# Billing intervals

An OpenRails-owned recurring price can bill on a fixed duration or a calendar
interval. These are different financial terms:

```yaml
products:
  membership:
    display_name: Membership
    prices:
      monthly:
        amount: 20 USD
        billing_interval: 1 month
      thirty-days:
        amount: 20 USD
        billing_interval: 30 days
```

`30 days` is exactly 720 elapsed hours. `1 month` keeps the original billing day
and UTC time of day. A January 31 purchase renews on February 28 (29 in a leap
year), then March 31 and April 30. Clamping February does not replace the original
anchor. A February 28 purchase retains the 28th. Two-month intervals retain their
original month phase. `1 year` is twelve calendar months; `365 days` is a fixed
duration.

The Go/API catalog uses `billing_interval_hours` or `billing_interval_months`
(1–1200), never both. Existing hour-based prices retain their values, IDs and
accepted schedules. The readable `billing_interval` alias normalizes to the same
fields. Access and trial durations continue to accept fixed hours, days or weeks.

The initial successful order fixes the calendar anchor. Its immutable line and
each accepted renewal retain the month interval, original anchor, cash amount and
access choice. Late workers, retries and restarts do not shift the schedule.
Collection after an entire missed period still requires the existing explicit
recovery decision; it does not automatically charge missed months. Catalog edits
do not change a subscriber's accepted terms.

A calendar price with no explicit access duration grants access through each
actual billing period. An explicit `access_duration` remains independent. Credit
grant expiry also remains independent; choosing monthly billing never shortens
an already issued credit lot.

Calendar-priced purchases currently use Stripe or NMI card collection scheduled
by OpenRails. Calendar trials, per-seat quantities, Solana plans and subscription
plan changes are refused before provider work. Canceling and resuming within the
paid period preserves the anchor. Provider-owned prices continue to use their
provider's observed periods and calendar semantics.

The calendar is UTC. This does not introduce merchant-local timezone or daylight
saving scheduling. UI labels distinguish `monthly` / `every N months` from
`every 30 days`.
