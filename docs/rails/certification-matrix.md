# Rail certification matrix

The required merge gate uses deterministic provider transports in the focused
`ci/greenfield` contracts. It does not claim that a fake provider response is a
real PSP qualification.

| Evidence | Scope | Required gate |
|---|---|---|
| Greenfield NMI | Provider-owned dunning import and engine-owned admission | Required PR contract |
| Greenfield Stripe | Hosted checkout replay and signed webhook convergence | Required PR contract |
| Live NMI/Stripe/CCBill/Solana | Real provider or chain behavior | Explicit operator qualification outside merge CI |

A sandbox or live provider result qualifies only the exact operation exercised.
It must name the account posture, request shape, response evidence, and date.
The greenfield suite remains the source of deterministic regression coverage;
provider qualification remains a separate operational activity.
