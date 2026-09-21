# Engine-owned subscription collection prototype

This draft prepares explicit subscription collection policy and locked renewal admission. It does not register a renewal worker or enable HyperSwitch subscription enrollment. Those capabilities require the accepted initial-membership writer from #576, qualified recurring customer consent/payment, and the full browser-to-worker renewal proof.

The fresh-database cut replaces payment-method rebill_driver with one immutable subscription collection_policy: provider, provider_dunning, or engine. Native provider scheduling remains available. Custody remapping does not change collection ownership, and this stage does not migrate provider subscriptions to engine ownership.

The first engine scope is positive fixed-price NMI settlement using qualified HyperSwitch permanent cards. Free, trial and zero-price initial engine enrollment are unsupported. Engine subscriptions have no fabricated provider subscription identifier. The existing ledger, receipts, retry state, immutable grant effects and River fleet remain authoritative.

One accepted renewal freezes its payer, account, card, recurring agreement, price, benefits and period. After a whole missed period, admit one new period from the recovery admission timestamp, with no backlog collection. Preserve the prior subscription period end separately in the accepted payload to retain the stale-state guard. Uncertain recovery retains the same accepted period and never resubmits or moves the period to verification time. This policy does not forgive or mutate existing past debt.

## Cancellation and resume

Engine cancellation uses the existing local subscription lifecycle and exact subscription ID. It bounds paid access and stops new due admission without scheduling a provider deletion. An already accepted or uncertain renewal stays on the existing operation ledger for recovery; cancellation does not manufacture a payment or discard the operation. Only ordinary user cancellation can be resumed, and only while the current paid period remains valid. Merchant revocation, chargeback and expiry cannot be undone by a queued customer action.

This behavior is qualified through internal engine fixtures, authenticated customer HTTP requests, the public merchant Client and existing River workers. Public engine enrollment and delayed engine settlement remain disabled and unqualified.

## Native recovery reachability gap

Fresh native NMI vault creation previously stamped the card-level provider mode. Billing import also defaulted to provider. Production writes of the old OpenRails mode belonged to custodian remap/import and the Basis Theory instrument creator, whose non-PSP methods are refused by verified-customer manual retry. Qualified native Pay-now and automatic dunning controls therefore do not establish that normal fresh native checkout can reach those paths. The prototype preserves provider as the native default; it does not silently turn on retries.

The existing manual-rebill preparation verifies an active, unpaused provider subscription, next billing date, vault, plan, cadence, installments and amount before freezing submission. It may adjust an accepted repriced amount, but does not suppress the next scheduled provider charge. NMI documents that a declined charge is not automatically retried between schedule dates; the next scheduled charge remains live. Local operation deduplication alone is not proof against a collision at that next boundary. Enabling either customer on-demand recovery or automatic native dunning needs its own qualified provider retry-ownership proof. A trusted merchant account capability could admit provider_dunning after that qualification; an arbitrary customer policy override is not appropriate. Customer-only recovery for provider policy must be evaluated separately from automatic dunning, with the same provider duplicate-charge constraint.

After a definitive declined engine recovery attempt, a later separately admitted attempt may choose a fresh recovery start against the unchanged previous period end. An unresolved or possibly submitted attempt cannot be replaced or have its period moved; only its existing receipt recovery may finish it.

NMI distinguishes retrying by changing the next charge date (counts toward scheduled installments) from retrying as a new sale (does not update that count). See [NMI recurring FAQ](https://support.nmi.com/hc/en-gb/articles/33210833988241-Recurring-via-the-Virtual-Terminal-Plans-and-Subscriptions) and [Subscription Reports](https://support.nmi.com/hc/en-gb/articles/16096543375505-Subscription-Reports). A native recovery policy must preserve finite-plan counts and converge a failed scheduled period followed by a manual payment without purchasing that same period twice. Default policy changes remain held for that qualification.
