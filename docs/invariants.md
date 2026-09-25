# Invariants

**Coverage status after the September 2026 harness removal:** this register
preserves design requirements and historical audit findings. Except where
explicitly refreshed, its test names, enforcement labels, commands, and source
line numbers describe the earlier audit; they are not current regression
evidence. Deleted tests confer no coverage. Use the
[focused coverage map](greenfield-coverage.md) for the maintained scenarios and
remaining gaps. Database and application constraints must be assessed separately
from whether a surviving test exercises them.

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
| **DB** | Postgres-enforced — CHECK, trigger, unique index, foreign key |
| **APP** | enforced at a single application chokepoint or validation function |
| **T** | a repo test fails |
| **C** | convention only — nothing fails |

**Historical verification.** The former tenant SQL and public API suites used
both owner and ordinary runtime connections with RLS absent. Financial tests
exercised constraints, transaction semantics and immutable-record triggers.
Those broad suites have been removed; the retained RLS failure analysis below
does not describe current authorization or current test coverage.

**History.** Migration numbers in FIXED notes are history; the fresh baseline
`internal/migrate/postgres/0001_schema.up.sql` carries every constraint.

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
| MONEY-1 | Internal money is units at the CURRENCY's registered scale — micros (10⁶) for USD/EUR, 10⁴ for JPY. Not "micros everywhere": the registry decides. | `internal/shared/moneyutil/currency.go` (registry); `moneyutil.go:9-13` (the 10⁶ constants) | C + T | `go test ./internal/shared/moneyutil -run TestRegisteredCurrenciesShareNativeShift` |
| MONEY-2 | `Micros` and `Cents` are distinct defined types, and **the unit converters take and return them**, so handing cents to a micros parameter is a compile error at the one place a value changes unit. The currency-BLIND narrowing converters are gone: `NativeToRailMinor` / `NativeToRailMinorExact` / `RailMinorToNative` are the only exported internal↔rail conversions and each requires a registered currency. | `moneyutil.go:18,22`; `currency.go` converters | **S** at the converters, **C** elsewhere | `go build ./...`; `internal/shared/moneyutil/money_test.go` pins the typed signatures so a silent revert to `int64` fails. Coverage limits: §10 GAP-12 |
| MONEY-3 | **Currency and crypto amounts are integers only.** No float may represent, convert, round, or compare an amount. A *rate* may be a float; arithmetic that produces or compares an *amount* may not. | `internal/shared/moneyutil/float_guard_test.go` (AST, allow-list keyed by declaration) | **T** over the listed packages, **C** elsewhere | `go test ./internal/shared/moneyutil -run TestNoFloatsInMoneyPackages` — see §10 GAP-13 for the packages it does not cover |
| MONEY-4 | Micros→cents is exact-or-error on the exact path; the ceil path never under-charges. | `moneyutil.go:60-72` | APP | `internal/shared/moneyutil/money_test.go` `TestRailConversions` |
| MONEY-5 | The single internal→rail converter is `moneyutil.NativeToRailMinor` (ceil) / `NativeToRailMinorExact` (errors on a sub-minor remainder); both error on an unregistered currency. Callers cannot guess a scale — there is no currency-blind converter left to call. | `internal/shared/moneyutil/currency.go` | **S** — the alternatives are deleted, not deprecated | `grep -rn "MicrosToCents" --include=*.go` → no hits; `go test ./internal/shared/moneyutil` |
| MONEY-6 | Decimal strings parse via exact rational (`big.Rat`), half-away-from-zero, with an int64-overflow error. | `moneyutil.go:28-41,112-141` | APP | `internal/shared/moneyutil/money_test.go` `TestParseDecimalToCents` pins rounding, MinInt64 and overflow; `internal/modules/webhooks/nmi_test.go` `TestNMITransactionAmountCents` covers the provider path |
| MONEY-7 | Provider-boundary money coverage is partial. The focused contract pins exact parsing and currency conversion; it does not enumerate or pin every provider wire call site. | `ci/greenfield/money_test.go` | **T** for the cases asserted | `go test -tags greenfield ./ci/greenfield -run TestExactIntegerMoneyBoundaries` covers rounding, overflow, unknown currencies, USD sub-cent rejection, and JPY scaling. The former AST boundary registry was removed with the legacy suite. Provider request formatting, call-site completeness, and Solana Pay formatter coverage remain separate gaps; see GAP-15. |
| MONEY-8 | `ledger_transfers.amount > 0`; `allow_debit_negative_up_to >= 0`. | `ledger_transfers_amount_positive`, `ledger_transfers_debit_floor_nonnegative` | **DB** | `SELECT count(*) FROM billing.ledger_transfers WHERE amount<=0;` → 0 |
| MONEY-9 | `payments.amount` deliberately has **no** non-negative CHECK — refunds are negative rows — and a test forbids adding one. | `internal/migrate/postgres/amount_checks_test.go` (`assertNoPaymentsAmountCheck`) | **T** | `go test ./internal/migrate/postgres -run TestAmountValueChecks`. or#865: previously matched two literal constraint names in the baseline only, so the same CHECK added in a **later migration** or under Postgres' **generated** name passed silently — the two ways it would actually break. Now every migration file is scanned and the match is on the predicate as well as the name, anchored so `invoice_payments_amount_positive_chk` (a different table, legitimately positive-only) is not a false positive. Both holes proven to fail. Still static text, not a live `pg_constraint` query |
| MONEY-10 | Amount CHECKs hold across prices, grants, invoices, invoice items/payments, usage events, credit limits, rating watermarks. | `0001_schema.up.sql` (the `*_amount_*` CHECKs) | **DB** | `SELECT conname FROM pg_constraint WHERE contype='c' AND connamespace='billing'::regnamespace;` |

## 2. Currency

**Every amount carries its currency.** No amount is ever ambiguous, and no code path may
substitute a default currency because one was not supplied.

| # | Invariant | Enforced at | Str | Audit |
|---|---|---|---|---|
| CUR-1 | Every stored amount carries a currency: 16 currency columns, 15 `NOT NULL`. | `0001_schema.up.sql` (every `currency` column) | **DB** | `SELECT table_name FROM information_schema.columns WHERE column_name='currency' AND table_schema='openrails' AND is_nullable='YES';` → only `grants` |
| CUR-2 | The one nullable currency is conditionally required: `grants.kind='credit' ⇒ amount AND currency NOT NULL`. | `grants_credit_amount` | **DB** | `\d openrails.grants` |
| CUR-3 | **FX is forbidden inside the ledger.** A transfer whose account currency differs from the transfer currency raises. | trigger `ledger_transfers_apply_counters()`, wired `trg_ledger_transfers_apply_counters` | **DB** | attempt a cross-currency transfer in psql → must raise |
| CUR-4 | FX is not merely forbidden but absent — the `fx_liquidity` account type has no non-declaration call site. | `ledger_accounts_type_check`; `money/ledger/ledger.go:39` | **C** | `grep -rn "FXLiquidity" --include=*.go \| grep -v _test` → the declaration only, zero call sites. True by habit, not structure: `ledger.FXLiquidity` is an ordinary exported const and passing it to `ensureAccount` compiles |
| CUR-5 | Currency **membership** comes from a Go registry with per-currency internal and rail decimals; the DB does not encode membership (it would need a migration per currency). | `internal/modules/money/currency.go:11-13,26-30` | APP | `money.ValidateCurrency` |
| CUR-5b | Currency **shape and case** are Postgres-enforced on all 16 currency columns: upper-case `[A-Z0-9]{3,12}`. | constraint `<table>_currency_shape` | **DB** + **T** | `SELECT count(*) FROM pg_constraint WHERE conname LIKE '%_currency_shape';` → 16; `go test ./internal/migrate/postgres -run TestCurrencyColumnsCarryShapeCheck` |
| CUR-6 | **UPPER case is the canonical internal form.** Established at the two INSERT chokepoints (`paymentInsertParams` behind both payment inserts; `PriceService.Create` behind every price insert) and at each provider INGESTION boundary, with the CUR-5b CHECK as the backstop that catches anything reaching the DB by another route. Lowercase survives ONLY where a rail wire demands it, at three sites that say so: Stripe's catalog and invoice APIs, and the FX endpoint. | `moneyutil.NormalizeCurrency` (ONE definition, in the leaf so the repo chokepoints can reach it); `payments/payment_repo.go`, `catalog/price.go`; ingestion `webhooks/ccbill.go` `requireCCBillCurrency`, `reconcile/unknown_orchestration.go`; wire exceptions `catalog/stripe_catalog.go`, `subscriptions/stripe_invoice_collection.go`, `fx/exchange_api.go` `fetchRate` | APP (chokepoint) + **DB** | `SELECT DISTINCT currency FROM billing.payments;` → all upper |
| CUR-7 | Billing surfaces require a registered currency. | `RequireBillingCurrency`, `internal/modules/money/currency.go` | APP | `grep -rn "RequireBillingCurrency"` |
| CUR-8 | Service entry points require a REGISTERED currency — `"XYZ"` no longer passes. It is still a helper an entry point can forget to call, so total enforcement lives where it cannot be bypassed: every off-session charge is registry-validated at `ScopedCharger.Prepare`, and every internal→rail conversion refuses an unregistered currency by construction. | `internal/service/currency.go`; `money/collection.go`; `moneyutil.NativeToRailMinor*` | APP at the entry point, **S** at the charge/convert boundary | `go test ./internal/service -run TestRequireCurrencyConsultsTheRegistry`; `go test ./internal/modules/money -run RefusesUnestablishedCurrency` |
| CUR-9 | **A missing provider currency is never fabricated, defaulted, or silently borrowed.** A decline carries no currency of its own; where the subscription's billing currency is genuinely the one the attempt was denominated in, the backfill may INHERIT it — and the row records `currency_provenance` so an inference is never mistaken for an observation. No path substitutes a default before a charge. | `internal/reconcile/unknown_orchestration.go` (inherit + provenance); `money/{collection,nmi_collection,custodian_proxy_collection,stripe_collection}.go` (refuse); `handlers/admin_users.go` (no query-param default) | APP + **T** | `go test ./internal/modules/money -run RefusesUnestablishedCurrency` — §10 GAP-16 |

## 3. Tenant isolation

Authorization resolves a merchant before persistence. Tenant SQL uses explicit
merchant predicates and composite references; missing scope cannot select a global
operation. OpenRails does not install RLS policies or rely on login flags.

| # | Invariant | Enforced at | Audit |
|---|---|---|---|
| TEN-1 | All tenant tables have a non-null merchant coordinate; the fresh schema has no RLS policies/flags. | migration schema guards | `TestTEN1_ExplicitScopeSchemaWithoutRLS` |
| TEN-2 | Foreign or missing merchant scope cannot read or modify a tenant resource, including with the owning connection. | scoped SQL plus service authority checks | `TestTEN2_QueryScopeIndependentOfDatabaseRole`, `TestMerchantScopeWithoutRLS` |
| TEN-3 | `merchants`, `worker_state`, and `destructive_action_switch` are deliberate global objects. | schema classification | `TestTEN1_ExplicitScopeSchemaWithoutRLS` |
| TEN-4 | Merchant columns have no implicit/default tenant; cross-resource foreign keys preserve merchant identity. | schema guards and composite FKs | `go test ./internal/migrate/postgres` |
| TEN-5 | Tenant services require verified scope; a request body or fetched row cannot mint authority. | `merchant.Require`, route Gate, scoped repositories | public Client/HTTP adversarial tests |
| TEN-6 | Session merchant state remains transaction-local or bound to a released request connection for explicit GUC predicates and stored functions. It does not filter arbitrary SQL. | `MerchantTx`, `WithMerchantConn` | connection lifecycle tests |
| TEN-7 | A connection whose merchant-state reset fails is closed rather than reused. | `lazyMerchantPgxConn.release` | connection lifecycle tests |
| TEN-8 | Initialization and runtime can share one owning application pool; tenant correctness is independent of superuser/BYPASSRLS flags. | runtime construction and SQL scope | owner and normal-login journeys |
| TEN-9 | Libraries create no database accounts or permission-group roles. A separate runtime login may receive optional direct grants. | migration API `RuntimePool` | provisioning/owner integration tests |
| TEN-10 | Platform directory and worker-discovery scans are explicit; tenant work runs under each authorized merchant's scope. | `GenDirectory`, worker fan-out | destructive ceiling and worker integration tests |
| TEN-11 | Webhooks resolve the merchant and verify the signature with that merchant's secret before applying evidence. | webhook handlers | signed webhook and provider-collision tests |
| TEN-12 | Core schema has no foreign keys to AuthKit and does not own host River tables. | portability guards | `TestPortabilityInvariant` |

## 4. Ledger integrity

| # | Invariant | Enforced at | Str | Audit |
|---|---|---|---|---|
| LED-1 | A transfer moves an amount within one `(merchant, currency)` ledger; both accounts lock `FOR UPDATE ORDER BY id` before counters move. | trigger `ledger_transfers_apply_counters()` | **DB** | — |
| LED-2 | A missing debit or credit account raises — never a silent no-op. | `:147-149` | **DB** | — |
| LED-3 | **Insufficient-funds floor**: debiting below `-allow_debit_negative_up_to` raises `ledger_insufficient_funds`. | trigger `:155-159`; flag set in `ledger.go:69-72` | **DB** | `SELECT … WHERE account_type='customer_balance' AND NOT debits_must_not_exceed_credits;` → 0 |
| LED-4 | Symmetric ceiling for `credits_must_not_exceed_debits` accounts. | `:160-162` | **DB** | — |
| LED-5 | Ledger records are append-only under ordinary DML even for an owner; account counters change through transfer triggers. Optional runtime grants remain narrow. Owner DDL can deliberately alter these guards. | immutable ledger triggers; runtime access template | **DB** + **T** | `TestOwningLoginInitializesWithoutRLSAndPreservesFinancialFacts`, `TestLED5_OptionalRuntimeGrantsKeepLedgerAppendOnly` |
| LED-6 | Debit ≠ credit account. | `:1674` | **DB** (via the trigger, not the CHECK) | the BEFORE trigger runs first and rejects the self-transfer as "account not found", so `ledger_transfers_distinct_accounts` never fires. Still rejected — just not by the constraint the row cites | — |
| LED-7 | A credit lot's deposit / expire / revoke each happen at most once. | `idx_ledger_transfers_lot_once`, `:1707` | **DB** | — |
| LED-8 | ~~An owed accrual is once per …~~ **Superseded by LED-14**, which covers every transfer type rather than this one. | `idx_ledger_transfers_operation_once` replaced `idx_ledger_transfers_owed_accrual_once` | **DB** | — |
| LED-11 | ~~A balance debit is once per …~~ **Superseded by LED-14.** | `idx_ledger_transfers_operation_once` replaced `idx_ledger_transfers_credit_spend_once` | **DB** | — |
| LED-12 | **`operation` is engine-composed**, never caller-supplied, and is part of the idempotency coordinate: two different money writes sharing one caller `(source, source_id)` — e.g. a wasted-spend overage charge and the capture of the same request — must not alias. | `ledger.go` `Coord.Validate`; `money.NewIdempotencyKey` (or#894) | **APP** + **DB** | `chk_ledger_transfers_coordinate_not_blank` (or#892) makes a blank half unrepresentable; `SELECT count(*) … WHERE operation = '' OR source = '' OR source_id = '';` → 0, enforced not merely audited |
| LED-14 | **ONE unique index covers EVERY transfer type**: `(merchant, customer, currency, transfer_type, operation, source, source_id, grant_id)` `NULLS NOT DISTINCT`. Once-only is a DATABASE fact, not a consequence of `lockBalance` running before a `SELECT`-then-`INSERT` — a new spend path that forgets the lock still cannot double-post. `grant_id` is in the key because a spend fans out one leg per FIFO lot; `NULLS NOT DISTINCT` because the owed/payment legs carry no lot and system transfers no customer, and under the default those rows would escape the index entirely. | `idx_ledger_transfers_operation_once` | **DB** | `TestOr892_TheDatabaseRefusesADuplicateCoordinate` drives the generated INSERT twice with every Go-side guard bypassed. Measured with the index dropped: 2 rows and 10,000 micros at one coordinate where 5,000 moved |
| LED-15 | Every durable money write reports **applied vs replayed**. `ledger.ApplyIdempotent` returns `applied`; `models.MoneyTransaction.Replayed` / `service.CreditTransaction.Replayed` carry it to consumers, so a caller that needs to know whether ITS call moved the money does not rebuild a claim table to find out. A replay is not an error. | `ledger.ApplyIdempotent`; `money.SpendCredits`/`CaptureAuthorized`/`Deposit`/`Withdraw` | **APP** | `TestMoneyWritesPreserveAcceptedAmountAndReplay`, `TestOr892_ConcurrentIdenticalSpendsApplyExactlyOnce` |
| LED-17 | **A deposit happens at most once at the caller's key** `(merchant, customer, source_id)`. Changed amount, unit or expiry returns `ErrIdempotencyKeyReused` (409); an identical replay returns the persisted receipt with `Replayed=true`. Diagnostic source/invoker/description changes cannot rewrite that receipt or create another deposit. Internal relative-expiry grants retain the first grant's instant across retries. | `uq_grants_credit_deposit_once`; `money.depositTx` | **DB** + **APP** | `TestOr906_TheDatabaseRefusesADuplicateDepositGrant`, `TestDepositReplayPreservesFinancialTermsAndReceipt`, `TestDepositTermsThroughEmbeddedAndRemoteClients` |
| LED-19 | **Usage pricing authority is explicit.** `host` rows are final settlement amounts and are excluded from catalog rating, including zero-cost captures; `catalog` rows are unpriced metering inputs and are the only rows consumed by catalog rating and allowance accrual. Every writer supplies the marker. | `usage_events.pricing_authority`; `RecordUsage`; `CaptureAdmission`; `metered_rating.go` | **APP** + **DB** | `TestPricingAuthoritySeparatesFinalCapturesFromCatalogInputs` |
| LED-18 | **Admission capture uses durable original identity and immutable capture terms.** `(merchant, request_id)` binds original payer/unit; the ledger coordinate remains `(capture, admit, request_id)`. Amount and optional usage terms are fixed on first success. State, window usage, ledger and optional usage event commit together; zero actual has no fake ledger transfer. | `money.CaptureAdmission`; `admission_operations`; `idx_ledger_transfers_operation_once` | **APP** + **DB** | `TestAdmissionClientRecoveryAndCaptureReceipt`, `TestDurableAdmissionIdentityCaptureAndWindows` cover Redis loss, changed-body conflicts, usage failure rollback and exact receipt replay |
| LED-16 | **Money idempotency is Postgres; `replaycache` is a cache.** `internal/modules/replaycache` (was `internal/modules/idempotency`) is Redis-backed request-replay coordination with an in-memory fallback, and nothing whose correctness is money may rest on it. The old name is what let a reader assume otherwise. | package doc, `internal/modules/replaycache/service.go` | **DOC** | Known gap: the checkout path still uses it as its SOLE store (or#892, unmeasured) |
| LED-13 | The two money discriminators are **different axes and must not be conflated**: `ledger_transfers.operation` says *which money write posted this leg* (idempotency identity — LED-8/11/12); `payments.money_movement` says *whether this payment row moved real money at the rail* (settlement-feed eligibility — or#827). Both are positive markers replacing an inference, and neither substitutes for the other: a `money_movement='none'` bookkeeping payment can still drive a real ledger operation, and a ledger `operation` says nothing about rail settlement. | `payments.money_movement`; `ledger_transfers.operation` | **DOC** | — |
| LED-9 | Ledger purity: transfer-side `grant_id`/`invoice_id`/`customer_id` carry **no FKs**, so the append-only ledger never blocks or cascades on control-plane rows. | `:1690-1691` | **DB** (by omission) | — |
| LED-10 | Balance reads never create an account. | `ledger.go:74-90` | APP | — |

## 5. Idempotency and provider writes

All outbound provider mutations post a durable intent first, then execute.

| # | Invariant | Enforced at | Str | Audit |
|---|---|---|---|---|
| IDEM-1 | Every provider mutation posts a `rail_intents` row before execution. | `internal/intents/intents.go:1-21` | APP + T | IDEM-7 |
| IDEM-2 | One intent per logical mutation per merchant: `UNIQUE (merchant_id, idempotency_key)`. | `uq_rail_intents_merchant_idempotency_key` | **DB** | — |
| IDEM-3 | **Idempotency keys are content-addressed and stable across retries.** No key may derive from wall clock or randomness. A *monotonic attempt counter* is permitted and is the mechanism by which a deliberate re-attempt maps onto a new intent (`ManualRebillIdempotencyKey`'s `attempt`, `NMIPaymentSourceUpdateIdempotencyKey`'s `priorSwaps`). | key constructors in `internal/intents/*.go` | APP | `grep -rn "IdempotencyKey" --include=*.go internal pkg \| grep -v _test \| grep -iE "time\.Now\|uuid\.New\|rand\."` → empty (verified 2026-07-28). All 12 constructors also traced by hand for indirect clock/random input; clean. Nothing runs this grep in CI |
| IDEM-5 | **Ambiguity verifies, never declines.** An ambiguous outcome parks as `unknown_needs_verify` and is resolved by provider **reads** only. Money movers never blind-retry. | `intents.go:62-66,140-143` | APP | — |
| IDEM-6 | Stripe refunds are double-walled: the intent key is also the Stripe `Idempotency-Key` header. | `intents/refund.go:79-85`; `stripeapi.go:101-111` | APP | — |
| IDEM-7 | New provider-write call sites **fail CI** — an allow-list names every file permitted to invoke a money mover, recurring mutation, vault delete, or Solana submit. | `internal/intents/enforcement_guard_test.go` | **T** (AST; still not S — see §10 GAP-11) | `go test ./internal/intents -run TestProviderWrite`. or#865 widened the walk from `internal/`+`pkg/` to the **whole module**, so `cmd/`, `embed/`, `config/` and the repo root are now covered (zero violations existed there, so nothing was suppressed); proven by planting a `DeleteCustomerVault` call in `cmd/`. `internal/integrations/nmi` is scanned with `probe.go:voidProbe` allow-listed, so the formerly-dead `.Void(` entry now polices a real path. `_test.go` files stay out on purpose: live-sandbox suites call writes by design |
| IDEM-8 | All Stripe HTTP goes through one transport that blocks writes under `readonly` before bytes reach the network, and pins `Stripe-Version` from one const. | `stripeapi.go:23-28,56-67,85` | **APP** | `go test ./internal/integrations/stripeapi`. or#865 replaced the old audit (`grep "stripe.com" \| grep -v stripeapi`, a permanent false positive — with no `stripe-go` dep every compliant call site contains the URL, so it returned 20+ hits on a clean tree) with two AST rules whose expected value is zero: a file naming `api.stripe.com` must import the choke point, and must not pair it with `http.DefaultClient` / `&http.Client{}` / `http.Get`/`Post`/`PostForm`. Both proven to fail on a planted call site 18. Residual closed: `stripeapi.Client(nil, …)` returned a WRITE-capable client while an empty config already failed closed; nil now fails closed too |
| IDEM-9 | The operating-mode gate is a total function — an unknown origin parks rather than guessing, and an unknown *mode* parks too. | `chk_rail_intents_origin`; `intents/gate.go` | **DB** (origin) + **T** (nil mode) | the app-side `default:` arm remains **dead** — the CHECK admits only user/admin/system and the Go `Origin` type declares exactly those three — and is counted as a total-function backstop, not as enforcement. or#865 closed the other half: `GateExecution(nil, …)` used to return "not blocked", disabling `readonly`, `limited` and the origin check at once; it now parks with a reason. `go test ./internal/intents -run TestGateExecution` |
| IDEM-10 | Mode/kill-switch blocking is recorded as a reason on the intent, never raised as an error, so the queue drains when the blocker lifts. | `intents.go:16-20`; CHECK `:2360` | APP + DB | — |
| IDEM-11 | Webhook dedup identity is `PRIMARY KEY (merchant_id, op, event_id)`, falling back to a body hash when the rail supplies no id. Dedup happens **before** effect. | `webhook_events_pkey`; `webhookutil.go:170-178` | **DB** | — |

## 6. Identity and uniqueness

| # | Invariant | Enforced at | Str |
|---|---|---|---|
| ID-1 | Entitlement coverage is the union of independent source intervals; revoking one source preserves the others. | Source-scoped materialization and revocation; existence-based access reads | APP |
| ID-2 | At most one non-deleted entitlement projection per `(merchant, grant, entitlement)`; independent indefinite sources may overlap. | `uq_entitlements_grant_feature` | **DB** |
| ID-3 | One live subscription per `(merchant, customer, product)`, and one per `(customer, tier_group)`, with `tier_group` denormalized by a BEFORE trigger. | `:1006,:1008`; trigger `:175-184` | **DB** |
| ID-4 | One customer per `(merchant, subject)`; one rail-customer per `(merchant, customer, rail)`. | `:691,:2324,:2326` | **DB** |
| ID-5 | Price financial substance is unique per product. | `:744` | **DB** |
| ID-6 | At most one non-archived price per `(merchant, key)`; the key is trigger-derived and raises if the product is missing. | `:762`; fn `:203-233` | **DB** |
| ID-7 | Natural-key identity is a deterministic uuidv5 over a length-prefixed injective encoding. The namespace must never change. | `internal/shared/uuidutil/uuid.go:18-46` | APP |
| ID-8 | Grant termination happens once; `event='grant' ⟺ supersedes_id IS NULL`. | `:1355,:1306` | **DB** |
| ID-9 | Invoice period, invoice-item source, usage-event, and finding identities are unique per merchant. | `:1552,:1598,:2915,:2617` | **DB** |
| ID-10 | Merchant slug unique; `api_host` unique among live merchants. | `:272,:276` | **DB** |
| ID-11 | **Every UNIQUE index on a merchant-owned table is scoped by `merchant_id`, except reviewed identities.** Cross-merchant uniqueness can reveal another merchant's values through conflicts. | `internal/migrate/postgres/unique_scope_exemptions.go` records the reviewed exceptions. The former migration-text and live `pg_indexes` guards were removed with the legacy suite; current index definitions need independent review. | **DB** for existing indexes; automated scope audit is a coverage gap |

## 7. Fail-closed posture

These guards must fail closed. A guard that cannot fail is worse than none, because it
reads as protection.

| # | Guard | Enforced at |
|---|---|---|
| FC-1 | Missing merchant context is rejected by tenant services; zero merchant parameters do not select all tenants. | `merchant.Require`; scoped SQL |
| FC-2 | RLS is absent in isolation qualification, so policies cannot hide missing predicates. | owner/normal-login adversarial tests |
| FC-3 | Managed DB secret storage requires `ENCRYPTION_MASTER_KEY` in both sandbox and live posture. | `merchantsecrets/store.go` |
| FC-4 | Snapshot credentials remain in memory and cannot be mutated through ordinary Client operations. | `merchants/secrets_manifest.go` |
| FC-5 | `secret_backend=vault` without a working Vault or KV read capability refuses. `vault.enabled` (which may be Transit signing alone) never selects the KV store. | `merchantsecrets/store.go`; `config.validateSecretBackend` |
| FC-6 | Credential custody, write capability and external configuration routes are independent. Startup preserves existing metadata and refuses managed provider declarations that would bypass explicit publication. | `bootstrap.ProvisionMerchant`; `embed.HTTPConfig`; `merchants/credential_publication.go` |
| FC-6b | Catalogs always use database state. `AllowCatalogUpdates` defaults false, omits ordinary mutation routes and denies ordinary Client writes independently of provider credential custody. Trusted local operator application uses the same engine with internal authority. | `catalogpolicy.Check`; `catalogPolicyRouter`; `manifestModeWriteGuardMW` |
| FC-5b | Every retired config family (`store`, `merchant`, `cors_origins`, `db.require_rls`, `auth.issuers`/`auth.expected_audience`, `rails`, `auth.control_plane`, `private_port`) REFUSES boot with the rename, rather than warning and booting with the operator believing it is active (or#893). | `config.Load`; `hostauth/config/retired_config_families_test.go` |
| FC-7 | Webhook signature: a missing secret or missing header is an error. There is **no** unsigned path. HMAC compares with `hmac.Equal`. | `webhookutil.go:98-110`; `sigverify.go:44-71` |
| FC-8 | Replay tolerance is 5 minutes on public ingestion; the queued re-verify path narrows to HMAC-only, deliberately and in one leaf package. | `webhookutil.go:205-224` |
| FC-9 | A webhook whose merchant cannot be resolved is a 404, never a guess. | `webhook.go:78-90` |
| FC-10 | ~~An unknown intent origin parks.~~ **Dead branch** — no writer can persist a fourth origin (IDEM-9). Kept as a total-function backstop, not counted as a guard. Its sibling, the nil-`ModeView` default, WAS live-shaped and is fixed (or#865): an unknown mode now parks. | `intents/gate.go` |
| FC-11 | A Stripe write under `readonly` errors at the transport. | `stripeapi.go:56-60` |
| FC-12 | Missing merchant on context errors; there is no default merchant. | `pkg/merchant/merchant.go:50-54` |
| FC-13 | A captcha that does not verify → invalid, not pass. | `ratelimit_neutral.go` (the `err != nil` and `!result.Success` legs — both live and tested). or#865 **deleted** the `Verifier == nil` leg: it could not fire in any configuration (call site gated behind `cfg.IsEnabled()`, and `NewVerifier` returns nil only when that same flag is off). The coupling it silently rested on is now asserted where it CAN fail — `middleware/captcha_wiring_test.go`, proven to fail when `NewVerifier` gains a second nil return |
| FC-14 | Admission reservations and hard spend windows use SQL under the payer lock. Redis loss cannot erase monetary holds or reset the window. SQL errors never allow a request. | `spendgate/gate.go`; `money.WithLockedAdmissionCapacity`; `TestAdmissionClientRecoveryAndCaptureReceipt` |
| FC-15 | A delegated invoker requires an explicit spend grant, including zero-estimate requests. Zero estimates also enforce the prospective-rate policy. | `admission/admitter.go`; `TestZeroEstimateStillEnforcesProspectiveRateAndDelegation` |

| FC-16 | **Every background worker that touches a merchant-owned table runs inside `RunInMerchantConn`/`MerchantTx`.** A worker that does not is not "cross-merchant" — it is blind, and every guard downstream of its reads becomes vacuous. | unwritten; violated by `river/jobs_provider_intents.go:49` (or#862). This one rule is what `rate_ceiling.go`, `alerting/store.go`, `reprice_repo.go` and `intents/breaker.go` each violated in a different disguise. Mechanically checkable with an AST rule over `internal/river/**` — §10 GAP-17 |

**Deliberately open — access preservation.** These fail *open* on purpose: our malfunction
must never cost a customer access. Do not "fix" them without changing the doctrine.

| # | Guard | Enforced at |
|---|---|---|
| FO-1 | Rate limiting degrades from Redis to an in-memory limiter (degraded, not unlimited). | `ratelimit_neutral.go:209-218` |
| FO-2 | Entitlement and access checks fail open: `unverified`-status subscriptions keep standing access; a broken justification chain must be *proven* before retraction. | `reconcile/store.go:477`; `converge/converge_passes.go:304`; `grants/grants.go:625` |

## 8. No silent fabrication

Record provider data verbatim; derive downstream deterministically. A missing required
value must error or warn — never be replaced by an invented one. Declared **config**
defaults are fine; invented **data** defaults are not.

| # | Invariant | Enforced at |
|---|---|---|
| FAB-1 | Rail decline codes are stored verbatim; the normalized category is derived; an unmapped code reads `"unknown"`. | `payments.failure_code` / `failure_reason` (`0001_schema.up.sql`); `payments/failure_reason.go:10` |
| FAB-2 | A product declaring no entitlements grants **none**. (A previous version fabricated `"premium"`.) | `subscriptions/lifecycle_service.go:411-418,1094-1099` |
| FAB-3 | Absent provider dates and tokens return `ok=false`, never a fabricated instant. | `ccbill/subscription_management.go:140,362`; `reconcile/unknown_probe.go:100,201` |
| FAB-4 | A Solana subscribe/cancel is not classified as a sale — that would fabricate payment. | `reconcile/solana.go:27` |
| FAB-5 | Missing PSP posture surfaces as posture, not as a fabricated empty. | `reconcile/merchant_wiring.go:183` |
| FAB-6 | A parse failure must not silently become a zero amount. | Catalog drift's NMI plan amounts were the last `return 0` on parse failure (a zero amount raises drift against every local price); the shared drift pass uses the exact parser and SKIPS the plan with a warning. `internal/modules/catalog/drift.go` |

## 9. Destructive and irreversible actions

Provider-side deletes and cancels cannot be undone by us. An NMI vault delete destroys the
stored card; the customer must re-enter it.

| # | Invariant | Status |
|---|---|---|
| DES-1 | **Cancellation is a last resort.** Terminal cancel and provider-side delete require *certainty* — a non-retryable decline, or genuinely exhausted dunning. Never a date comparison, never an absence. | **ENFORCED** (or#821) — `gateCancelCertainty` is one chokepoint in `Decide`; every plane inherits it. `RemoteGone` (provider's own word) is certainty; our inference is not. |
| DES-2 | Stale or unreadable provider data parks as `unverified` and never costs entitlements. | FO-2 upholds the entitlement half |
| DES-3 | **NMI rebill is infinite retry.** Missed periods are forgiven; the customer is paid up from the latest success. A lapsed expiration is not a dead schedule — only provider roster state classifies. | **ENFORCED** (or#821) — a stale `next_billing_date` now parks as `unverified`; the `NonRetryableDecline` leg is wired to real rail codes and unrecognised codes are retryable by construction. |
| DES-4 | Absence is not evidence of death. A subscription missing from a pull, an empty response, a 404, or a timeout must not retract standing. The confirmed-absence gate must hold at **every** retraction site. | **MOSTLY ENFORCED** (or#842) — confirmed-absence gate extended to every retraction site; an empty roster is no longer stamped exhaustive; `diff.go`'s hardcoded `forceExhaustive` removed. Residual: #839/#840 in the collection engine. |
| DES-5 | No unattended irreversible provider call without provider-confirmed certainty, a blast-radius cap, and an operator kill switch that works without a deploy. | **SPLIT.** The switch itself is **ENFORCED** and genuinely fail-closed — `destructive_action_switch` is RLS-exempt on purpose, the read is a `LEFT JOIN` off `(SELECT 1)` so `ErrNoRows` is impossible, `COALESCE(s.enabled,false)` reads an unreadable switch as OFF, and OFF means deny. The `jobs_dunning`/or#856 residuals are closed. But on the **background intent-runner plane the gate is never consulted**: `river/jobs_provider_intents.go:49-66` runs `RunExecuteOnce` on a bare job context, so `ClaimDue` claims zero intents and neither the kill switch nor the #679 volume breaker executes at all — or#862. |

## 9a. Recovery classes — what a rollback may restore

A rollback restores state that was wrongly **destroyed**. It can never retract value that
was wrongly **granted**: retraction is a revoke event, a compensating transfer, and an
explicitly operator-authorised refund. Conflating the two is how a recovery feature becomes
a way to silently take money and access back.

Every merchant-owned table falls in exactly one class. This is the register `undo-run`
obeys (`internal/reconcile/never_rollbackable.go`).

| Class | Tables | Rule |
|---|---|---|
| **A — append-only spine** | `ledger_transfers`, `ledger_accounts`, `grants`, `subscription_status_transitions`, `rail_mutation_logs`, `webhook_events`, `reconciliation_findings` (catalog drift included), `maintenance_runs` headers (column-restricted UPDATE, no DELETE), `price_key_movements` | **NEVER rolled back, at any scope, by any tier.** |
| **P — provider mirror** | `payments`, `subscriptions`, `payment_methods`, `rail_customer_accounts`, `solana_subscriptions`, `checkout_sessions`, refund/dispute mirrors, `rail_refresh_watermarks` | Freely restored; `reconcile pull` is the repair. Six of the eight PSP-tagged tables live here, so **per-PSP scope is native**. |
| **D — derived** | `entitlements`, product access, credit-lot spendability | **Never restored, always recomputed.** `Converge` rebuilds each effect from the append-only grant log. A restored effect can silently disagree with its grant; a re-derived one cannot. |
| **C — definitions and policy** | `products`, `prices`, rate cards, meters, `psps`, `merchant_webhooks`, spend limits, `merchant_destructive_policy`, … | Merchant-scoped only, **never PSP-scoped**. No external authority — convergence pushes this outward, so corruption here propagates instead of self-correcting. |
| **X — outside the transaction** | the `river_*` set (host-chosen schema, `public` by default), Redis holds and leases, `profiles.*` (AuthKit), Vault secrets, `worker_state` | Not rollbackable with the schema. Jobs are **quiesced** for the duration, never rewound; Redis holds self-heal. |

| # | Invariant | Status |
|---|---|---|
| REC-1 | **Class A is never rolled back.** No undo path may write a Class A table. | **ENFORCED** — `TestNeverRollbackableRegisterIsEnforcedOnEveryUndoPath` walks the undo implementations, resolves the sqlc queries they call, and fails on any write to a registered table. Backed by role privilege where it can be: `ledger_transfers`/`ledger_accounts`/`grants`/`subscription_status_transitions` are `GRANT SELECT,INSERT` only, `rail_mutation_logs` has no `UPDATE`, and `maintenance_runs` has no `DELETE` and only status/finish/reversal/note column `UPDATE`. |
| REC-2 | **Superseding an unfired intent is a forward transition, not a rollback.** `rail_intents` moves `pending`/`failed_retryable` → `superseded`, never deleted, never rewritten once executed. This is how an undo neutralises a queued provider write. | **ENFORCED** — `TestSupersedeIsTheOnlyRailIntentWriteOnAnUndoPath` pins the status predicate and refuses a DELETE on any undo path. |
| REC-3 | **Class D is invalidated and re-derived, never restored.** | **ENFORCED** — the reverse soft-deletes the windows the run closed and stamps them with it; `Converge` rebuilds them. Entitlement before-images are captured as evidence and deliberately left `restored_at IS NULL`. |
| REC-4 | **A destructive operation with no way to record its undo does not run.** | **ENFORCED** — an enforce pass planning state transitions with no `DestructiveRunRecorder` errors before writing, and a before-image capture failure skips that transition. |
| REC-5 | **An undo plans before it applies, and never resurrects a row another run removed.** Dry run is the default; `--apply` needs a typed row count matching the plan; a row a later prune tombstoned belongs to that run's reverse and is skipped and reported. | Historical implementation claim; `undo_run_integration_test.go` was removed. No focused undo/non-resurrection scenario currently replaces it. |
| REC-6 | **A reversal is scoped by the run, not by a flag.** The ledger row carries the merchant and (when account-bound) the PSP; every restore predicate is keyed on the run id inside a merchant-scoped connection. | **ENFORCED** — the per-PSP and cross-merchant cases are proven against the real enforce path. |
| REC-7 | **A kind whose damage no local undo reaches is refused by name**, with what to reach for instead — never half-reversed and marked reversed. | **ENFORCED** — `merchant_delete` is registered unrecoverable; unconverted kinds refuse. |
| REC-8 | **A rollback is not a complete operation; `rollback → pull → converge` is.** The post-rollback book is definitionally incomplete, so the proven source-domain flags are reset and first-enforce is disarmed — the next pull runs advisory until an operator re-arms it. | Historical implementation claim; `converge_rollback_integration_test.go` was removed. No focused rollback/pull/converge scenario currently replaces it. |

**The hole, CLOSED (or#893)**: `psp_id` used to be nullable on every PSP-tagged table, so a
PSP-scoped predicate silently skipped unattributed rows and the undo could only report that
blind spot as a count. Migration 0063 made provenance total — `NOT NULL` on subscriptions,
payment_methods, checkout_sessions, rail_intents, rail_mutation_logs and rail_customer_accounts,
and a `psp_id IS NOT NULL OR rail IN ('manual','admin')` CHECK on payments and invoice_payments,
the two tables that also record off-rail money. The count is now an INVARIANT the undo asserts
and refuses on, not a report.

## 10. Historical gap register

These entries retain earlier audit evidence and dates. RLS-specific failure modes
and role requirements were superseded by #1021; use the current tracker for
remaining release gates. Other historical rows are not re-qualified by that change.

Every one of these is a rule the codebase intends. Rows are retained after closure as a
log, so read the **Tracked** column before trusting the Gap column: only rows without a
**FIXED** / **CLOSED** / **PARTIALLY CLOSED** marker are still unenforced. A row marked
**STRENGTHENED** or **PARTIALLY CLOSED** names the residual explicitly — that residual is the
live gap, not the original text. Tracker issues in `~/open-rails-tracker/openrails/`.

Still open as of 2026-07-29: GAP-15 (untested live Solana amount formatter) and GAP-17
(RLS-blind workers). GAP-18 (fail-open `GateExecution` default) is CLOSED — or#865. GAP-11, GAP-12, GAP-13 and
GAP-14 are open only in their named residuals; GAP-16 is closed.

| # | Gap | Evidence | Tracked |
|---|---|---|---|
| GAP-1 | **Integers-only for money is convention only.** `MajorUnits` is a `float64` money type reaching the live NMI wire; Solana base units round-trip through float and overcharge 1.19% of whole-cent amounts; FX multiplies an amount by a float; a refund amount arrives as a JSON float. | `moneyutil.go:27`; `nmi/subscriptions.go:113`; `solana/support.go:63,122`; `fx/provider.go:79`; `admin_findings_actions.go:355-361` | **CLOSED** #818 + the guard. `MajorUnits` deleted, Solana integer rescale, integer tolerances — and now `TestNoFloatsInMoneyPackages` (AST, per-declaration allow-list) fails CI on a new float. Turning it on found five MORE live violations #818 had not reached: `fx.ConvertAmount` float-multiplied an amount then `math.Ceil`'d it; `intents.nmiAmountToCents` `ParseFloat`'d a refund amount that is compared for EQUALITY to decide whether a provider refund is ours; catalog drift `ParseFloat`'d NMI plan amounts and returned 0 on failure; the Solana token base-unit amount round-tripped through JSONB as a JSON number; and `webhooks.Stringish` decoded bare JSON numbers through `float64`, mangling any NMI id past 2^53. All five fixed. Residual: GAP-13's package holes. |
| GAP-2 | **Currency is silently defaulted in two paths** — both substitute a lowercase, unregistered `"usd"` when the provider gave none, one while minting a `payments` row. Violates CUR-9 and FAB-6. | `solana/support.go:116-119`; `modules/reconcile/reconcile.go:570-573` | **FIXED** #830 — three fallback sites removed (one more than filed); a missing provider currency now errors or parks. |
| GAP-3 | **Terminal cancel + NMI vault delete fire on a stale date.** Past-due is inferred from `next_billing_date < today` with no decline evidence; beyond 14 days that becomes an irreversible provider delete. The decider's own comment admits the remote may still be retrying. | `reconcile/nmi.go:181-186`; `decider.go:315-319`; `lifecycle_service.go:1533-1540` | **FIXED** #821/#834/#835/#837/#841/#842 — measured: empty roster went 40 cancellations → 0. |
| GAP-4 | **RLS coverage was not checked beyond the baseline.** `payment_settlement_events` (now `host_outbox`) shipped carrying `merchant_id` and full CRUD grants with no RLS, because the guard test read a hardcoded table list. | `host_outbox`; `merchant_aware_schema_test.go` | **FIXED** SEC-16 — guard now derives its table list from ALL migrations; 5 tables had partial-only merchant_id indexes (or#846). |
| GAP-5 | **Duplicate payments and subscriptions are representable.** The uniques are partial and disjoint on `rail_merchant_account_id IS NULL`, so the same `(merchant, rail, transaction_id)` can exist twice — once legacy, once account-attributed. | `uq_payments_merchant_psp_transaction` vs `uq_payments_merchant_offrail_transaction` | **FIXED** #831, then CORRECTED by #17 — 0012 over-tightened to (merchant, rail, id), forbidding two PSPs issuing the same provider id; 0017 restores per-PSP scope via COALESCE. |
| GAP-6 | **Currency codes in the DB are unvalidated.** Deliberately app-only across 16 columns; the ledger trigger checks equality, not validity. Nothing stops an import or GAP-2 persisting an unregistered code. | `money/currency.go:11-13` | **FIXED** or#832 — migration 0020 adds a `*_currency_shape` CHECK to all 16 currency columns, applied by iterating `information_schema` (not a hand-written list — the lesson of GAP-4) and guarded by `TestCurrencyColumnsCarryShapeCheck`. It pins CASE and rough shape, not membership; membership stays in the Go registry. Measured cause of the drift: `pkg/catalog`'s loader lower-cased on the way in. Running the CHECK against the integration suite then found FOUR live write paths that were storing the rail's own casing — the CCBill webhook, the Stripe converge backfill, `Service.CreatePrice`, and every rail reaching the payment repo — all now canonicalised at the boundary. **Correction to the original claim:** "the write boundary is not one place" is true for the schema as a whole but FALSE for the two tables that carry most currency rows; payments and prices each have exactly one INSERT chokepoint, and those now normalise, so the CHECK is a backstop there rather than the only defence. See CUR-6. |
| GAP-7 | **`transfer_type` has no CHECK.** `account_type` does. A typo in a new transfer type silently bypasses the lot-once uniqueness index. | `ledger_transfers_type_check` vs `ledger_accounts_type_check` | **FIXED** #832 — CHECK + typed Go constants pinned to each other; the live vocabulary was 7 values, not 5, and `credit_reinstate` existed only as a bare literal. |
| GAP-8 | **Ledger conservation is never computed.** `sum(balances) = 0` per `(merchant, currency)` is structural, but nothing checks it, and the account counters are a maintained projection that can drift from `ledger_transfers` if the trigger is ever bypassed (superuser, restore, `COPY`). | trigger `:164-169`; no diagnostic found | **FIXED** #833 — `CheckConservation` + `CheckCounterDrift` + `openrails ledger-audit`; a trigger-bypass drift test proves it catches real drift. |
| GAP-9 | **org ↔ merchant 1:1 is not enforced.** The decision was a unique `owner_org_id`; the shipped column is `permission_group_id` with a **non-unique** index, so two merchants can claim one group and merchant-from-group resolution is ambiguous. | `merchants.permission_group_id` | **FIXED** #843 — `uq_merchants_permission_group_id`, partial on live merchants. |
| GAP-10 | **Three unique indexes lack `merchant_id`** — `checkout_sessions` on `(rail, reference)` and `(rail, transaction_id)`, plus the open-drift-finding unique (now `uq_reconciliation_findings_identity`). Under RLS the conflicting row is invisible, so one merchant blocks another's insert: a cross-tenant existence oracle and a claim-squat. | `uq_checkout_sessions_merchant_psp_reference`, `uq_checkout_sessions_merchant_psp_transaction`, `uq_reconciliation_findings_identity` | **FIXED** SEC-24 — migration 0020 scopes all three by `merchant_id` (and by PSP, via 0017's COALESCE-the-nullable technique so one TOTAL index keeps per-PSP scope). Made permanent by `TestUniqueIndexesAreMerchantScoped`, which derives the unique-index inventory from every migration and fails on a new cross-merchant unique; the five legitimate exceptions are exempted BY NAME with reasons. |
| GAP-11 | **The provider-write guard is textual.** A wrapper, method value, or renamed import reaches the same wire call invisibly, and an allow-listed file is trusted wholesale. | `enforcement_guard_test.go:15-24` | **STRENGTHENED, not closed.** Now AST-based with the allow-list keyed by `file:FUNCTION`, so method values, interface dispatch, renamed imports and a second call inside an allow-listed file are all visible. A second guard, `TestProviderWriteSurfaceIsClassified`, inventories the client's exported surface and fails until every method is classified read or write — which closed the real hole: six writes (`AddRecurringPlan`, `EditRecurringPlan`, `CreateCustomerVault`, `UpdateCustomerVault`, `UpdateRecurringSubscription`, `Void`) were never in the old token list, so their call sites were unguarded. **Residual: strength is T, not S.** Go has no friend visibility; making bypass a COMPILE error needs the write client moved under `internal/intents/internal/…`, which the legitimate reactive callers make a large refactor. Not attempted. |
| GAP-13 | **The float guard's package list has holes, and two live violations sit in them.** `TestNoFloatsInMoneyPackages` is a real AST check, but its `guarded` list omits `internal/http/handlers/`, `internal/reconcile/` (it guards the *different* `internal/modules/reconcile/`), `internal/integrations/pyth/` and `internal/river/`. A refund amount is read as a JSON `float64` and truncated (`admin_findings_actions.go:355-361` — the very site GAP-1 marked FIXED), and an on-chain token amount round-trips through `float64` (`reconcile/local.go:425-431`). | those two files | **CLOSED** or#863 — the list gains `internal/reconcile/`, `internal/http/handlers/`, `internal/river/`, `internal/integrations/{pyth,solana,basistheory}/`, `internal/modules/payments/`, `internal/controlplane/`; both violations fixed (`override_params` binds RAW and decodes with `UseNumber`, `paramAmountMicros` parses an exact `json.Number`/decimal string and REFUSES a float; the Solana `case float64` is deleted so an unreadable amount parks). The guard was verified failing on each violation before its fix. Residual: packages outside the list are still convention-only. |
| GAP-14 | **The "single internal→rail converter" is bypassed.** `NativeToRailMinor` had 2 non-test callers; 14 sites called `moneyutil.MicrosToCents*` directly, hardcoding ÷10 000. | `checkout/*`, `subscriptions/*`, `webhooks/*`, `internal/service/*`, `handlers/admin_payments.go` | **CLOSED** or#863. **Correction to the original claim:** those sites were NOT wrong by 10⁴ for JPY. Every registered currency shares a native shift of 4 (USD/EUR 6−2, JPY 4−0), so ÷10 000 was numerically right for all three. The real defect is that a converter which cannot see the currency cannot REFUSE one — it converted amounts whose currency nobody had established, which is what made GAP-16's pre-charge guesses invisible — and it was a silent landmine for the first currency registered with a different shift. Fix: the registry moved to the leaf `internal/shared/moneyutil` (it had to — `internal/modules/money` imports `internal/modules/subscriptions`, so the NMI plan pushes could never have reached it), all 14 sites routed, `MicrosToCents*` DELETED, and `TestRegisteredCurrenciesShareNativeShift` turns the shift coincidence into a tripwire. Residual: the ~31 inbound `CentsToMicros` sites stay currency-blind under that tripwire, and NMI's `/100` cents↔decimal-string edges stay — they are the rail's WIRE FORMAT (`amount` is documented `x.xx`), not an internal→rail conversion. Named residual: that wire format is 2-decimal-only, so putting a ZERO-decimal currency on NMI would need `WireAmount`/`centsJSONAmount` made scale-aware. No merchant runs one today. |
| GAP-15 | **Solana's live Pay-URL amount formatter has no test.** `solana/pay.go:607 formatTokenAmount` reaches the wire; the *tested* formatter is a near-duplicate, `solana/support.go:166 FormatBaseUnits`. Two divergent formatters, only the untested one live. CCBill's unqualified outbound refund adapter was removed under #696; automatic CCBill refunds are refused. | `solana/pay.go:308-311,607` | or#863 |
| GAP-16 | **CUR-9 is violated by its own cited enforcement site**, which borrows the subscription's currency for a decline that carried none — and says so in a comment. Three further paths substitute `DefaultCurrency`, two of them immediately before charging a card. | `reconcile/unknown_orchestration.go:281-285`; `money/nmi_collection.go:51-53`; `money/custodian_proxy_collection.go:43-45`; `handlers/admin_users.go:112-114` | **FIXED** or#864. The two pre-charge substitutions are gone and the gate is hoisted to `ScopedCharger` — the one dispatch point for every off-session collection — where a REGISTERED currency is required; `stripe_collection.go`, which never defaulted but also never checked, is covered by the same gate. The admin profile no longer defaults a query-param currency (top-level `trust_level` only when the caller names one; each credit-balance row now carries its own). The decline borrow stays but records `currency_provenance=inherited_from_subscription_price` and logs it — the difference between an inference and a fabrication is whether a reader can tell. `money.DefaultCurrency` survives only as a declared CONFIG default: the FX base/accounting unit, the `currency != DefaultCurrency` FX-needed test, and NMI's v5 vault billing-currency field (a record field, no amount attached). |
| GAP-17 | **The RLS-blind-worker class is not fully closed.** Fixed 2026-07-28: the #732 rate ceiling and both armed-merchant scans now go through migration 0021's definer readers, and the settlement feed runs under `MerchantTx`. Still inert: the #816 re-driver (`reprice_repo.go:189`), fleet analytics/timeseries, and — worst — the whole background intent-executor plane, which claims zero intents so the #836 kill switch and #679 volume breaker never execute (or#862). | `db_pgx.go:66-86` | or#861, or#862 |
| ~~GAP-18~~ | **CLOSED (or#865).** `GateExecution(nil, …)` now parks with a reason instead of returning "not blocked". It was more live than the filing assumed — a `Runner` built without `Config` executed every intent, which `runner_test.go` was relying on. | `intents/gate.go` | closed |
| GAP-12 | **`Micros` is opt-in.** DB columns are bare `bigint` and most Go structs use `int64`; the unit is carried by column comments. The type only bites where someone chose it. | `moneyutil.go:18` vs `ledger/ledger.go:118` | **PARTIALLY CLOSED** #818 — scoped to the highest-value boundary: the unit CONVERTERS. `CentsToMicros` / `MicrosToCentsCeil` / `MicrosToCentsExact` are the only places a value changes unit and were `int64 -> int64`; they are typed now, as are `ParseDecimalToCents` and the `Format*` helpers, and the type propagated outward on its own into the NMI plan pusher, the CCBill amount-tolerance comparison, the chargeback matcher and the admin refund path. **Deliberately left:** DB columns stay bare `bigint`; most struct fields stay `int64`; `moneyutil.NativeToRailMinor` keeps an `int64` input because its argument is internal units at the CURRENCY's registered scale (JPY is 10⁴), which is not always micros — typing it `Micros` would be a lie. GAP-14 closed the other half: `MicrosToCentsCeil` / `MicrosToCentsExact` are deleted and the surviving converters are currency-aware. |

### Audit bundle

Run these from an explicitly authorized operator connection. These are intentional
fleet-wide diagnostic scans; ordinary tenant requests must use the scoped APIs.
With RLS absent, the diagnostics no longer silently return empty results because
of a missing session merchant. The former `internal/invariantaudit` package
was removed; these SQL diagnostics are operator checks, not current CI evidence.

Read-only checks that should pass at any time:

```sql
-- TEN-1: any remaining RLS flags (expect no rows)
SELECT relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE n.nspname='billing' AND c.relkind='r' AND (c.relrowsecurity OR c.relforcerowsecurity);

-- ID-11 (was GAP-10): unique indexes not scoped by merchant.
-- Expect ONLY surrogate-id *_pkey rows plus the named exceptions in
-- internal/migrate/postgres/unique_scope_exemptions.go.
SELECT indexdef FROM pg_indexes WHERE schemaname='billing'
   AND indexdef LIKE '%UNIQUE%' AND indexdef NOT LIKE '%merchant_id%';

-- CUR-5b: every currency column carries the shape CHECK (expect 16).
SELECT count(*) FROM pg_constraint
 WHERE contype='c' AND conname LIKE '%\_currency\_shape' ESCAPE '\'
   AND connamespace='billing'::regnamespace;

-- GAP-8: ledger conservation per (merchant, currency)
SELECT merchant_id, currency, sum(credits_posted - debits_posted) FROM billing.ledger_accounts
 GROUP BY 1,2 HAVING sum(credits_posted - debits_posted) <> 0;

-- GAP-2/GAP-6: lowercase currency codes. Now impossible to insert (CUR-5b), so
-- this is a post-migration audit of legacy rows rather than a live guard.
SELECT DISTINCT currency FROM billing.payments;

-- GAP-5: duplicate provider transactions
SELECT merchant_id, rail, transaction_id, count(*) FROM billing.payments
 GROUP BY 1,2,3 HAVING count(*) > 1;

-- GAP-9: merchants sharing a permission group
SELECT permission_group_id, count(*) FROM billing.merchants
 WHERE permission_group_id IS NOT NULL GROUP BY 1 HAVING count(*) > 1;
```

Historical audit commands (verify that the named tests still exist before
using them; a successful command matching no tests is not evidence):

```
go test ./internal/intents  -run TestProviderWrite                    # IDEM-7 (both halves: surface + call sites)
go test ./internal/shared/moneyutil -run TestNoFloatsInMoneyPackages  # MONEY-3
go test ./internal/merchants -run TestNoAdHocSecretPathConstruction   # secret-path builder
go test ./internal/merchants -run TestSecretNameRejectsTraversal      # SEC-24 item 6
go test ./internal/merchants -run TestEncryptedSecretIsSealedToItsRow  # SEC-24 item 1 (AAD binding)
go test ./internal/http/handlers -run TestStripeRelatedObjectURLIsAlwaysAPath         # SEC-24 item 5
go test ./internal/integrations/stripeapi                             # IDEM-8/FC-11 (choke point + readonly + version pin)
go test ./internal/http/middleware -run TestEnabledCaptchaAlwaysHasVerifier           # FC-13
go test ./internal/intents  -run TestGateExecution                    # IDEM-9 (origin + nil mode)
go test -tags greenfield ./ci/greenfield -run TestExactIntegerMoneyBoundaries # MONEY-7 (conversion cases only)
go test ./internal/integrations/nmi -run 'TestStalledGateway|TestPerRequestDeadline'  # NMI ctx + deadlines (or#866)
go test .                   -run 'TestRootPackageStaysLight|TestCorePackagesStayFrameworkNeutral'
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
