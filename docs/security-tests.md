# Security tests

Each important attack on an embedded, multi-replica OpenRails is a permanent
test in the required `End-to-end` job (`scripts/e2e.sh`): real
PostgreSQL, fake NMI/Stripe transports, mounted HTTP routes, and the embedded
and remote Client. Tests live in `ci/subscriptions/security_*_test.go`
unless noted.

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
| Two upgrades of one membership racing on two replicas | `TestSecurityConcurrentUpgradesChargeOnce` | One unresolved tier change per subscription, including engine upgrades (unique index) |
| Tier change into or out of an ungrouped product | `TestSecurityTierChangeStaysInGroup` | Both products must share a declared tier group |
| Refunded stacked pass or revoked future grant restored by convergence | `TestSecurityRevokedAccessStaysRevoked` | Retracting a window terminates its grant |
| Replayed completion after a full refund grants again | `ci/security_test.go` `TestSecurityRefundedPurchaseIsNotRegranted` | A purchase projects access once |
| Stolen owner or staff token with a stale sign-in grants permanent access, refunds, mints credit, imports billing or edits the catalog through any route that serves the operation (import, catalog, merchant API) | `TestSecurityStaleSignInReachesNoOwnerOperation`; `ci/step_up_test.go` `TestSecurityOwnerOperationsNeedRecentSignIn` | Per operation: every staff route marked `sensitive` in [routes](api/routes.md) needs a recent sign-in from the auth provider, 403 `step_up_required` |
| A viewer or read-only API key mints prepaid credit or opens an unsecured credit line | `ci/credit_authority_test.go` `TestSecurityOnlyStaffWritesMintCredit` | Credit grants and the credit-limit write are staff writes (`server.MerchantWrite` on the standalone server) |
| Automation or unknown-class customer credential starts a card charge | `TestSecurityAutomationCredentialCannotCharge` | Upgrade and saved-card checkout require the customer's interactive session |
| `findings:resolve` retargets a recommendation at another payment or subscription | `TestSecurityFindingOverrideCannotRetarget` | Overrides cannot change the ids a recommendation names |
| Browser-chosen cheaper price, archived price, negative price or duration, tampered confirm fields | `TestSecurityCheckoutTermsAreServerSide` | Catalog-derived terms; DB amount and duration checks |
| Live Stripe key reachable through an injected transport in a sandbox deployment | `TestSecurityProviderConfigurationSafety` | Live keys are disarmed under sandbox posture |
| Merchant-configured Collect.js origin skims cards | `TestSecurityProviderConfigurationSafety` | Only NMI's Collect.js is served |
| Card testing through card saves | `TestSecurityCardTestingIsThrottled` | Per-customer and per-address `payment` rate limit |
| Leaked rotated-out webhook secret forges "paid" notices after rotation, Stripe and NMI | `TestSecurityRotatedWebhookSecretExpires` | The previous secret verifies only until `webhook_overlap_expires_at` (default 24h, max 168h), never without one; `retire_webhook_overlap` ends it at once |
| One merchant's card declines put every merchant's API, API keys and consoles included, behind a captcha | `TestSecurityCardAttackModeIsPerMerchant` | Attack mode is per merchant and challenges only its card routes; merchant and API routes never meet a captcha |
| Card testing across replicas with no Redis or captcha | `TestSecurityCardTestingLedgerAcrossReplicas` | PostgreSQL decline ledger per customer, client address and merchant (attack mode); blocked attempts never reach the gateway |
| Junk card saves from a few accounts lock every buyer of a merchant out of card routes for the day (no captcha configured) | `TestSecurityCardAttackModeWithoutCaptcha` | Only provider refusals count; attack mode needs declines from many customers and addresses, slides with its window, and without a captcha blocks only subjects with a recent decline |
| Open redirect through checkout success/cancel URLs | `TestSecurityCheckoutReturnURLsStayOnHost` | Exact-origin `return_origins` allow-list; the billing portal returns only to an allowed origin |
| Another customer pre-claims a predictable tier-change idempotency key | `TestSecurityTierChangeKeysAreCustomerScoped` | Tier-change keys are scoped to the customer |
| Probing another customer's payment-method or subscription ids | `TestSecurityForeignIDsLookMissing` | A foreign id answers exactly like a missing one |
| Enumerating provider accounts through webhook responses | `TestSecurityWebhookResponsesRevealNoAccounts` | Unknown account, missing secret and bad signature share one 401 |
| A merchant owner claims the shared API host, or a domain it does not control, as its `api_host` | `ci/api_host_test.go` `TestSecurityAPIHostNeedsProofOfControl` | An `api_host` routes only after a TXT record at `_openrails-challenge.<host>` carries the claim's token; the deployment's own hosts are reserved; a proven host stays with its merchant |
| Squatting another merchant's provider account id to receive its events | `TestSecurityProviderAccountClaimsNeedProof` | A merchant-API claim needs a successful credential probe; the operator declares otherwise |
| NMI account left in test mode under live posture grants access without payment | `internal/integrations/nmi` `TestLivePostureRefusesTestModeAccount` | Live posture arms NMI only on `test_mode_enabled=false` (read-only query); e2e cannot run live posture because transport injection is refused under live by design |
| Client-written first `X-Forwarded-For` line spoofs the CCBill source allowlist behind a proxy that appends its own line | `TestSecurityForwardedForReadsEveryLine` | Every `X-Forwarded-For` line counts, walked right to left past trusted hops |
| Renewal grace kept after a period-end cancel | `TestEngineCancelAndResume/cancel_at_period_end` | Cancel removes the future grace window |
| Provider refund notice arriving before OpenRails' own refund finalizes records it twice or revokes against `revoke_access=false` | `TestSecurityRefundNoticeDuringOwnRefundCountsOnce` | Provider refund facts wait for redelivery while a refund reservation is pending; finalize takes over an already-recorded refund |
| NMI chargeback of a one-time purchase leaves access granted; unmatched chargebacks vanish | `TestSecurityNMIChargebackRevokesOneTimePurchase` | Chargeback matching covers one-off charges and revokes entitlements and product access; unmatched entries raise a durable repair alert |
| Late `charge.dispute.created` after a won dispute revokes; a won dispute revives an unrelated cancellation | `TestSecurityStripeDisputeOrdering` | Won outcome stored durably; reactivation only for the chargeback cancellation this dispute caused |
| Unsigned CCBill post buys years of access (RenewalSuccess/BillingDateChange date, UserReactivation without payment) | `TestSecurityCCBillPeriodEndsAreBounded` | Period ends capped at one cycle + 72h past the paid anchor; reactivation never extends the paid end |
| Solana transfer landing long after its quote settles at a stale price | `TestSettlementTooLate` (`internal/integrations/solana`) and `TestSolanaPayLatePaymentIsFlagged` | 30-minute late window; later landings grant nothing and raise an operator repair alert |

## Documented decisions

- NMI account claims through the merchant API prove the security key works,
  not that the account id belongs to it. NMI events are HMAC-signed with the
  claimer's own secret, so a squatter cannot receive or forge the owner's
  events; the owner's operator can still reassign the id.
- Checkout sessions keep 403 for another customer's session: ids are random
  UUIDs, so the distinction reveals nothing enumerable.
- Webhook routes must load an account's secret before verifying its HMAC. The
  `webhook` rate-limit bucket bounds that work, and every refusal answers
  alike.
