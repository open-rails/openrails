# Invariants

Hard constraints this codebase must uphold. Each entry states the rule, where it is
enforced, how strongly, and how to audit it mechanically.

An invariant that nothing can mechanically check is a wish. Every rule below carries an
**audit** column for that reason — if you cannot write the query, grep, or test that fails
when the rule breaks, the rule is not yet enforced. Section 10 lists the rules we hold but
do not yet enforce.

**Strength**

| | meaning |
|---|---|
| **S** | structurally impossible — the type system, a role privilege, or a chokepoint makes the violation unrepresentable |
| **DB** | Postgres-enforced — CHECK, trigger, unique index, RLS policy |
| **APP** | enforced at a single application chokepoint or validation function |
| **T** | a repo test fails |
| **C** | convention only — nothing fails |

**Scope.** The convergence / billing-consistency domain (the four diagnostic planes,
finding taxonomy, confirmed-absence gate, replay-vs-converge) is specified separately and
in depth; this register does not restate it. Where a rule lives there, it is cross-referenced
rather than duplicated.

---

## 1. Money representation

All internal amounts are **micros** — millionths of a major currency unit. Not cents, not
millicents. Cents and decimal major units exist only at rail boundaries.

| # | Invariant | Enforced at | Str | Audit |
|---|---|---|---|---|
| MONEY-1 | Internal money is micros (1 major unit = 1 000 000). | `internal/shared/moneyutil/moneyutil.go:9-13` | C + T | `grep -rn "MicrosPerMajorUnit\|MicrosPerCent" --include=*.go` |
| MONEY-2 | Passing micros where cents are expected is a **compile error** — `Micros` and `Cents` are distinct defined types. | `moneyutil.go:18,22` | **S** | `go build ./...` |
| MONEY-3 | **Currency and crypto amounts are integers only.** No float may represent, convert, round, or compare an amount. A *rate* may be a float; arithmetic that produces or compares an *amount* may not. | see §10 GAP-1 | C | `grep -rnE "float(32\|64)\|ParseFloat\|FormatFloat\|math\.(Ceil\|Floor\|Round\|Pow10)" --include=*.go` in money/token packages |
| MONEY-4 | Micros→cents is exact-or-error on the exact path; the ceil path never under-charges. | `moneyutil.go:60-72` | APP | `moneyutil_test.go` |
| MONEY-5 | The single internal→rail converter is `NativeToRailMinor`; it errors on an unregistered currency and rounds **up**. Callers must not guess a scale. | `internal/modules/money/currency.go:66-87` | APP | any ad-hoc `/100` or `*100` at a provider edge is a violation |
| MONEY-6 | Decimal strings parse via exact rational (`big.Rat`), half-away-from-zero, with an int64-overflow error. | `moneyutil.go:33-46,125-154` | APP | `grep -rn "ParseFloat" --include=*.go internal/integrations` |
| MONEY-7 | Every provider money boundary ships a **wire-pinning test**: known micros in ⇒ exact integer on the wire. | `internal/modules/subscriptions/stripe_wire_pinning_test.go`, `internal/modules/webhooks/{stripe,ccbill}_wire_pinning_test.go`, `internal/integrations/nmi/recurring_plan_test.go` | **T** | `grep -rln "wire pinning" --include=*_test.go` |
| MONEY-8 | `ledger_transfers.amount > 0`; `allow_debit_negative_up_to >= 0`. | `0001_schema.up.sql:1672-1673` | **DB** | `SELECT count(*) FROM openrails.ledger_transfers WHERE amount<=0;` → 0 |
| MONEY-9 | `payments.amount` deliberately has **no** non-negative CHECK — refunds are negative rows — and a test forbids adding one. | `migrations/postgres/amount_checks_test.go:45-48` | **T** | `go test ./migrations/postgres -run TestAmountValueChecks` |
| MONEY-10 | Amount CHECKs hold across prices, grants, invoices, invoice items/payments, usage events, minimum spend, credit limits, rating watermarks. | `0001_schema.up.sql:715-720,1303,1524,1574,1743,2880,1238,2037,1996` | **DB** | `SELECT conname FROM pg_constraint WHERE contype='c' AND connamespace='openrails'::regnamespace;` |

## 2. Currency

**Every amount carries its currency.** No amount is ever ambiguous, and no code path may
substitute a default currency because one was not supplied.

| # | Invariant | Enforced at | Str | Audit |
|---|---|---|---|---|
| CUR-1 | Every stored amount carries a currency: 16 currency columns, 15 `NOT NULL`. | `0001_schema.up.sql:476,703,1031,1137,1234,1498,1564,1611,1661,1731,1989,2034,2827,2869`; `0005:6` | **DB** | `SELECT table_name FROM information_schema.columns WHERE column_name='currency' AND table_schema='openrails' AND is_nullable='YES';` → only `grants` |
| CUR-2 | The one nullable currency is conditionally required: `grants.kind='credit' ⇒ amount AND currency NOT NULL`. | `0001_schema.up.sql:1304` | **DB** | `\d openrails.grants` |
| CUR-3 | **FX is forbidden inside the ledger.** A transfer whose account currency differs from the transfer currency raises. | trigger `0001_schema.up.sql:151-153`, wired `:1717` | **DB** | attempt a cross-currency transfer in psql → must raise |
| CUR-4 | FX is not merely forbidden but absent — the `fx_liquidity` account type has no non-declaration call site. | `0001_schema.up.sql:1617`; `money/ledger/ledger.go:39` | **S** (today) | `grep -rn "fx_liquidity" --include=*.go \| grep -v _test` → declarations only |
| CUR-5 | Currency codes come from a Go registry with per-currency internal and rail decimals. There is deliberately no DB CHECK. | `internal/modules/money/currency.go:11-13,26-30` | APP | see §10 GAP-6 |
| CUR-6 | Currency is case-normalized on read. | `currency.go:34-43` | APP | — |
| CUR-7 | Billing surfaces reject a qualified custom-credit unit — external currency only. | `RequireBillingCurrency`, `currency.go:103-108` | APP | `grep -rn "RequireBillingCurrency"` |
| CUR-8 | Service entry points require a non-empty currency. | `pkg/service/currency.go:10-19` | APP | `grep -rn "requireCurrency(" pkg/service` |
| CUR-9 | **A missing provider currency is never fabricated.** A decline carries no currency of its own and must not borrow one. | `internal/reconcile/unknown_orchestration.go:221` | APP | see §10 GAP-4 — two paths violate this today |

## 3. Tenant isolation

One merchant per controlling org. Isolation is enforced twice: Postgres RLS, and the rule
that a merchant id is derived from the authenticated principal, never from request data.

| # | Invariant | Enforced at | Str | Audit |
|---|---|---|---|---|
| TEN-1 | Every merchant-owned table has `ENABLE` **and** `FORCE ROW LEVEL SECURITY` plus a `merchant_isolation` policy with both `USING` and `WITH CHECK`. | `0001_schema.up.sql` per table, e.g. `:1018-1020,1357-1359,1719-1721` | **DB** | `SELECT relname FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='openrails' AND c.relkind='r' AND NOT c.relrowsecurity;` |
| TEN-2 | Unset GUC ⇒ policy is NULL ⇒ **zero rows**. RLS fails closed. | policy text; `db_pgx.go:18-28` | **DB** | query as `openrails_app` without the GUC → 0 rows |
| TEN-3 | Exactly three tables are RLS-exempt by design, each documented in-schema: `merchants`, `probe_verdicts`, `worker_health`. | `0001_schema.up.sql:258,290,309` | **DB** + comment | the TEN-1 query returns exactly those three |
| TEN-4 | `merchant_id` is `uuid NOT NULL` everywhere and must never be defaulted, back-filled, or derived from the GUC. | `merchant_aware_schema_test.go:100-120` | **T** | `go test ./migrations/postgres` |
| TEN-5 | The merchant id comes from resolved context; `merchant.Require` errors rather than defaulting. There is no default merchant and the schema seeds none. | `pkg/merchant/merchant.go:50-54,103-112`; `merchant_aware_schema_test.go:86-98` | APP + T | `grep -rn 'json:"merchant_id' --include=*.go internal pkg \| grep -v /gen/` → no API request struct |
| TEN-6 | `MerchantTx` pins the GUC **transaction-locally** so it cannot leak onto a pooled connection; a zero merchant id is rejected. | `internal/db/db_pgx.go:144-177` | APP | `grep -rn "set_config" internal/db` |
| TEN-7 | On release-reset failure the request connection is **closed**, not returned to the pool. | `db_pgx.go:225-243` | APP | — |
| TEN-8 | Boot refuses, outside development, if the Postgres role bypasses RLS. | `internal/db/rls.go:59-94`; `build_runtime.go:176` | APP | boot `env=production` as superuser → must fail |
| TEN-9 | The app role is `NOLOGIN NOBYPASSRLS`. | `0001_schema.up.sql:24-27` | **DB** | `SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname='openrails_app';` |
| TEN-10 | Cross-merchant reads go through **one named accessor**, `DB.GenGlobal()`. Any other pool read of an RLS'd table is a bug — it returns empty, not an error. | `db_pgx.go:66-80` | APP | `grep -rn "GenGlobal()" --include=*.go` — every hit needs justification |
| TEN-11 | Unauthenticated webhook surfaces resolve the merchant from route slug / Host / declared PSP catalog, then verify the signature with *that merchant's* secret. An unresolvable Host is a hard 404. | `internal/http/handlers/webhook.go:78-90,99-116,126-145,272-322` | APP | — |
| TEN-12 | Core schema may not FK into AuthKit's schema and may not create River tables. | `portability_guard_test.go:27-56` | **T** | `go test ./migrations/postgres -run TestPortabilityInvariant` |

## 4. Ledger integrity

| # | Invariant | Enforced at | Str | Audit |
|---|---|---|---|---|
| LED-1 | A transfer moves an amount within one `(merchant, currency)` ledger; both accounts lock `FOR UPDATE ORDER BY id` before counters move. | trigger `0001_schema.up.sql:132-145` | **DB** | — |
| LED-2 | A missing debit or credit account raises — never a silent no-op. | `:147-149` | **DB** | — |
| LED-3 | **Insufficient-funds floor**: debiting below `-allow_debit_negative_up_to` raises `ledger_insufficient_funds`. | trigger `:155-159`; flag set in `ledger.go:69-72` | **DB** | `SELECT … WHERE account_type='customer_balance' AND NOT debits_must_not_exceed_credits;` → 0 |
| LED-4 | Symmetric ceiling for `credits_must_not_exceed_debits` accounts. | `:160-162` | **DB** | — |
| LED-5 | Transfers and accounts are **append-only**: the app role holds `SELECT,INSERT` only; counters move solely through the `SECURITY DEFINER` trigger. | `:1652,:1723`; trigger `:122-123` | **S** | `SELECT privilege_type FROM information_schema.table_privileges WHERE grantee='openrails_app' AND table_name IN ('ledger_transfers','ledger_accounts');` |
| LED-6 | Debit ≠ credit account. | `:1674` | **DB** | — |
| LED-7 | A credit lot's deposit / expire / revoke each happen at most once. | `idx_ledger_transfers_lot_once`, `:1707` | **DB** | — |
| LED-8 | An owed accrual is once per `(merchant, customer, currency, source, source_id)`. | `:1713` | **DB** | — |
| LED-9 | Ledger purity: transfer-side `grant_id`/`invoice_id`/`customer_id` carry **no FKs**, so the append-only ledger never blocks or cascades on control-plane rows. | `:1690-1691` | **DB** (by omission) | — |
| LED-10 | Balance reads never create an account. | `ledger.go:74-90` | APP | — |

## 5. Idempotency and provider writes

All outbound provider mutations post a durable intent first, then execute.

| # | Invariant | Enforced at | Str | Audit |
|---|---|---|---|---|
| IDEM-1 | Every provider mutation posts a `rail_intents` row before execution. | `internal/intents/intents.go:1-21` | APP + T | IDEM-7 |
| IDEM-2 | One intent per logical mutation per merchant: `UNIQUE (merchant_id, idempotency_key)`. | `0001_schema.up.sql:2408` | **DB** | — |
| IDEM-3 | **Idempotency keys are content-addressed and stable across retries.** No key may derive from wall clock, randomness, or a mutable field. | key constructors in `internal/intents/*.go` | APP | `grep -rn "IdempotencyKey" --include=*.go internal pkg \| grep -v _test \| grep -iE "time\.Now\|uuid\.New\|rand\."` → **must stay empty** |
| IDEM-4 | The one time-shaped key input derives from a durable stamp (`last_topup_at`), never the clock. | `internal/modules/money/money_in.go:138-141` | APP | — |
| IDEM-5 | **Ambiguity verifies, never declines.** An ambiguous outcome parks as `unknown_needs_verify` and is resolved by provider **reads** only. Money movers never blind-retry. | `intents.go:62-66,140-143` | APP | — |
| IDEM-6 | Stripe refunds are double-walled: the intent key is also the Stripe `Idempotency-Key` header. | `intents/refund.go:79-85`; `stripeapi.go:101-111` | APP | — |
| IDEM-7 | New provider-write call sites **fail CI** — an allow-list names every file permitted to invoke a money mover, recurring mutation, vault delete, or Solana submit. | `internal/intents/enforcement_guard_test.go:9-45` | **T** (weak, textual — see §10 GAP-11) | `go test ./internal/intents -run TestProviderWritesStayBehindIntents` |
| IDEM-8 | All Stripe HTTP goes through one transport that blocks writes under `readonly` before bytes reach the network, and pins `Stripe-Version` from one const. | `stripeapi.go:23-28,56-67,85` | **S** | `grep -rn "stripe.com" --include=*.go \| grep -v stripeapi` |
| IDEM-9 | The operating-mode gate is a total function — an unknown origin parks rather than guessing. | `internal/intents/gate.go:34-50` | APP | — |
| IDEM-10 | Mode/kill-switch blocking is recorded as a reason on the intent, never raised as an error, so the queue drains when the blocker lifts. | `intents.go:16-20`; CHECK `:2360` | APP + DB | — |
| IDEM-11 | Webhook dedup identity is `PRIMARY KEY (merchant_id, op, event_id)`, falling back to a body hash when the rail supplies no id. Dedup happens **before** effect. | `0001_schema.up.sql:2938`; `webhookutil.go:170-178` | **DB** | — |

## 6. Identity and uniqueness

| # | Invariant | Enforced at | Str |
|---|---|---|---|
| ID-1 | **Entitlement windows for one `(merchant, customer, entitlement)` may not overlap** — GiST EXCLUDE over a generated `tstzrange`, filtered to live rows. | `0001_schema.up.sql:1375,1387` | **DB** |
| ID-2 | At most one open-ended live entitlement per `(merchant, customer, entitlement)`. | `:1423` | **DB** |
| ID-3 | One live subscription per `(merchant, customer, product)`, and one per `(customer, tier_group)`, with `tier_group` denormalized by a BEFORE trigger. | `:1006,:1008`; trigger `:175-184` | **DB** |
| ID-4 | One customer per `(merchant, subject)`; one rail-customer per `(merchant, customer, rail)`. | `:691,:2324,:2326` | **DB** |
| ID-5 | Price financial substance is unique per product. | `:744` | **DB** |
| ID-6 | At most one non-archived price per `(merchant, key)`; the key is trigger-derived and raises if the product is missing. | `:762`; fn `:203-233` | **DB** |
| ID-7 | Natural-key identity is a deterministic uuidv5 over a length-prefixed injective encoding. The namespace must never change. | `internal/shared/uuidutil/uuid.go:18-46` | APP |
| ID-8 | Grant termination happens once; `event='grant' ⟺ supersedes_id IS NULL`. | `:1355,:1306` | **DB** |
| ID-9 | Invoice period, invoice-item source, usage-event, and finding identities are unique per merchant. | `:1552,:1598,:2915,:2617` | **DB** |
| ID-10 | Merchant slug unique; `api_host` unique among live merchants. | `:272,:276` | **DB** |

## 7. Fail-closed posture

These guards must fail closed. A guard that cannot fail is worse than none, because it
reads as protection.

| # | Guard | Enforced at |
|---|---|---|
| FC-1 | RLS on unset GUC → zero rows. | policy text |
| FC-2 | Boot refuses a BYPASSRLS role outside dev. | `internal/db/rls.go:73-94` |
| FC-3 | Boot refuses the DB secret store without `ENCRYPTION_MASTER_KEY` outside dev. | `merchantsecrets/store.go:297-308` |
| FC-4 | Even in dev, Solana private keys may not be stored unencrypted. | `store.go:227-233` |
| FC-5 | `secret_backend=vault` without a working Vault, or without KV read capability, refuses. | `store.go:191-197`; `config.go:1284-1291` |
| FC-6 | `merchant_source=api` refuses env overlays and mounted manifest secrets (two truths); an unknown value refuses to load. | `config.go:1243-1271` |
| FC-7 | Webhook signature: a missing secret or missing header is an error. There is **no** unsigned path. HMAC compares with `hmac.Equal`. | `webhookutil.go:98-110`; `sigverify.go:44-71` |
| FC-8 | Replay tolerance is 5 minutes on public ingestion; the queued re-verify path narrows to HMAC-only, deliberately and in one leaf package. | `webhookutil.go:205-224` |
| FC-9 | A webhook whose merchant cannot be resolved is a 404, never a guess. | `webhook.go:78-90` |
| FC-10 | An unknown intent origin parks. | `intents/gate.go:46-49` |
| FC-11 | A Stripe write under `readonly` errors at the transport. | `stripeapi.go:56-60` |
| FC-12 | Missing merchant on context errors; there is no default merchant. | `pkg/merchant/merchant.go:50-54` |
| FC-13 | Captcha verifier unavailable → treated as invalid, not as pass. | `ratelimit_neutral.go:281-289` |
| FC-14 | Spendgate Redis errors propagate — no default-allow. | `spendgate/gate.go:247-250,269-271` |
| FC-15 | A delegated invoker with no explicit budget grant may never spend the payer's money. | `admission/admitter.go:37-40` |

**Deliberately open — access preservation.** These fail *open* on purpose: our malfunction
must never cost a customer access. Do not "fix" them without changing the doctrine.

| # | Guard | Enforced at |
|---|---|---|
| FO-1 | Rate limiting degrades from Redis to an in-memory limiter (degraded, not unlimited). | `ratelimit_neutral.go:209-218` |
| FO-2 | Entitlement and access checks fail open: `unknown`-status subscriptions keep standing access; a broken justification chain must be *proven* before retraction. | `reconcile/store.go:477`; `converge/converge_passes.go:304`; `grants/grants.go:625` |

## 8. No silent fabrication

Record provider data verbatim; derive downstream deterministically. A missing required
value must error or warn — never be replaced by an invented one. Declared **config**
defaults are fine; invented **data** defaults are not.

| # | Invariant | Enforced at |
|---|---|---|
| FAB-1 | Rail decline codes are stored verbatim; the normalized category is derived; an unmapped code reads `"unknown"`. | `0001_schema.up.sql:1094-1096`; `payments/failure_reason.go:10` |
| FAB-2 | A product declaring no entitlements grants **none**. (A previous version fabricated `"premium"`.) | `subscriptions/lifecycle_service.go:411-418,1094-1099` |
| FAB-3 | Absent provider dates and tokens return `ok=false`, never a fabricated instant. | `ccbill/subscription_management.go:140,362`; `reconcile/unknown_probe.go:100,201` |
| FAB-4 | A Solana subscribe/cancel is not classified as a sale — that would fabricate payment. | `reconcile/solana.go:27` |
| FAB-5 | Missing PSP posture surfaces as posture, not as a fabricated empty. | `reconcile/merchant_wiring.go:183` |
| FAB-6 | A parse failure must not silently become a zero amount. | see §10 GAP-2 |

## 9. Destructive and irreversible actions

Provider-side deletes and cancels cannot be undone by us. An NMI vault delete destroys the
stored card; the customer must re-enter it.

| # | Invariant | Status |
|---|---|---|
| DES-1 | **Cancellation is a last resort.** Terminal cancel and provider-side delete require *certainty* — a non-retryable decline, or genuinely exhausted dunning. Never a date comparison, never an absence. | **VIOLATED** — see §10 GAP-3 |
| DES-2 | Stale or unreadable provider data parks as `unknown` and never costs entitlements. | FO-2 upholds the entitlement half |
| DES-3 | **NMI rebill is infinite retry.** Missed periods are forgiven; the customer is paid up from the latest success. A lapsed expiration is not a dead schedule — only provider roster state classifies. | **VIOLATED** — see §10 GAP-3 |
| DES-4 | Absence is not evidence of death. A subscription missing from a pull, an empty response, a 404, or a timeout must not retract standing. The confirmed-absence gate must hold at **every** retraction site. | audit in progress |
| DES-5 | No unattended irreversible provider call without provider-confirmed certainty, a blast-radius cap, and an operator kill switch that works without a deploy. | audit in progress |

## 10. Known gaps — rules we hold but do not enforce

Every one of these is a rule the codebase intends. None currently fails when broken.
Tracker issues in `~/open-rails-tracker/openrails/`.

| # | Gap | Evidence | Tracked |
|---|---|---|---|
| GAP-1 | **Integers-only for money is convention only.** `MajorUnits` is a `float64` money type reaching the live NMI wire; Solana base units round-trip through float and overcharge 1.19% of whole-cent amounts; FX multiplies an amount by a float; a refund amount arrives as a JSON float. | `moneyutil.go:27`; `nmi/subscriptions.go:113`; `solana/support.go:63,122`; `fx/provider.go:79`; `admin_findings_actions.go:355-361` | #818 |
| GAP-2 | **Currency is silently defaulted in two paths** — both substitute a lowercase, unregistered `"usd"` when the provider gave none, one while minting a `payments` row. Violates CUR-9 and FAB-6. | `solana/support.go:116-119`; `modules/reconcile/reconcile.go:570-573` | #830 |
| GAP-3 | **Terminal cancel + NMI vault delete fire on a stale date.** Past-due is inferred from `next_billing_date < today` with no decline evidence; beyond 14 days that becomes an irreversible provider delete. The decider's own comment admits the remote may still be retrying. | `reconcile/nmi.go:181-186`; `decider.go:315-319`; `lifecycle_service.go:1533-1540` | #821 |
| GAP-4 | **RLS coverage is not checked beyond migration 0001.** `payment_settlement_events` (0005) carries `merchant_id` and full CRUD grants with no RLS. The guard test reads only `0001` from a hardcoded list. | `0005_payment_settlements.up.sql:1-19`; `merchant_aware_schema_test.go:14-77` | SEC-16 |
| GAP-5 | **Duplicate payments and subscriptions are representable.** The uniques are partial and disjoint on `rail_merchant_account_id IS NULL`, so the same `(merchant, rail, transaction_id)` can exist twice — once legacy, once account-attributed. | `0001_schema.up.sql:1119` vs `:1121`; `:1010` vs `:1012` | #831 |
| GAP-6 | **Currency codes in the DB are unvalidated.** Deliberately app-only across 16 columns; the ledger trigger checks equality, not validity. Nothing stops an import or GAP-2 persisting an unregistered code. | `money/currency.go:11-13` | #832 |
| GAP-7 | **`transfer_type` has no CHECK.** `account_type` does. A typo in a new transfer type silently bypasses the lot-once uniqueness index. | `0001_schema.up.sql:1663` vs `:1617` | #832 |
| GAP-8 | **Ledger conservation is never computed.** `sum(balances) = 0` per `(merchant, currency)` is structural, but nothing checks it, and the account counters are a maintained projection that can drift from `ledger_transfers` if the trigger is ever bypassed (superuser, restore, `COPY`). | trigger `:164-169`; no diagnostic found | #833 |
| GAP-9 | **org ↔ merchant 1:1 is not enforced.** The decision was a unique `owner_org_id`; the shipped column is `permission_group_id` with a **non-unique** index, so two merchants can claim one group and merchant-from-group resolution is ambiguous. | `0001_schema.up.sql:274` | #843 |
| GAP-10 | **Two unique indexes lack `merchant_id`** — `checkout_sessions` on `(rail, reference)` and `(rail, transaction_id)`. The only cross-merchant uniques in the schema; one merchant can block another's insert. | `0001_schema.up.sql:1181,1183` | SEC-24 |
| GAP-11 | **The provider-write guard is textual.** A wrapper, method value, or renamed import reaches the same wire call invisibly, and an allow-listed file is trusted wholesale. Record its strength honestly as weak-T, not S. | `enforcement_guard_test.go:15-24` | — |
| GAP-12 | **`Micros` is opt-in.** DB columns are bare `bigint` and most Go structs use `int64`; the unit is carried by column comments. The type only bites where someone chose it. | `moneyutil.go:18` vs `ledger/ledger.go:118` | #818 |

### Audit bundle

Read-only checks that should pass at any time:

```sql
-- TEN-1/TEN-3: tables without RLS (expect exactly merchants, probe_verdicts, worker_health)
SELECT relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE n.nspname='openrails' AND c.relkind='r' AND NOT c.relrowsecurity;

-- GAP-10: unique indexes not scoped by merchant
SELECT indexdef FROM pg_indexes WHERE schemaname='openrails'
   AND indexdef LIKE '%UNIQUE%' AND indexdef NOT LIKE '%merchant_id%';

-- GAP-8: ledger conservation per (merchant, currency)
SELECT merchant_id, currency, sum(credits_posted - debits_posted) FROM openrails.ledger_accounts
 GROUP BY 1,2 HAVING sum(credits_posted - debits_posted) <> 0;

-- GAP-2/GAP-6: unregistered or lowercase currency codes
SELECT DISTINCT currency FROM openrails.payments;

-- GAP-5: duplicate provider transactions
SELECT merchant_id, rail, transaction_id, count(*) FROM openrails.payments
 GROUP BY 1,2,3 HAVING count(*) > 1;

-- GAP-9: merchants sharing a permission group
SELECT permission_group_id, count(*) FROM openrails.merchants
 WHERE permission_group_id IS NOT NULL GROUP BY 1 HAVING count(*) > 1;
```

Test gates:

```
go test ./migrations/postgres                                         # TEN-4, TEN-12, MONEY-9
go test ./internal/intents  -run TestProviderWritesStayBehindIntents  # IDEM-7
go test ./internal/merchants -run TestNoAdHocSecretPathConstruction   # secret-path builder
go test .                   -run 'TestRootPackageStaysLight|TestModuleIsGinFree'
```

---

## Adding an invariant

State it imperatively, then answer three questions before merging: where is it enforced,
how strongly, and what fails when it breaks. If the answer to the third is "nothing", it
belongs in §10 with a tracker issue — not in §1–§9.

Prefer enforcement in this order: make it unrepresentable (a type, a role privilege, a
chokepoint), then a DB constraint, then a test. Comments and review are not enforcement.
