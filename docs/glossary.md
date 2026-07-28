# Glossary

The billing/payments and identity vocabulary in one place. Terms are grouped; each maps
to concrete code (enum, table, or manifest key).

## Identity & control

| Term | Meaning |
|---|---|
| Merchant | The billing/isolation namespace — scopes subscriptions, payments, credits, catalog, webhooks, analytics. `openrails.merchants`; RLS pins every tenant-scoped query to the `app.merchant_id` GUC. Deliberately controlled by exactly **one** AuthKit group (1:1). |
| Org / permission-group | The AuthKit-side controller of a merchant. The merchant row stores `permission_group_id`; AuthKit decides which users, API keys, and remote applications act for that group. OpenRails carries no auth of its own. |
| Customer (payer / tenant_subject) | The payable subject under a merchant — a UUID; identity is `(merchant, subject)`. "Tenant" survives only in RLS and payer contexts, never as a top-level identity word. |
| `delegated_sub` | External OIDC subject from a registered issuer — the host app's end user, carried in AuthKit delegated access tokens. OpenRails resolves the issuer to its merchant and touches the customer `(merchant_id, delegated_sub)`; tokens never carry merchant claims. |
| Remote application | AuthKit-registered issuer/JWKS principal that signs delegated and service tokens. A credential nested under a permission-group, not an owner. |
| Invoker | The principal that caused usage when it differs from the payable customer (spend-delegation budgets meter per-invoker). |

## Money & access

| Term | Meaning |
|---|---|
| Micros | ALL amounts are micros — millionths of a currency unit (not cents). `moneyutil.Micros`. |
| Money ledger | Double-entry ledger, the source of truth for money. FX inside the ledger is forbidden — no cross-currency transfers. |
| Grant | An immutable event in the append-only grant ledger (`openrails.grants`), kind `entitlement`/`ownership`/`credit`. Revoke/expire/supersede are new events referencing the original; a credit grant IS the FIFO lot. |
| Entitlement | A plain string (e.g. `premium`) a customer holds over time — a timeline of windows in `openrails.entitlements`, materialized from grants. See `docs/entitlements_timeline.md`. |
| Grace | A bounded, revocable generosity window (`source_type='grace'`) appended beyond the paid term; revoked or lapsed the moment truth arrives. |

## Rails & PSPs

| Term | Meaning |
|---|---|
| Rail | A payment gateway **kind** OpenRails codes against — `models.Rail`: `nmi`, `ccbill`, `stripe`, `solana`, `vaulted_card`, `paypal`. One adapter per rail under `internal/integrations/<rail>`. |
| PSP | A merchant's concrete **account on a rail** — credentials + operator-declared `account_id` + manifest key. Row: `openrails.psps` (`psps.key`, e.g. `mobius` on rail `nmi`); manifest: `merchants.<slug>.psps.<key>.<rail>`. Catalog `psp_links` and the checkout wire speak PSP keys. Renamed from `rail_merchant_accounts` (earlier `provider_accounts`) — the retired names fail loudly, no aliases. |
| `account_id` | Operator-declared, opaque PSP label — never derived from credentials at runtime (Stripe is the one self-discovering exception). NMI = the dashboard Gateway ID; Stripe = `acct_…`; CCBill = `clientAccnum-clientSubacc` (dash-joined); Solana = the recipient wallet address. |
| Channel | An off-rail source for **recording** a payment that never flowed through a gateway — `models.Channel`: `admin`, `manual`. No adapter, no credentials, no PSP; stored in the same `payments.rail` column, kept distinct by the two Go enums. |
| Armed | A rail is usable for a merchant iff it has an active PSP row, resolved per-merchant at request time through the one seam `internal/railresolve` (fail closed on `ErrRailNotArmed`). |
| Integration | The Go client speaking a rail's external API: `internal/integrations/{nmi,stripeapi,ccbill,solana}`. All Stripe HTTP goes through `stripeapi`; all NMI HTTP through `nmi`. |
| Provider intent | A durable outbox row (`openrails.rail_intents`) posted before **every** outbound provider mutation, executed effectively-once by a scheduled runner. Outcomes: succeeded, retryable, `unknown_needs_verify` (ambiguity ⇒ verify via provider reads, never blind retry), terminal, parked. |
| Processor (NMI wire) | NMI's own name for its backend acquiring processor (`processor_id`, `processor_response_text`, decline strings). External wire format — a different concept from our rail; never renamed. |

## Operating knobs

| Term | Meaning |
|---|---|
| MODE 1 / MODE 2 (`merchant_source`) | Who owns merchant config. `manifest` (MODE 1): YAML-is-truth in memory, operator = merchant, reboot to change. `api` (MODE 2): no YAML — Vault + DB via API, for true multi-tenant SaaS. Orthogonal to embedded vs standalone. |
| Embedded vs standalone | Only how the process/routes are hosted: a host app mounts OpenRails under `/billing/v1/*`, standalone serves `/v1/*`. Both run either MODE. |
| `provider_write_mode` | How much OpenRails may do against providers: `full` (normal) / `limited` (no system-initiated writes) / `readonly` (no writes). Unset defaults to `readonly` — fail closed; non-dev boot requires an explicit value. |
| `test_mode` | Credential posture: `sandbox` or `live`, two explicit states. Sandbox attaches credential guarantees (live Stripe keys refuse boot, NMI accounts probed, CCBill sandbox URL, Solana devnet). Independent of environment — production can legitimately run sandbox rails. |

## Reconciliation

| Term | Meaning |
|---|---|
| Finding planes | The four diagnostic planes of the truth model: **pull** (provider is authoritative — pull observed truth in), **derive** (grant effect vs source ledger), **life** (record vs clock + state machine), **consistency** (duplicates, amount mismatches, dangling references). Finding types are `<plane>.<subject>.<shape>`, e.g. `pull.charge.missing`. |
| Shapes | `missing` / `excess` / `mismatch` — exhaustive; repaired by materialize / retract / adjust. Unevaluable cases are the INDETERMINATE state, not a shape. |

See [operations.md](operations.md#the-convergence-engine) for the full reconciliation model and
`docs/merchant-guide.md` for catalog vocabulary.
