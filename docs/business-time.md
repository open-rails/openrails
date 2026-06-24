# Business Time and Test Clocks

OpenRails has two kinds of time:

- **Business time** controls billing state: subscription periods, entitlement
  validity, cancellation timestamps, renewal windows, dunning retries, checkout
  session expiry, credit expiry, and credit hold expiry.
- **Infrastructure time** controls process mechanics: cache TTLs, rate limits,
  webhook signature tolerance, queue receipt timestamps, retry backoff, metrics
  durations, and external quote freshness.

Business-time code must use the runtime `clockwork.Clock`. Infrastructure-time
code may use wall-clock time when wall-clock behavior is the thing being tested.

## Runtime Clock

The application runtime owns one clock. Production boot defaults that clock to
`clockwork.NewRealClock()` at the composition boundary. Tests can inject a fake
clock before runtime construction:

```go
start := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
clock := clockwork.NewFakeClockAt(start)
suite := setupTestSuite(t, WithSuiteClock(clock))

// Create data at fake time.
products := suite.SeedProducts()
sub := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
    UserID:  userID,
    PriceID: products[0].Prices[0].ID,
    Status:  models.StatusActive,
})

// Advance without sleeping.
clock.Advance(30 * 24 * time.Hour)
```

New tests should prefer `WithSuiteClock` over `SetMockClock`. `SetMockClock`
exists as a compatibility helper for older tests that patch the shared runtime
after construction.

## Rail Test Clocks

Rail-side test clocks and sandboxes are separate from OpenRails app time.
For example, Stripe Test Clocks control Stripe test customers, subscriptions,
invoices, and webhook timing. The OpenRails fake clock controls how this app
interprets time when processing those webhooks and updating local records.

When testing rail integrations, advance both sides deliberately:

- Use the rail sandbox/test clock to produce realistic external events.
- Use the OpenRails fake clock to verify local billing, entitlements, credits,
  and retry windows.

## Guardrail

Run:

```bash
bash scripts/check_business_time.sh
```

The guardrail scans business/domain paths for direct `time.Now()`, SQL `NOW()`,
`CURRENT_TIMESTAMP`, and `clockwork.NewRealClock()`. Existing allowed usages are
classified in `docs/business-time-allowlist.txt`. New business-time logic should
inject or pass the runtime clock instead of adding another allowlist entry.
