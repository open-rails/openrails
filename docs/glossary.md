# Glossary

The billing/payments and identity vocabulary in one place. Terms are grouped; each maps
to concrete code (enum, table, or manifest key).

## Identity & control

| Term | Meaning |
|---|---|
| Merchant | The billing/isolation namespace — scopes subscriptions, payments, credits, catalog, webhooks, analytics. `billing.merchants`; every tenant-scoped query carries an explicit `merchant_id` (or `psp_id`) predicate, backed by composite foreign keys. Deliberately controlled by exactly **one** AuthKit group (1:1). |
| Org / permission-group | The AuthKit-side controller of a merchant. The merchant row stores `permission_group_id`; AuthKit decides which users, API keys, and remote applications act for that group. OpenRails carries no auth of its own: the host's `Routes.Auth` admits each request. |
| Customer | The paying account under a merchant: a UUID the host supplies (`billing.CustomerID`); identity is `(merchant_id, id)`. The same person is a separate customer at each merchant. On a customer route the customer is the admitted identity's subject. |
| Subject | The native account a request acts as (`Identity.Subject`, a user or an application): its money and authority are used. A customer route's customer; on a merchant route, whose permission is checked. A user is the same subject on every credential they sign in with. |
| Credential | How a subject proved itself (`Identity.Credential`): a session, a device key, an API key, a signed token or an access token, with its id. Never the subject: rotating a key changes no one. Recorded for audit. |
| Trusted issuer | An OAuth 2.0 authorization server whose RFC 9068 access tokens OpenRails accepts: declared under `resource_server.trusted_issuers`, or a merchant's remote application. Bound to its merchants by OpenRails, never by a token claim. |
| Remote application | AuthKit-registered issuer/JWKS principal nested under a merchant's permission-group: a trusted issuer for that merchant, within its role there. |
| Federated grant | A merchant role an owner grants by email to a trusted issuer's user, who accepts it with that verified email. |
| Invoker | The party actually acting (`Identity.Invoker`): the subject itself, or someone acting on its behalf, possibly another issuer's user spending the subject's balance. Spend delegations, spend limits and staff rate limits key on it; usage records it (`invoker`, `invoker_type`). |

## Money & access

| Term | Meaning |
|---|---|
| Native units | Amounts are integer units at the currency's registered scale (`billing.Currencies()` / `GET /v1/config`'s `currencies`): micros for USD/EUR, 10^4 per yen for JPY; decimal strings on the wire. |
| Money ledger | Double-entry ledger, the source of truth for money. FX inside the ledger is forbidden — no cross-currency transfers. |
| Grant | An immutable event in the append-only grant ledger (`billing.grants`), kind `access` (a window of one product) or `credit`. Revoke/expire/supersede are new events referencing the original; a credit grant IS the FIFO lot. |
| Entitlement | A plain string key (e.g. `premium`) a product grants (`billing.product_entitlements`). A customer holds the keys of the products they hold (`billing.product_access`), derived at check time. See `docs/entitlements_timeline.md`. |
| Grace | A bounded, revocable generosity window (`source_type='grace'`) appended beyond the paid term; revoked or lapsed the moment truth arrives. |

## Rails & PSPs

| Term | Meaning |
|---|---|
| Rail | A payment gateway **kind** OpenRails codes against — `billing.Rail`: `nmi`, `ccbill`, `stripe`, `solana`. One adapter per rail under `internal/integrations/<rail>`. |
| PSP | A merchant's concrete **account on a rail** — credentials + operator-declared `account_id` + manifest key. Row: `billing.psps` (`psps.key`, e.g. `mobius` on rail `nmi`); manifest: `merchants.<slug>.psps.<key>` (its `rail:` names the rail). Catalog `psp_links` and the checkout wire speak PSP keys. Renamed from `rail_merchant_accounts` (earlier `provider_accounts`) — the retired names fail loudly, no aliases. |
| Custodian account | A merchant's concrete **account with a custodian** — credentials + operator-declared `account_id` (the vendor's tenant id) + manifest key. Row: `billing.custodians` (`custodians.key`); manifest: `merchants.<slug>.custodians.<key>` (its `kind:` names the vendor). Referenced by `psps.custodian_id`, so several PSPs can charge cards out of one vault. |
| `account_id` | Operator-declared, opaque PSP (or custodian) label — never derived from credentials at runtime, on **every** rail. It is a segment of the merchant-secret path (`psps/<rail>/<env>/<account_id>/<key>`, `custodians/<kind>/<env>/<account_id>/<key>`), so deriving it from a credential would need the credential, which needs the path, which needs the id. NMI = the dashboard Gateway ID; Stripe = `acct_…`; CCBill = `clientAccnum-clientSubacc` (dash-joined); Solana = derived from the signer pubkey (a declared value is warned and ignored). |
| Custodian | Who **holds** a stored card, orthogonal to who charges it — `payment_methods.custodian`: `psp` (inside the processor: a Stripe `pm_`, an NMI customer vault) or `basis_theory` (a neutral third-party custodian proxying the PAN into the rail's gateway). Declared ONCE per merchant as `custodians.<key>` (its `kind:` names the vendor) and referenced by each PSP that charges its cards (`custodian: <key>`); a custodian is NEVER a rail. Never empty; "no stored instrument" (CCBill, Solana) is the absence of a `payment_methods` row. See `docs/payment-method-custody.md`. |
| Channel | How a payment reached OpenRails — `payments.channel`: `rail` (through a gateway, with its `rail` and `psp_id`) or `manual` (an off-channel payment the merchant recorded; no rail, no PSP). |
| Armed | A rail is usable for a merchant iff it has an active PSP row, resolved per-merchant at request time through the one seam `internal/railresolve` (fail closed on `ErrRailNotArmed`). |
| Integration | The Go client speaking a rail's external API: `internal/integrations/{nmi,stripeapi,ccbill,solana}`. All Stripe HTTP goes through `stripeapi`; all NMI HTTP through `nmi`. |
| Provider intent | A durable outbox row (`billing.provider_intents`) posted before **every** outbound provider mutation, executed effectively-once by a scheduled runner. Outcomes: succeeded, retryable, `unknown_needs_verify` (ambiguity ⇒ verify via provider reads, never blind retry), terminal, parked. |
| Processor (NMI wire) | NMI's own name for its backend acquiring processor (`processor_id`, `processor_response_text`, decline strings). External wire format — a different concept from our rail; never renamed. |

## Operating knobs

| Term | Meaning |
|---|---|
| Credential backend (`secret_backend`) | `snapshot`: externally owned, in-memory credentials; `db`: encrypted managed credentials; `vault`: exact published Vault references. Credential read/write capability and external configuration HTTP publication are independent. Merchant metadata always lives in PostgreSQL. |
| Deployment shape | Only how the process/routes are hosted: **embedded** (a Go host runs `openrails.New` and mounts `/billing/v1/*`), **standalone** (`openrails run-server` serves `/v1/*`), **OpenRails-SaaS** (one hosted standalone engine, one merchant binding per tenant client). Every shape selects credential custody independently; application code uses the same `*openrails.Client` in all three. |
| `provider_write_mode` | How much OpenRails may do against providers: `full` (normal) / `limited` (no system-initiated writes) / `readonly` (no writes). Required: boot refuses it unset. |
| `test_mode` | Credential posture: `sandbox` or `live`, two explicit states. Sandbox attaches credential guarantees (live Stripe keys refuse boot, NMI accounts probed, CCBill sandbox URL, Solana devnet). Independent of environment — production can legitimately run sandbox rails. |

## Reconciliation

| Term | Meaning |
|---|---|
| Finding planes | The four diagnostic planes of the truth model: **pull** (provider is authoritative — pull observed truth in), **derive** (grant effect vs source ledger), **life** (record vs clock + state machine), **consistency** (duplicates, amount mismatches, dangling references). Finding types are `<plane>.<subject>.<shape>`, e.g. `pull.charge.missing`. |
| Shapes | `missing` / `excess` / `mismatch` — exhaustive; repaired by materialize / retract / adjust. Unevaluable cases are the INDETERMINATE state, not a shape. |

See [operations.md](operations.md#the-convergence-engine) for the full reconciliation model and
`docs/merchant-guide.md` for catalog vocabulary.
