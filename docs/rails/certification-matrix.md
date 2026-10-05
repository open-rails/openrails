# Rail certification matrix

The required merge gate uses deterministic provider transports in the focused
`ci/` contracts. It does not claim that a fake provider response is a
real PSP qualification.

| Evidence | Scope | Required gate |
|---|---|---|
| E2E NMI | Provider-owned import and engine/provider subscription lifecycle | Required PR contract |
| E2E Stripe | Hosted checkout, signed webhooks, and engine/provider subscription lifecycle | Required PR contract |
| Live NMI/Stripe/CCBill/Solana | Real provider or chain behavior | Explicit operator qualification outside merge CI |

A sandbox or live provider result qualifies only the exact operation exercised.
It must name the account posture, request shape, response evidence, and date.
The e2e suite remains the source of deterministic regression coverage;
provider qualification remains a separate operational activity.

## Statuses

| Status | Means | Evidence required |
|---|---|---|
| `live-verified` | The wire was exercised against the provider's **real** gateway — production account, real money or real account state. | A dated probe record or a test run naming the account posture. Covers only the exact operation probed. |
| `sandbox-verified` | The wire was exercised against the provider's **sandbox / test-mode / devnet** endpoint by a named automated test. | Test function name + the CI lane that runs it + its cadence. |
| `modeled` | The adapter models the operation, but current provider verification is absent. Hermetic regression coverage must be checked separately. | Nothing beyond source. This is the default for anything not verified. |
| `limited` | Works, with a named restriction that changes what a customer can do. The restriction is a fact of the rail or a deliberate design choice — not a code gap. | The caveat must name the restriction. Verification level appears in the evidence line. |
| `unsupported` | The flow does not execute on this rail. Marked **(guarded)** when a request is refused with an error, **(silent)** when the input is accepted and ignored. | — |

`unsupported (silent)` is a defect class, not a design: it means a merchant can
declare something the rail will quietly drop. Those cells are called out below.

### What demotes a status

- **A wire change demotes.** Changing a rail adapter's request or response
  shape drops every affected cell to `modeled` until it is re-verified. This is
  the rule the PR process below enforces.
- **A dead test demotes.** A `sandbox-verified` cell whose test is deleted,
  permanently skipped, or has not run green in 60 days decays to `modeled`.
- **Verification does not generalize.** `live-verified` on one operation says
  nothing about its siblings. A verified `cancelSubscription` does not certify
  `refundTransaction` on the same endpoint.

## What each rail does

The rail registry declares these; checkout, catalog application and the workers
read them, and a request a rail cannot serve is refused, never dropped.

| | NMI | Stripe | CCBill | Solana |
|---|---|---|---|---|
| One-time purchase | yes (gateway sale) | yes | no | yes (Solana Pay) |
| New subscription | OpenRails collects on the saved card | OpenRails collects on the saved card | no: existing subscribers only | the price's published on-chain plan |
| Trial first phase on a new subscription | no | no | no | no |
| Renewals of an imported, provider-scheduled subscription | NMI charges; OpenRails retries its declines | Stripe charges and retries | CCBill charges and retries | n/a |
| Card entry | Collect.js token, or server card entry when the PSP declares it | Stripe Elements; a hosted redirect for one-off without a publishable key | CCBill's own page | none: a wallet |
| Saved cards managed through OpenRails | yes | saved through setup; changed in Stripe's billing portal | no | n/a |
| Charge a saved card for an invoice | yes | yes | no | no |
| Customer cancel | reversible until the scheduled delete runs | reversible until period end | final; access runs to the paid date | signed by the customer's wallet |
| Merchant cancel | yes | yes | yes | no: `customer_action_required` |
| Tier change | upgrade now with proration, downgrade at period end | upgrade now with proration, downgrade at period end | upgrade by redirect; no downgrade | signed by the customer's wallet |
| Refund through the rail | yes | yes | no (`refund_unsupported`) | no (`refund_unsupported`) |
| Catalog push | recurring plans, find-or-create | products, prices and features, find-or-create | link only: the operator supplies `form_name` and `flex_id` | plans, find-or-create |
| Events | signed webhooks | signed webhooks | webhooks from CCBill's address ranges | chain polling |

Per-rail setup and caveats: [NMI](nmi.md), [Stripe](stripe.md),
[CCBill](ccbill.md) (and its [refunds](ccbill-refund-qualification.md)),
[Solana](solana.md). Who holds a card is a separate axis:
[payment-method custody](../payment-method-custody.md).

## Maintenance

Three rules, and they are the whole process:

1. A PR that changes a rail adapter's **wire behavior** — request fields, response
   parsing, endpoints, signing — must update the affected cells in the same PR.
2. If the change was not re-verified against the provider, the new status is
   `modeled`. Do not carry a stale `live-verified` across a wire change.
3. When a live or sandbox verification happens, record the date, the environment,
   and the command or test name. Never the credentials, account ids, or amounts.
