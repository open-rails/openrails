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

**How this register is verified.** Every DB-facing check runs as the unprivileged
`openrails_app` role with RLS enforcing — `internal/invariantaudit` (build tag `integration`)
asserts `NOT rolsuper AND NOT rolbypassrls` before it asserts anything else. This is not a
detail: a GUC-less read of a policied table returns **zero rows and no error**, so a check run
as superuser passes for a guard that can never fire in production. Three of the six SQL checks
in this file's own audit bundle were exactly that shape (§Audit bundle).

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
| MONEY-1 | Internal money is units at the CURRENCY's registered scale — micros (10⁶) for USD/EUR, 10⁴ for JPY. Not "micros everywhere": the registry decides. | `internal/shared/moneyutil/currency.go` (registry); `moneyutil.go:9-13` (the 10⁶ constants) | C + T | `go test ./internal/shared/moneyutil -run TestRegistryNativeShiftIsUniform` |
| MONEY-2 | `Micros` and `Cents` are distinct defined types, and **the unit converters take and return them**, so handing cents to a micros parameter is a compile error at the one place a value changes unit. The currency-BLIND narrowing converters are gone: `NativeToRailMinor` / `NativeToRailMinorExact` / `RailMinorToNative` are the only exported internal↔rail conversions and each requires a registered currency. | `moneyutil.go:18,22`; `currency.go` converters | **S** at the converters, **C** elsewhere | `go build ./...`; `moneyutil_test.go` + `currency_test.go` pin the typed signatures so a silent revert to `int64` fails. Coverage limits: §10 GAP-12 |
| MONEY-3 | **Currency and crypto amounts are integers only.** No float may represent, convert, round, or compare an amount. A *rate* may be a float; arithmetic that produces or compares an *amount* may not. | `internal/shared/moneyutil/float_guard_test.go` (AST, allow-list keyed by declaration) | **T** over the listed packages, **C** elsewhere | `go test ./internal/shared/moneyutil -run TestNoFloatsInMoneyPackages` — see §10 GAP-13 for the packages it does not cover |
| MONEY-4 | Micros→cents is exact-or-error on the exact path; the ceil path never under-charges. | `moneyutil.go:60-72` | APP | `moneyutil_test.go` |
| MONEY-5 | The single internal→rail converter is `moneyutil.NativeToRailMinor` (ceil) / `NativeToRailMinorExact` (errors on a sub-minor remainder); both error on an unregistered currency. Callers cannot guess a scale — there is no currency-blind converter left to call. | `internal/shared/moneyutil/currency.go` | **S** — the alternatives are deleted, not deprecated | `grep -rn "MicrosToCents" --include=*.go` → no hits; `go test ./internal/shared/moneyutil` |
| MONEY-6 | Decimal strings parse via exact rational (`big.Rat`), half-away-from-zero, with an int64-overflow error. | `moneyutil.go:28-41,112-141` | APP | rounding is pinned by `internal/modules/webhooks/nmi_test.go:156-183`; the overflow branch has no test. The register's old grep tested none of the three properties |
| MONEY-7 | Every provider money boundary ships a **wire-pinning test**: known micros in ⇒ exact integer on the wire. | `internal/modules/subscriptions/stripe_wire_pinning_test.go`, `internal/modules/webhooks/{stripe,ccbill}_wire_pinning_test.go`, `internal/integrations/nmi/{recurring_plan,payments_wire}_test.go` | **weak-T** | a hand-kept list. `grep -rln "wire pinning"` has no expected value, so a new unpinned boundary changes nothing. Solana Pay's live formatter is unpinned — §10 GAP-15 |
| MONEY-8 | `ledger_transfers.amount > 0`; `allow_debit_negative_up_to >= 0`. | `0001_schema.up.sql:1672-1673` | **DB** | `SELECT count(*) FROM openrails.ledger_transfers WHERE amount<=0;` → 0 |
| MONEY-9 | `payments.amount` deliberately has **no** non-negative CHECK — refunds are negative rows — and a test forbids adding one. | `migrations/postgres/amount_checks_test.go:45-48` | **weak-T** | `go test ./migrations/postgres -run TestAmountValueChecks` — matches two literal constraint names inside `0001_*.sql` only, so the same CHECK added in a later migration or under Postgres' default name passes |
| MONEY-10 | Amount CHECKs hold across prices, grants, invoices, invoice items/payments, usage events, minimum spend, credit limits, rating watermarks. | `0001_schema.up.sql:715-720,1303,1524,1574,1743,2880,1238,2037,1996` | **DB** | `SELECT conname FROM pg_constraint WHERE contype='c' AND connamespace='openrails'::regnamespace;` |

## 2. Currency

**Every amount carries its currency.** No amount is ever ambiguous, and no code path may
substitute a default currency because one was not supplied.

| # | Invariant | Enforced at | Str | Audit |
|---|---|---|---|---|
| CUR-1 | Every stored amount carries a currency: 16 currency columns, 15 `NOT NULL`. | `0001_schema.up.sql:476,703,1031,1137,1234,1498,1564,1611,1661,1731,1989,2034,2827,2869`; `0005:6` | **DB** | `SELECT table_name FROM information_schema.columns WHERE column_name='currency' AND table_schema='openrails' AND is_nullable='YES';` → only `grants` |
| CUR-2 | The one nullable currency is conditionally required: `grants.kind='credit' ⇒ amount AND currency NOT NULL`. | `0001_schema.up.sql:1304` | **DB** | `\d openrails.grants` |
| CUR-3 | **FX is forbidden inside the ledger.** A transfer whose account currency differs from the transfer currency raises. | trigger `0001_schema.up.sql:151-153`, wired `:1717` | **DB** | attempt a cross-currency transfer in psql → must raise |
| CUR-4 | FX is not merely forbidden but absent — the `fx_liquidity` account type has no non-declaration call site. | `0001_schema.up.sql:1617`; `money/ledger/ledger.go:39` | **C** | `grep -rn "FXLiquidity" --include=*.go \| grep -v _test` → the declaration only, zero call sites. True by habit, not structure: `ledger.FXLiquidity` is an ordinary exported const and passing it to `ensureAccount` compiles |
| CUR-5 | Currency **membership** comes from a Go registry with per-currency internal and rail decimals; the DB does not encode membership (it would need a migration per currency). | `internal/modules/money/currency.go:11-13,26-30` | APP | `money.ValidateCurrency` |
| CUR-5b | Currency **shape and case** are Postgres-enforced on all 16 currency columns: upper-case `[A-Z0-9]{3,12}`, or a qualified `slug/name` custom-credit unit. | `0020_currency_case_and_session_merchant_scope.up.sql`; constraint `<table>_currency_shape` | **DB** + **T** | `SELECT count(*) FROM pg_constraint WHERE conname LIKE '%_currency_shape';` → 16; `go test ./migrations/postgres -run TestCurrencyColumnsCarryShapeCheck` |
| CUR-6 | **UPPER case is the canonical internal form.** Established at the two INSERT chokepoints (`paymentInsertParams` behind both payment inserts; `PriceService.Create` behind every price insert) and at each provider INGESTION boundary, with the CUR-5b CHECK as the backstop that catches anything reaching the DB by another route. Lowercase survives ONLY where a rail wire demands it, at three sites that say so: Stripe's catalog and invoice APIs, and the FX endpoint. | `moneyutil.NormalizeCurrency` (ONE definition, in the leaf so the repo chokepoints can reach it); `payments/payment_repo.go`, `catalog/price.go`; ingestion `webhooks/ccbill.go` `requireCCBillCurrency`, `reconcile/unknown_orchestration.go`; wire exceptions `catalog/stripe_catalog.go`, `subscriptions/stripe_invoice_collection.go`, `fx/exchange_api.go` `fetchRate` | APP (chokepoint) + **DB** | `SELECT DISTINCT currency FROM openrails.payments;` → all upper |
| CUR-7 | Billing surfaces reject a qualified custom-credit unit — external currency only. | `RequireBillingCurrency`, `currency.go:103-108` | APP | `grep -rn "RequireBillingCurrency"` |
| CUR-8 | Service entry points require a REGISTERED currency — `"XYZ"` no longer passes; qualified custom-credit units (#475) are the deliberate exemption. It is still a helper an entry point can forget to call, so total enforcement lives where it cannot be bypassed: every off-session charge is registry-validated at `ScopedCharger.ChargeSavedMethod`, and every internal→rail conversion refuses an unregistered currency by construction. | `pkg/service/currency.go`; `money/collection.go`; `moneyutil.NativeToRailMinor*` | APP at the entry point, **S** at the charge/convert boundary | `go test ./pkg/service -run TestRequireCurrencyConsultsTheRegistry`; `go test ./internal/modules/money -run RefusesUnestablishedCurrency` |
| CUR-9 | **A missing provider currency is never fabricated, defaulted, or silently borrowed.** A decline carries no currency of its own; where the subscription's billing currency is genuinely the one the attempt was denominated in, the backfill may INHERIT it — and the row records `currency_provenance` so an inference is never mistaken for an observation. No path substitutes a default before a charge. | `internal/reconcile/unknown_orchestration.go` (inherit + provenance); `money/{collection,nmi_collection,vaultedcard_collection,stripe_collection}.go` (refuse); `handlers/admin_users.go` (no query-param default) | APP + **T** | `go test ./internal/modules/money -run RefusesUnestablishedCurrency` — §10 GAP-16 |

## 3. Tenant isolation

One merchant per controlling org. Isolation is enforced twice: Postgres RLS, and the rule
that a merchant id is derived from the authenticated principal, never from request data.

| # | Invariant | Enforced at | Str | Audit |
|---|---|---|---|---|
| TEN-1 | Every merchant-owned table has `ENABLE` **and** `FORCE ROW LEVEL SECURITY` plus a `merchant_isolation` policy with both `USING` and `WITH CHECK`. | `0001_schema.up.sql` per table, e.g. `:1018-1020,1357-1359,1719-1721` | **DB** + **T** | `go test -tags integration ./internal/invariantaudit -run TestTEN1` — verified 2026-07-28 as `openrails_app`: zero tables ENABLEd without FORCE, zero policies missing a clause, zero `merchant_id`-bearing tables without RLS |
| TEN-2 | Unset GUC ⇒ policy is NULL ⇒ **zero rows, and no error**. RLS fails closed — and fails *silently*, which is why §10 GAP-17's class exists. | policy text; `db_pgx.go:18-28` | **DB** + **T** | `go test -tags integration ./internal/invariantaudit -run TestTEN2` — asserts the emptiness, the absence of an error, and that a GUC-bearing tx sees the row |
| TEN-3 | Exactly **four** tables are RLS-exempt by design, each documented in-schema: `merchants`, `probe_verdicts`, `worker_health`, `destructive_action_switch` (or#836 — the kill switch must be readable before any merchant is resolved). | `0001_schema.up.sql:258,290,309`; `0015_destructive_action_policy.up.sql:27-47` | **DB** + **T** | `TestTEN1_AllTablesUnderRLSExceptDocumentedExemptions` pins the set in both directions. The register said "three" until 2026-07-28; the fourth had been shipped and undocumented here |
| TEN-4 | `merchant_id` is `uuid NOT NULL` everywhere and must never be defaulted, back-filled, or derived from the GUC. | `merchant_aware_schema_test.go:100-120` | **T** | `go test ./migrations/postgres` |
| TEN-5 | The merchant id comes from resolved context; `merchant.Require` errors rather than defaulting. There is no default merchant and the schema seeds none. | `pkg/merchant/merchant.go:50-54,103-112`; `merchant_aware_schema_test.go:86-98` | APP + T | `grep -rn 'json:"merchant_id' --include=*.go internal pkg \| grep -v /gen/` → no API request struct |
| TEN-6 | `MerchantTx` pins the GUC **transaction-locally** so it cannot leak onto a pooled connection; a zero merchant id is rejected. | `internal/db/db_pgx.go:144-177` | APP | `grep -rn "set_config" internal/db` |
| TEN-7 | On release-reset failure the request connection is **closed**, not returned to the pool. | `db_pgx.go:225-243` | APP | — |
| TEN-8 | Boot refuses, outside development, if the Postgres role bypasses RLS. | `internal/db/rls.go:59-94`; `build_runtime.go:176` | APP | boot `env=production` as superuser → must fail |
| TEN-9 | The app role is `NOLOGIN NOBYPASSRLS`. | `0001_schema.up.sql:24-27` | **DB** | `SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname='openrails_app';` |
| TEN-10 | **There is no privileged pool.** One pool, one role — dropping the GUC does not bypass a policy, it fails it. A genuine cross-merchant read goes through a `SECURITY DEFINER` reader (migrations 0016, 0021) that ASSERTS its definer bypasses RLS and RAISES if not; everything else runs per-merchant under `MerchantTx`/`RunInMerchantConn`. `DB.GenGlobal()` names the not-merchant-pinned connection, **not** a privilege. | `db_pgx.go:66-86`; `0016`/`0021` | APP | `grep -rn "GenGlobal()\|gen.New(.*\.pool)" --include=*.go` — every hit must resolve to a definer reader or an exempt table. §10 GAP-17 lists the ones that still do not |
| TEN-11 | Unauthenticated webhook surfaces resolve the merchant from route slug / Host / declared PSP catalog, then verify the signature with *that merchant's* secret. An unresolvable Host is a hard 404. | `internal/http/handlers/webhook.go:78-90,99-116,126-145,272-322` | APP | — |
| TEN-12 | Core schema may not FK into AuthKit's schema and may not create River tables. | `portability_guard_test.go:27-56` | **T** | `go test ./migrations/postgres -run TestPortabilityInvariant` |

## 4. Ledger integrity

| # | Invariant | Enforced at | Str | Audit |
|---|---|---|---|---|
| LED-1 | A transfer moves an amount within one `(merchant, currency)` ledger; both accounts lock `FOR UPDATE ORDER BY id` before counters move. | trigger `0001_schema.up.sql:132-145` | **DB** | — |
| LED-2 | A missing debit or credit account raises — never a silent no-op. | `:147-149` | **DB** | — |
| LED-3 | **Insufficient-funds floor**: debiting below `-allow_debit_negative_up_to` raises `ledger_insufficient_funds`. | trigger `:155-159`; flag set in `ledger.go:69-72` | **DB** | `SELECT … WHERE account_type='customer_balance' AND NOT debits_must_not_exceed_credits;` → 0 |
| LED-4 | Symmetric ceiling for `credits_must_not_exceed_debits` accounts. | `:160-162` | **DB** | — |
| LED-5 | Transfers and accounts are **append-only**: the app role holds `SELECT,INSERT` only; counters move solely through the `SECURITY DEFINER` trigger. | `:1652,:1723`; trigger `:122-123` | **S** + **T** | `TestLED5_LedgerIsAppendOnlyByPrivilege`. Verified live 2026-07-28: as `openrails_app`, UPDATE/DELETE on either table is `permission denied` |
| LED-6 | Debit ≠ credit account. | `:1674` | **DB** (via the trigger, not the CHECK) | the BEFORE trigger runs first and rejects the self-transfer as "account not found", so `ledger_transfers_distinct_accounts` never fires. Still rejected — just not by the constraint the row cites | — |
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
| IDEM-3 | **Idempotency keys are content-addressed and stable across retries.** No key may derive from wall clock or randomness. A *monotonic attempt counter* is permitted and is the mechanism by which a deliberate re-attempt maps onto a new intent (`ManualRebillIdempotencyKey`'s `attempt`, `NMIPaymentSourceUpdateIdempotencyKey`'s `priorSwaps`). | key constructors in `internal/intents/*.go` | APP | `grep -rn "IdempotencyKey" --include=*.go internal pkg \| grep -v _test \| grep -iE "time\.Now\|uuid\.New\|rand\."` → empty (verified 2026-07-28). All 12 constructors also traced by hand for indirect clock/random input; clean. Nothing runs this grep in CI |
| IDEM-4 | The one time-shaped key input derives from a durable stamp (`last_topup_at`), never the clock. | `internal/modules/money/money_in.go:138-141` | APP | — |
| IDEM-5 | **Ambiguity verifies, never declines.** An ambiguous outcome parks as `unknown_needs_verify` and is resolved by provider **reads** only. Money movers never blind-retry. | `intents.go:62-66,140-143` | APP | — |
| IDEM-6 | Stripe refunds are double-walled: the intent key is also the Stripe `Idempotency-Key` header. | `intents/refund.go:79-85`; `stripeapi.go:101-111` | APP | — |
| IDEM-7 | New provider-write call sites **fail CI** — an allow-list names every file permitted to invoke a money mover, recurring mutation, vault delete, or Solana submit. | `internal/intents/enforcement_guard_test.go:9-45` | **weak-T** (textual — see §10 GAP-11) | `go test ./internal/intents -run TestProviderWritesStayBehindIntents`. Verified 2026-07-28: no live bypass exists. But the walk skips `internal/integrations/**`, `cmd/`, `tests/`, `embed/`, `config/` and the repo root, and the `.Void(` entry is **dead** — its only caller in the tree is `nmi/probe.go:142`, inside the skipped subtree |
| IDEM-8 | All Stripe HTTP goes through one transport that blocks writes under `readonly` before bytes reach the network, and pins `Stripe-Version` from one const. | `stripeapi.go:23-28,56-67,85` | **APP** | verified by hand 2026-07-28: all 17 Stripe call sites construct `stripeapi.Client`/`ReadOnlyClient`; no raw `http.Client` reaches Stripe. The old audit (`grep "stripe.com" \| grep -v stripeapi`) is a permanent false positive — there is no `stripe-go` dep, so every compliant call site contains the literal URL. Nothing structurally stops call site 18 from dialling directly |
| IDEM-9 | The operating-mode gate is a total function — an unknown origin parks rather than guessing. | `chk_rail_intents_origin`, `0001_schema.up.sql:2359` | **DB** | the app-side `default:` arm (`intents/gate.go:46-49`) is **dead**: the CHECK admits only user/admin/system and the Go `Origin` type declares exactly those three. The rule holds; the *enforcement* is the CHECK. Note `GateExecution` also returns "not blocked" on a nil `ModeView` (`gate.go:35`) — see §10 GAP-18 |
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
| ID-11 | **Every UNIQUE index on a merchant-owned table is scoped by `merchant_id`.** A cross-merchant unique is an existence oracle: under RLS the conflicting row is invisible, so the victim sees only an opaque insert failure. Checked TWICE against ONE shared exemption list — `TestUniqueIndexesAreMerchantScoped` derives the inventory from the migration text (no database, catches a bad migration); `TestGAP10_UniqueIndexesAreMerchantScoped` reads `pg_indexes` on a live DB as `openrails_app` (catches an index that arrived some other way). Both have vacuity guards. | `migrations/postgres/unique_scope_exemptions.go` (the ONE list); guards in `merchant_aware_schema_test.go` and `internal/invariantaudit` | **DB** + **T** |

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
| FC-10 | ~~An unknown intent origin parks.~~ **Dead branch** — no writer can persist a fourth origin (IDEM-9). Kept as a total-function backstop, not counted as a guard. | `intents/gate.go:46-49` |
| FC-11 | A Stripe write under `readonly` errors at the transport. | `stripeapi.go:56-60` |
| FC-12 | Missing merchant on context errors; there is no default merchant. | `pkg/merchant/merchant.go:50-54` |
| FC-13 | A captcha that does not verify → invalid, not pass. | `ratelimit_neutral.go:286-293` (the `err != nil` and `!result.Success` legs). The `Verifier == nil` leg at `:281-284` is **dead**: it is gated behind `cfg.IsEnabled()` and `NewVerifier` only returns nil when that same flag is off |
| FC-14 | Spendgate Redis errors propagate — no default-allow. | `spendgate/gate.go:247-250,269-271` |
| FC-15 | A delegated invoker with no explicit budget grant may never spend the payer's money. | `admission/admitter.go:164-168` (live path: `pkg/service/admission.go:132` → `embed/client.go:186`). Caveat: `admitter.go:124-126` short-circuits `EstimatedAmount <= 0` to allowed *before* the check, so a zero-amount admit skips it — nothing is spent on that path, but the guard is not total |

| FC-16 | **Every background worker that touches a merchant-owned table runs inside `RunInMerchantConn`/`MerchantTx`.** A worker that does not is not "cross-merchant" — it is blind, and every guard downstream of its reads becomes vacuous. | unwritten; violated by `river/jobs_provider_intents.go:49` (or#862). This one rule is what `rate_ceiling.go`, `alerting/store.go`, `reprice_repo.go` and `intents/breaker.go` each violated in a different disguise. Mechanically checkable with an AST rule over `internal/river/**` — §10 GAP-17 |

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
| FAB-6 | A parse failure must not silently become a zero amount. | Catalog drift's NMI plan amounts were the last `return 0` on parse failure (a zero amount raises drift against every local price); both copies now use the exact parser and SKIP the plan with a warning. `pkg/service/catalog_drift.go`, `river/jobs_catalog_reconciliation.go` |

## 9. Destructive and irreversible actions

Provider-side deletes and cancels cannot be undone by us. An NMI vault delete destroys the
stored card; the customer must re-enter it.

| # | Invariant | Status |
|---|---|---|
| DES-1 | **Cancellation is a last resort.** Terminal cancel and provider-side delete require *certainty* — a non-retryable decline, or genuinely exhausted dunning. Never a date comparison, never an absence. | **ENFORCED** (or#821) — `gateCancelCertainty` is one chokepoint in `Decide`; every plane inherits it. `RemoteGone` (provider's own word) is certainty; our inference is not. |
| DES-2 | Stale or unreadable provider data parks as `unknown` and never costs entitlements. | FO-2 upholds the entitlement half |
| DES-3 | **NMI rebill is infinite retry.** Missed periods are forgiven; the customer is paid up from the latest success. A lapsed expiration is not a dead schedule — only provider roster state classifies. | **ENFORCED** (or#821) — a stale `next_billing_date` now parks as `unknown`; the `NonRetryableDecline` leg is wired to real rail codes and unrecognised codes are retryable by construction. |
| DES-4 | Absence is not evidence of death. A subscription missing from a pull, an empty response, a 404, or a timeout must not retract standing. The confirmed-absence gate must hold at **every** retraction site. | **MOSTLY ENFORCED** (or#842) — confirmed-absence gate extended to every retraction site; an empty roster is no longer stamped exhaustive; `diff.go`'s hardcoded `forceExhaustive` removed. Residual: #839/#840 in the collection engine. |
| DES-5 | No unattended irreversible provider call without provider-confirmed certainty, a blast-radius cap, and an operator kill switch that works without a deploy. | **SPLIT.** The switch itself is **ENFORCED** and genuinely fail-closed — `destructive_action_switch` is RLS-exempt on purpose, the read is a `LEFT JOIN` off `(SELECT 1)` so `ErrNoRows` is impossible, `COALESCE(s.enabled,false)` reads an unreadable switch as OFF, and OFF means deny. The `jobs_dunning`/or#856 residuals are closed. But on the **background intent-runner plane the gate is never consulted**: `river/jobs_provider_intents.go:49-66` runs `RunExecuteOnce` on a bare job context, so `ClaimDue` claims zero intents and neither the kill switch nor the #679 volume breaker executes at all — or#862. |

## 10. Known gaps — rules we hold but do not enforce

Every one of these is a rule the codebase intends. Rows are retained after closure as a
log, so read the **Tracked** column before trusting the Gap column: only rows without a
**FIXED** / **CLOSED** / **PARTIALLY CLOSED** marker are still unenforced. A row marked
**STRENGTHENED** or **PARTIALLY CLOSED** names the residual explicitly — that residual is the
live gap, not the original text. Tracker issues in `~/open-rails-tracker/openrails/`.

Still open as of 2026-07-28: GAP-15 (untested live Solana amount formatter), GAP-17
(RLS-blind workers), GAP-18 (fail-open `GateExecution` default). GAP-11, GAP-12, GAP-13 and
GAP-14 are open only in their named residuals; GAP-16 is closed.

| # | Gap | Evidence | Tracked |
|---|---|---|---|
| GAP-1 | **Integers-only for money is convention only.** `MajorUnits` is a `float64` money type reaching the live NMI wire; Solana base units round-trip through float and overcharge 1.19% of whole-cent amounts; FX multiplies an amount by a float; a refund amount arrives as a JSON float. | `moneyutil.go:27`; `nmi/subscriptions.go:113`; `solana/support.go:63,122`; `fx/provider.go:79`; `admin_findings_actions.go:355-361` | **CLOSED** #818 + the guard. `MajorUnits` deleted, Solana integer rescale, integer tolerances — and now `TestNoFloatsInMoneyPackages` (AST, per-declaration allow-list) fails CI on a new float. Turning it on found five MORE live violations #818 had not reached: `fx.ConvertAmount` float-multiplied an amount then `math.Ceil`'d it; `intents.nmiAmountToCents` `ParseFloat`'d a refund amount that is compared for EQUALITY to decide whether a provider refund is ours; catalog drift `ParseFloat`'d NMI plan amounts and returned 0 on failure; the Solana token base-unit amount round-tripped through JSONB as a JSON number; and `webhooks.Stringish` decoded bare JSON numbers through `float64`, mangling any NMI id past 2^53. All five fixed. Residual: GAP-13's package holes. |
| GAP-2 | **Currency is silently defaulted in two paths** — both substitute a lowercase, unregistered `"usd"` when the provider gave none, one while minting a `payments` row. Violates CUR-9 and FAB-6. | `solana/support.go:116-119`; `modules/reconcile/reconcile.go:570-573` | **FIXED** #830 — three fallback sites removed (one more than filed); a missing provider currency now errors or parks. |
| GAP-3 | **Terminal cancel + NMI vault delete fire on a stale date.** Past-due is inferred from `next_billing_date < today` with no decline evidence; beyond 14 days that becomes an irreversible provider delete. The decider's own comment admits the remote may still be retrying. | `reconcile/nmi.go:181-186`; `decider.go:315-319`; `lifecycle_service.go:1533-1540` | **FIXED** #821/#834/#835/#837/#841/#842 — measured: empty roster went 40 cancellations → 0. |
| GAP-4 | **RLS coverage is not checked beyond migration 0001.** `payment_settlement_events` (0005) carries `merchant_id` and full CRUD grants with no RLS. The guard test reads only `0001` from a hardcoded list. | `0005_payment_settlements.up.sql:1-19`; `merchant_aware_schema_test.go:14-77` | **FIXED** SEC-16 — guard now derives its table list from ALL migrations; 5 tables had partial-only merchant_id indexes (or#846). |
| GAP-5 | **Duplicate payments and subscriptions are representable.** The uniques are partial and disjoint on `rail_merchant_account_id IS NULL`, so the same `(merchant, rail, transaction_id)` can exist twice — once legacy, once account-attributed. | `0001_schema.up.sql:1119` vs `:1121`; `:1010` vs `:1012` | **FIXED** #831, then CORRECTED by #17 — 0012 over-tightened to (merchant, rail, id), forbidding two PSPs issuing the same provider id; 0017 restores per-PSP scope via COALESCE. |
| GAP-6 | **Currency codes in the DB are unvalidated.** Deliberately app-only across 16 columns; the ledger trigger checks equality, not validity. Nothing stops an import or GAP-2 persisting an unregistered code. | `money/currency.go:11-13` | **FIXED** or#832 — migration 0020 adds a `*_currency_shape` CHECK to all 16 currency columns, applied by iterating `information_schema` (not a hand-written list — the lesson of GAP-4) and guarded by `TestCurrencyColumnsCarryShapeCheck`. It pins CASE and rough shape, not membership; membership stays in the Go registry. Measured cause of the drift: `pkg/catalog`'s loader lower-cased on the way in. Running the CHECK against the integration suite then found FOUR live write paths that were storing the rail's own casing — the CCBill webhook, the Stripe converge backfill, `Service.CreatePrice`, and every rail reaching the payment repo — all now canonicalised at the boundary. **Correction to the original claim:** "the write boundary is not one place" is true for the schema as a whole but FALSE for the two tables that carry most currency rows; payments and prices each have exactly one INSERT chokepoint, and those now normalise, so the CHECK is a backstop there rather than the only defence. See CUR-6. |
| GAP-7 | **`transfer_type` has no CHECK.** `account_type` does. A typo in a new transfer type silently bypasses the lot-once uniqueness index. | `0001_schema.up.sql:1663` vs `:1617` | **FIXED** #832 — CHECK + typed Go constants pinned to each other; the live vocabulary was 7 values, not 5, and `credit_reinstate` existed only as a bare literal. |
| GAP-8 | **Ledger conservation is never computed.** `sum(balances) = 0` per `(merchant, currency)` is structural, but nothing checks it, and the account counters are a maintained projection that can drift from `ledger_transfers` if the trigger is ever bypassed (superuser, restore, `COPY`). | trigger `:164-169`; no diagnostic found | **FIXED** #833 — `CheckConservation` + `CheckCounterDrift` + `openrails ledger-audit`; a trigger-bypass drift test proves it catches real drift. |
| GAP-9 | **org ↔ merchant 1:1 is not enforced.** The decision was a unique `owner_org_id`; the shipped column is `permission_group_id` with a **non-unique** index, so two merchants can claim one group and merchant-from-group resolution is ambiguous. | `0001_schema.up.sql:274` | **FIXED** #843 — `uq_merchants_permission_group_id`, partial on live merchants. |
| GAP-10 | **Three unique indexes lack `merchant_id`** — `checkout_sessions` on `(rail, reference)` and `(rail, transaction_id)`, plus `uq_catalog_drift_open`. Under RLS the conflicting row is invisible, so one merchant blocks another's insert: a cross-tenant existence oracle and a claim-squat. | `0001_schema.up.sql:1181,1183`; `uq_catalog_drift_open` | **FIXED** SEC-24 — migration 0020 scopes all three by `merchant_id` (and by PSP, via 0017's COALESCE-the-nullable technique so one TOTAL index keeps per-PSP scope). Made permanent by `TestUniqueIndexesAreMerchantScoped`, which derives the unique-index inventory from every migration and fails on a new cross-merchant unique; the five legitimate exceptions are exempted BY NAME with reasons. |
| GAP-11 | **The provider-write guard is textual.** A wrapper, method value, or renamed import reaches the same wire call invisibly, and an allow-listed file is trusted wholesale. | `enforcement_guard_test.go:15-24` | **STRENGTHENED, not closed.** Now AST-based with the allow-list keyed by `file:FUNCTION`, so method values, interface dispatch, renamed imports and a second call inside an allow-listed file are all visible. A second guard, `TestProviderWriteSurfaceIsClassified`, inventories the client's exported surface and fails until every method is classified read or write — which closed the real hole: six writes (`AddRecurringPlan`, `EditRecurringPlan`, `CreateCustomerVault`, `UpdateCustomerVault`, `UpdateRecurringSubscription`, `Void`) were never in the old token list, so their call sites were unguarded. **Residual: strength is T, not S.** Go has no friend visibility; making bypass a COMPILE error needs the write client moved under `internal/intents/internal/…`, which the legitimate reactive callers make a large refactor. Not attempted. |
| GAP-13 | **The float guard's package list has holes, and two live violations sit in them.** `TestNoFloatsInMoneyPackages` is a real AST check, but its `guarded` list omits `internal/http/handlers/`, `internal/reconcile/` (it guards the *different* `internal/modules/reconcile/`), `internal/integrations/pyth/` and `internal/river/`. A refund amount is read as a JSON `float64` and truncated (`admin_findings_actions.go:355-361` — the very site GAP-1 marked FIXED), and an on-chain token amount round-trips through `float64` (`reconcile/local.go:425-431`). | those two files | **CLOSED** or#863 — the list gains `internal/reconcile/`, `internal/http/handlers/`, `internal/river/`, `internal/integrations/{pyth,solana,basistheory}/`, `internal/modules/payments/`, `internal/controlplane/`; both violations fixed (`override_params` binds RAW and decodes with `UseNumber`, `paramAmountMicros` parses an exact `json.Number`/decimal string and REFUSES a float; the Solana `case float64` is deleted so an unreadable amount parks). The guard was verified failing on each violation before its fix. Residual: packages outside the list are still convention-only. |
| GAP-14 | **The "single internal→rail converter" is bypassed.** `NativeToRailMinor` had 2 non-test callers; 14 sites called `moneyutil.MicrosToCents*` directly, hardcoding ÷10 000. | `checkout/*`, `subscriptions/*`, `webhooks/*`, `pkg/service/*`, `handlers/admin_payments.go` | **CLOSED** or#863. **Correction to the original claim:** those sites were NOT wrong by 10⁴ for JPY. Every registered currency shares a native shift of 4 (USD/EUR 6−2, JPY 4−0), so ÷10 000 was numerically right for all three. The real defect is that a converter which cannot see the currency cannot REFUSE one — it converted amounts whose currency nobody had established, which is what made GAP-16's pre-charge guesses invisible — and it was a silent landmine for the first currency registered with a different shift. Fix: the registry moved to the leaf `internal/shared/moneyutil` (it had to — `internal/modules/money` imports `internal/modules/subscriptions`, so the NMI plan pushes could never have reached it), all 14 sites routed, `MicrosToCents*` DELETED, and `TestRegistryNativeShiftIsUniform` turns the shift coincidence into a tripwire. Residual: the ~31 inbound `CentsToMicros` sites stay currency-blind under that tripwire, and NMI's `/100` cents↔decimal-string edges stay — they are the rail's WIRE FORMAT (`amount` is documented `x.xx`), not an internal→rail conversion. Named residual: that wire format is 2-decimal-only, so putting a ZERO-decimal currency on NMI would need `WireAmount`/`centsJSONAmount` made scale-aware. No merchant runs one today. |
| GAP-15 | **Solana's live Pay-URL amount formatter has no test.** `solana/pay.go:607 formatTokenAmount` reaches the wire; the *tested* formatter is a near-duplicate, `solana/support.go:166 FormatBaseUnits`. Two divergent formatters, only the untested one live. CCBill's outbound refund pin encodes an admitted dollars-vs-cents guess. | `solana/pay.go:308-311,607` | or#863 |
| GAP-16 | **CUR-9 is violated by its own cited enforcement site**, which borrows the subscription's currency for a decline that carried none — and says so in a comment. Three further paths substitute `DefaultCurrency`, two of them immediately before charging a card. | `reconcile/unknown_orchestration.go:281-285`; `money/nmi_collection.go:51-53`; `money/vaultedcard_collection.go:43-45`; `handlers/admin_users.go:112-114` | **FIXED** or#864. The two pre-charge substitutions are gone and the gate is hoisted to `ScopedCharger` — the one dispatch point for every off-session collection — where a REGISTERED currency is required; `stripe_collection.go`, which never defaulted but also never checked, is covered by the same gate. The admin profile no longer defaults a query-param currency (top-level `trust_level` only when the caller names one; each credit-balance row now carries its own). The decline borrow stays but records `currency_provenance=inherited_from_subscription_price` and logs it — the difference between an inference and a fabrication is whether a reader can tell. `money.DefaultCurrency` survives only as a declared CONFIG default: the FX base/accounting unit, the `currency != DefaultCurrency` FX-needed test, and NMI's v5 vault billing-currency field (a record field, no amount attached). |
| GAP-17 | **The RLS-blind-worker class is not fully closed.** Fixed 2026-07-28: the #732 rate ceiling and both armed-merchant scans now go through migration 0021's definer readers, and the settlement feed runs under `MerchantTx`. Still inert: the #816 re-driver (`reprice_repo.go:189`), fleet analytics/timeseries, and — worst — the whole background intent-executor plane, which claims zero intents so the #836 kill switch and #679 volume breaker never execute (or#862). | `db_pgx.go:66-86` | or#861, or#862 |
| GAP-18 | **`GateExecution` returns "not blocked" on a nil `ModeView`,** disabling `readonly`, `limited` and the origin check in one line. Not live today (`Runner.Config` is always set), but it is a fail-*open* default in a fail-closed gate. | `intents/gate.go:35` | or#865 |
| GAP-12 | **`Micros` is opt-in.** DB columns are bare `bigint` and most Go structs use `int64`; the unit is carried by column comments. The type only bites where someone chose it. | `moneyutil.go:18` vs `ledger/ledger.go:118` | **PARTIALLY CLOSED** #818 — scoped to the highest-value boundary: the unit CONVERTERS. `CentsToMicros` / `MicrosToCentsCeil` / `MicrosToCentsExact` are the only places a value changes unit and were `int64 -> int64`; they are typed now, as are `ParseDecimalToCents` and the `Format*` helpers, and the type propagated outward on its own into the NMI plan pusher, the CCBill amount-tolerance comparison, the chargeback matcher and the admin refund path. **Deliberately left:** DB columns stay bare `bigint`; most struct fields stay `int64`; `moneyutil.NativeToRailMinor` keeps an `int64` input because its argument is internal units at the CURRENCY's registered scale (JPY is 10⁴), which is not always micros — typing it `Micros` would be a lie. GAP-14 closed the other half: `MicrosToCentsCeil` / `MicrosToCentsExact` are deleted and the surviving converters are currency-aware. |

### Audit bundle

Run these as `openrails_app`, not as a superuser — and read the next paragraph first.

> **Three of these queries cannot fail as written.** GAP-8 (conservation), GAP-2/6 (currency
> codes) and GAP-5 (duplicate transactions) all read merchant-owned, RLS-policied tables. As
> `openrails_app` with no `app.merchant_id` they return **zero rows whatever the data is** —
> measured 2026-07-28 against a seeded database: the app role saw 0 ledger accounts where the
> superuser saw 4. Run as a superuser they are honest but do not reproduce production; run as
> the app role they are green by construction. Either way they were not checking anything.
> They are marked below and must be run either per-merchant under a GUC, or through
> `openrails ledger-audit` (which takes a merchant), or from a `SECURITY DEFINER` reader. The
> remaining three (TEN-1/3, GAP-10, GAP-9) read `pg_catalog` or the policy-free `merchants`
> directory and are sound on any role.
>
> The executable form of this bundle is `go test -tags integration ./internal/invariantaudit`,
> which asserts `NOT rolsuper AND NOT rolbypassrls` before it asserts anything else.

Read-only checks that should pass at any time:

```sql
-- TEN-1/TEN-3: tables without RLS (expect exactly merchants, probe_verdicts, worker_health)
SELECT relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE n.nspname='openrails' AND c.relkind='r' AND NOT c.relrowsecurity;

-- ID-11 (was GAP-10): unique indexes not scoped by merchant.
-- Expect ONLY surrogate-id *_pkey rows plus the named exceptions in
-- migrations/postgres/unique_scope_exemptions.go. NOT RLS-blind — pg_indexes is catalog.
SELECT indexdef FROM pg_indexes WHERE schemaname='openrails'
   AND indexdef LIKE '%UNIQUE%' AND indexdef NOT LIKE '%merchant_id%';

-- CUR-5b: every currency column carries the shape CHECK (expect 16).
SELECT count(*) FROM pg_constraint
 WHERE contype='c' AND conname LIKE '%\_currency\_shape' ESCAPE '\'
   AND connamespace='openrails'::regnamespace;

-- GAP-8: ledger conservation per (merchant, currency)
-- ⚠ RLS-BLIND as openrails_app: returns nothing regardless of state. Run per
--   merchant under app.merchant_id, or via `openrails ledger-audit <merchant>`.
SELECT merchant_id, currency, sum(credits_posted - debits_posted) FROM openrails.ledger_accounts
 GROUP BY 1,2 HAVING sum(credits_posted - debits_posted) <> 0;

-- GAP-2/GAP-6: lowercase currency codes. Now impossible to insert (CUR-5b), so
-- this is a post-migration audit of legacy rows rather than a live guard.
-- ⚠ RLS-BLIND as openrails_app (see above).
SELECT DISTINCT currency FROM openrails.payments;

-- GAP-5: duplicate provider transactions
-- ⚠ RLS-BLIND as openrails_app (see above).
SELECT merchant_id, rail, transaction_id, count(*) FROM openrails.payments
 GROUP BY 1,2,3 HAVING count(*) > 1;

-- GAP-9: merchants sharing a permission group
SELECT permission_group_id, count(*) FROM openrails.merchants
 WHERE permission_group_id IS NOT NULL GROUP BY 1 HAVING count(*) > 1;
```

Test gates:

```
go test -tags integration ./internal/invariantaudit                   # TEN-1/2/3/9, LED-2/3/5/6/7, MONEY-8, CUR-1/3, GAP-7/9/10 — as openrails_app
go test ./migrations/postgres                                         # TEN-4, TEN-12, MONEY-9, ID-11, CUR-5b
go test ./internal/intents  -run TestProviderWrite                    # IDEM-7 (both halves: surface + call sites)
go test ./internal/shared/moneyutil -run TestNoFloatsInMoneyPackages  # MONEY-3
go test ./internal/merchants -run TestNoAdHocSecretPathConstruction   # secret-path builder
go test ./internal/merchants -run TestSecretNameRejectsTraversal      # SEC-24 item 6
go test ./internal/merchants -run TestEncryptedSecretStore_CiphertextIsBoundToItsRow  # SEC-24 item 1 (AAD binding)
go test ./internal/http/handlers -run TestStripeRelatedObjectURLIsAlwaysAPath         # SEC-24 item 5
go test .                   -run 'TestRootPackageStaysLight|TestModuleIsGinFree'
```

---

## Adding an invariant

State it imperatively, then answer three questions before merging: where is it enforced,
how strongly, and what fails when it breaks. If the answer to the third is "nothing", it
belongs in §10 with a tracker issue — not in §1–§9.

Prefer enforcement in this order: make it unrepresentable (a type, a role privilege, a
chokepoint), then a DB constraint, then a test. Comments and review are not enforcement.

And answer a fourth question: **can the check fail?** A guard whose input is structurally
always empty, a `default:` arm no writer can reach, a `grep -l` with no expected value, an
allow-list entry policing a token that only appears where the walker refuses to look — each
reads as protection and stops anyone looking. Every one of those shapes was found in this
register on 2026-07-28. Grade them `C`, or delete them; do not leave them looking like `S`.
