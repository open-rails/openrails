# Account usage billing

Status: accepted. Implementation tracked by OpenRails #1289.

A customer may accumulate usage debt within a merchant's configured credit limit.
The customer must authorize a default payment method before new work can spend
credit. Recording usage remains a statement of work already performed, so a
failed admission does not erase a billable fact.

Invoice pending usage when its amount reaches the collection threshold or its
fixed monthly billing date arrives. A threshold invoice never moves that billing
date. The collection threshold cannot exceed the customer's debt limit.

Apply the minimum invoice amount before creating an invoice. Smaller accruals
stay pending and join a later invoice, including across month boundaries. The
ledger remains authoritative for the amount owed, independently of whether that
debt has been attached to an invoice.

Use the same account billing anchor for invoice periods and graduated usage
pricing. Dates 29–31 clamp to the shorter month's last day without changing the
original anchor. All boundaries use UTC.

Payment collection retains the existing durable operations, default-method
consent checks, retries and uncertain-result recovery. Automatic collection may
not charge a different saved method when the default fails or disappears.

A host explicitly enables usage billing. Subscription-only hosts do not run
usage-billing scans. One due-billing path replaces the separate monthly-floor
pass; threshold crossings wake that path without creating one job per usage
report. A due scan catches missed wakeups and account billing dates.
