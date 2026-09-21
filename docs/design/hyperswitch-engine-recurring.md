# Engine-owned subscription collection prototype

This draft prepares explicit subscription collection policy and locked renewal admission. It does not register a renewal worker or enable HyperSwitch subscription enrollment. Those capabilities require the accepted initial-membership writer from #576, qualified recurring customer consent/payment, and the full browser-to-worker renewal proof.

The fresh-database cut replaces payment-method rebill_driver with one immutable subscription collection_policy: provider, provider_dunning, or engine. Native provider scheduling remains available. Custody remapping does not change collection ownership, and this stage does not migrate provider subscriptions to engine ownership.

The first engine scope is positive fixed-price NMI settlement using qualified HyperSwitch permanent cards. Free, trial and zero-price initial engine enrollment are unsupported. Engine subscriptions have no fabricated provider subscription identifier. The existing ledger, receipts, retry state, immutable grant effects and River fleet remain authoritative.

One accepted renewal freezes its payer, account, card, recurring agreement, price, benefits and period. After a whole missed period, admit one new period from the recovery admission timestamp, with no backlog collection. Preserve the prior subscription period end separately in the accepted payload to retain the stale-state guard. Uncertain recovery retains the same accepted period and never resubmits or moves the period to verification time. This policy does not forgive or mutate existing past debt.
