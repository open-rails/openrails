# OpenRails functionality and focused coverage

This inventory distinguishes behavior exercised by the focused suite from
features merely present in the implementation. It is not a line-coverage
percentage. Ordinary unit, wire-contract, static and security checks run
separately; the deleted legacy integration corpus is not current evidence.

The focused runner must select `./ci/greenfield/...`, including the subscription
package. Passing only the parent package does not qualify renewal or dunning.
All provider traffic here is fake; no result certifies a live merchant account.

| Functionality | Current focused coverage | Remaining important cases |
| --- | --- | --- |
| Exact money and currency units | Decimal parsing, signed integer bounds, rounding, USD/JPY rail conversion, unknown currency rejection and JSON string amounts beyond JavaScript's exact integer range | More currencies, tax/discount calculations where supported, every outgoing provider money field |
| Products, prices and offers | Create, retrieve, list, ensure/replay, entitlement-to-offer lookup | Atomic catalog applications, revisions, archive/reprice offer history, cursor pagination, creator catalog boundaries |
| Merchant and customer separation | Foreign product read refused, merchant lists separated, entitlement lookup isolated | Adversarial writes and customer impersonation across every privileged surface |
| One-time checkout | Hosted Stripe session, replay without another create, conflicting request rejection, unpaid access absent | Saved-card NMI/Stripe one-time completion, account selection, uncertain sale recovery |
| Paid access and entitlements | Signed paid webhook creates access; subscription periods and declines affect access | Bundles, partial ownership, revocation sources, paginated purchased-product history |
| Engine-owned NMI and Stripe subscriptions | Real customer confirmation and saved card, repeated due renewals, amounts/payments agree with fake provider ledger | Cross-account migrations, unsupported combinations and very long delinquency histories |
| Engine dunning | Soft, card-fixable and terminal declines; retry timing, card replacement and recovery; destructive-action switch | More provider refusal codes and interrupted retry interleavings |
| Provider-owned NMI and Stripe schedules | Import/replay, provider renewal and failure notifications, late/duplicate events, cancellation | Account-specific provider discovery and missed-notification backfills |
| NMI schedule with OpenRails dunning | Explicit `provider_dunning` import, failed scheduled renewal, timed OpenRails rebill, exact receipt/period/access, replay and conflicting recurring-reference refusal | More decline categories and customer/provider races around the next regular charge |
| Subscription changes | Cancellation/resumption, host account-deletion cancellation, repricing, replacing a card | Full tier-change/proration matrix, trials and bulk plan migration |
| Refunds | Engine subscription renewal refunds, repeated refund requests and provider notifications | One-time partial refunds, concurrent over-refund refusal, archive-with-refund/review operations |
| Payment uncertainty and restart | Engine request interruption/restart, late receipts, duplicate refusal and abandoned authentication | Every sale/refund/cutover uncertainty path; no test may infer no charge from a timeout |
| Webhooks | Signed Stripe completion/replay/stale expiry, provider-owned subscription notices | Forged signatures, wrong account, thin-event URL validation, CCBill and Solana notifications |
| River jobs | Real shared fleet, scheduled renewal, operation recovery, interrupted rescue-worker recovery and restart after completed rescue work | Host/managed ownership permutations, arbitrary schema migration concurrency, stalled queue health |
| Embedded/HTTP client parity | Subscription scenarios run embedded and remote clients against mounted HTTP routes | Full catalog, treasury, configuration and archive parity; standalone executable boot |
| PostgreSQL initialization | Fresh schema, public migration entry point, replay and runtime startup | Concurrent bootstrap, one-connection pool, privilege grants, drift refusal and restored databases |
| Saved payment methods and custody | NMI/Stripe setup and subscription card changes | Vault deletion with active agreements, Basis Theory/HyperSwitch custody, credential rotation |
| Provider safety/configuration | Engine write-mode holds and explicit embedded write mode; fake sandbox posture | Live-key rejection for all providers, read-only enforcement on every mutation, secret/public configuration isolation |
| Treasury/credit ledger | No focused database scenario yet | Deposit replay, balanced append-only transfers, insufficient funds, holds/capture/release, lot expiry, concurrent spending |
| Usage metering and billing policies | No focused database scenario yet | Rate cards, usage aggregation, catalog vs host pricing authority, limits, delegation and wasted-spend caps |
| Invoices and receivables | No focused database scenario yet | Finalization, rounding, collections, partial payment, pay-now, arrears and delinquency |
| Provider/custodian account migration | No focused scenario yet | Freeze source/target identity, paused destination, exact cancellation receipts, no double billing |
| Merchant configuration and portability | Constructor provisioning exercised incidentally | Versioned configuration, slug forwarding, export/import, retirement, shared-schema purge isolation |
| Authentication/authorization | Neutral host verification used in subscription routes; some permission boundaries exercised incidentally | Explicit negative user/machine/delegated tests, live permissions, issuer/audience and tenant authority |
| Other rails | NMI/Mobius and Stripe modeled | CCBill lifecycle and constraints, Solana transfer/signature/recurring paths, external vault adapters |
| Self-service, administration and reporting | Subscription and payment reads used as assertions | `/me` pagination, notifications, billing portal, analytics, metrics/copilot, readiness, rate limits, CLI and browser UX |

The highest-value additions after both NMI ownership paths are treasury/ledger
atomicity, uncertain one-time payments/refunds, authorization denials and schema
concurrency. Cheap pure tests should cover arithmetic boundaries directly;
database workflows should assert financial effects and provider request counts.
Using `int64` alone does not prevent overflow, incorrect scaling or precision
loss in a browser's JSON decoder.

NMI distinguishes the next regular scheduled payment from a failed-payment
retry; a future next-charge date alone does not prove payment. The focused
hybrid case models that distinction from the [NMI recurring guide](https://support.nmi.com/hc/en-gb/articles/33210833988241-Recurring-via-the-Virtual-Terminal-Plans-and-Subscriptions)
and [subscription recovery guidance](https://support.nmi.com/hc/en-gb/articles/16096543375505-Subscription-Reports).
