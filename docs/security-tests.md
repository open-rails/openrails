# Security tests

Each important attack on an embedded, multi-replica OpenRails is a permanent
test in the required `End-to-end` job (`scripts/greenfield.sh`): real
PostgreSQL, fake NMI/Stripe transports, mounted HTTP routes, and the embedded
and remote Client. Tests live in `ci/greenfield/subscriptions/security_*_test.go`
unless noted. Fixes are tracked as SEC items in the OpenRails tracker.

| Threat | Test | Control |
| --- | --- | --- |
| Customer reads or acts on another customer's subscription, card or checkout by id (IDOR) | `TestSecurityCustomerCannotActOnAnotherCustomer` | Every `/v1/me` lookup is bound to the signed-in customer and merchant |
| Customer's card pays for someone else (foreign `payment_method_id` in confirm, subscription or collection method) | `TestSecurityCustomerCannotActOnAnotherCustomer` | Payment methods are owner-checked on every path |
| Merchant B, or staff of merchant A, reads, refunds or cancels merchant A's objects; sells A's price; charges A's saved card | `TestSecurityMerchantIsolation` | Merchant-scoped predicates; principal-pinned merchant selection |
| Customer credential used as merchant authority | `TestSecurityMerchantIsolation` | Merchant routes require merchant authority |
| Forged, stale, future, tampered, unsigned or foreign-secret webhooks; another merchant's endpoint naming this merchant's subscription | `TestSecurityWebhookAuthenticity` | Per-account HMAC, ±5 min tolerance, merchant-bound lookups |
| Double charge by concurrent confirmation across replicas | `TestSecurityConcurrentConfirmChargesOnce` | Customer lock, operation claim, submission fence |
| Over-refund by concurrent refunds across replicas and topologies | `TestSecurityConcurrentRefundsNeverExceedPayment` | Per-payment lock; pending reservations count |
| Two checkouts of one permanent product racing an in-flight charge | `TestSecurityConcurrentPermanentPurchaseChargesOnce` | Unresolved-purchase and ownership guards |
| Two upgrades of one membership racing on two replicas (SEC-26) | `TestSecurityConcurrentUpgradesChargeOnce` | One unresolved tier change per subscription, including engine upgrades (unique index) |
| Tier change into or out of an ungrouped product (SEC-26) | `TestSecurityTierChangeStaysInGroup` | Both products must share a declared tier group |
| Refunded stacked pass or revoked future grant restored by convergence (SEC-25) | `TestSecurityRevokedAccessStaysRevoked` | Retracting a window terminates its grant |
| Replayed completion after a full refund grants again (SEC-25) | `ci/greenfield/security_test.go` `TestSecurityRefundedPurchaseIsNotRegranted` | A purchase projects access once |
| Automation or unknown-class customer credential starts a card charge (SEC-27) | `TestSecurityAutomationCredentialCannotCharge` | Upgrade and saved-card checkout require the customer's interactive session |
| `findings:resolve` retargets a recommendation at another payment or subscription (SEC-28) | `TestSecurityFindingOverrideCannotRetarget` | Overrides cannot change the ids a recommendation names |
| Browser-chosen cheaper price, archived price, negative price or duration, tampered confirm fields | `TestSecurityCheckoutTermsAreServerSide` | Catalog-derived terms; DB amount and duration checks |
| Live Stripe key reachable through an injected transport in a sandbox deployment (SEC-31) | `TestSecurityProviderConfigurationSafety` | Live keys are disarmed under sandbox posture |
| Merchant-configured Collect.js origin skims cards (SEC-32) | `TestSecurityProviderConfigurationSafety` | Only NMI's Collect.js is served |
| Card testing through card saves | `TestSecurityCardTestingIsThrottled` | Per-customer and per-address `payment` rate limit |

Known open items (not yet covered by a passing control) are listed in the
tracker's SEC issues with their intended fix.
