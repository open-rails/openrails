# Rails

A rail is a gateway kind; a PSP is a merchant's account on one. The rail registry
declares what each rail does; purchase, catalog application and the workers read
it, and a request a rail cannot serve is refused, never dropped.

| | NMI | Stripe | CCBill | Solana |
|---|---|---|---|---|
| One-time purchase | yes (gateway sale) | yes | no | yes (Solana Pay) |
| New subscription | OpenRails collects on the saved card | OpenRails collects on the saved card | an order's sale on the price's FlexForm; CCBill bills it | the price's published on-chain plan |
| Trial first phase | no | no | no | no |
| Renewals of an imported provider schedule | NMI charges; OpenRails retries declines | Stripe charges and retries | CCBill charges and retries | n/a |
| Card entry | Collect.js token, or server card entry when the PSP declares it | Stripe Elements; a hosted redirect for one-off without a publishable key | CCBill's page | a wallet |
| Saved cards managed through OpenRails | yes | saved through setup; changed in Stripe's billing portal | no | n/a |
| Charge a saved card for a usage order | yes | yes | no | no |
| Customer cancel | reversible until the scheduled delete runs | reversible until period end | final; access runs to the paid date | reversible until period end; optional wallet cancellation |
| Tier change | upgrade now with proration, downgrade at period end | same as NMI | upgrade by redirect; no downgrade | signed by the customer's wallet |
| Refund through the rail | yes | yes | no (`refund_unsupported`) | no (`refund_unsupported`) |
| Catalog objects | existing plans mirrored read-only | existing products and prices mirrored read-only | link only: `flex_id` and `recurring_billing_option_id` | a recurring price's plan, published on catalog apply |
| Events | signed webhooks | signed webhooks | webhooks from CCBill's address ranges | chain polling |

OpenRails never creates or edits NMI or Stripe catalog objects. Per-rail setup:
[NMI](nmi.md), [Stripe](stripe.md), [CCBill](ccbill.md), [Solana](solana.md).
Who holds a card is a separate axis: [payment-method custody](../payment-method-custody.md).

## Evidence

CI exercises NMI and Stripe through deterministic provider transports; that is
regression coverage, not provider qualification. A sandbox or live result
qualifies only the exact operation exercised, and its record names the account
posture, request, response and date, never credentials or amounts. A change to a
rail's wire behaviour (request fields, parsing, endpoints, signing) is unverified
until it is exercised against the provider again.
