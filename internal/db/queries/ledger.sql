-- The books (ledger_accounts + ledger_transfers): the units book and the
-- general ledger. Balances are maintained counters on accounts; transfers are
-- append-only.

-- name: GetLedgerAccount :one
SELECT * FROM billing.ledger_accounts
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND account_type = sqlc.arg(account_type)::text
  AND currency = sqlc.arg(currency)::text
  AND customer_id IS NOT DISTINCT FROM sqlc.narg(customer_id)::uuid
  AND psp_id IS NOT DISTINCT FROM sqlc.narg(psp_id)::uuid
  AND bank_account_id IS NOT DISTINCT FROM sqlc.narg(bank_account_id)::uuid;

-- ListOpenGeneralLedgerAccounts: the merchant-wide general-ledger accounts
-- open.
-- name: ListOpenGeneralLedgerAccounts :many
SELECT account_type, currency, psp_id, bank_account_id
FROM billing.ledger_accounts
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND book = 'general' AND customer_id IS NULL;

-- ListSellableCurrencies: every currency the merchant's catalog prices in.
-- name: ListSellableCurrencies :many
SELECT currency::text AS currency FROM billing.prices WHERE merchant_id = sqlc.arg(merchant_id)::uuid
UNION
SELECT (price->>'currency')::text FROM billing.catalog_rate_cards
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND price->>'currency' IS NOT NULL;

-- ListGeneralLedgerPSPs: the merchant's PSPs, each with cash clearing.
-- name: ListGeneralLedgerPSPs :many
SELECT id FROM billing.psps WHERE merchant_id = sqlc.arg(merchant_id)::uuid ORDER BY id;

-- ListGeneralLedgerBankAccounts: the merchant's bank accounts, each with bank
-- cash in its currency.
-- name: ListGeneralLedgerBankAccounts :many
SELECT id, currency FROM billing.bank_accounts WHERE merchant_id = sqlc.arg(merchant_id)::uuid ORDER BY id;

-- ListPSPImportCurrencies: the currencies a PSP import's records name.
-- name: ListPSPImportCurrencies :many
SELECT DISTINCT (data->>'currency')::text AS currency FROM billing.psp_import_records
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND import_id = sqlc.arg(import_id)::uuid
  AND data->>'currency' IS NOT NULL;

-- name: InsertLedgerAccount :one
INSERT INTO billing.ledger_accounts (
    merchant_id, book, class, customer_id, psp_id, bank_account_id, account_type, currency,
    debits_must_not_exceed_credits, credits_must_not_exceed_debits
) VALUES (
    sqlc.arg(merchant_id)::uuid, sqlc.arg(book)::text, sqlc.arg(class)::text,
    sqlc.narg(customer_id)::uuid, sqlc.narg(psp_id)::uuid, sqlc.narg(bank_account_id)::uuid,
    sqlc.arg(account_type)::text, sqlc.arg(currency)::text,
    sqlc.arg(debits_must_not_exceed_credits)::boolean, sqlc.arg(credits_must_not_exceed_debits)::boolean
)
ON CONFLICT DO NOTHING
RETURNING *;

-- name: EnsureCustomerBalanceAccounts :exec
-- Customers' balance accounts, made in (customer, currency) order: each one
-- this creates is watched as InsertPrepaidBalanceWatch watches it.
WITH created AS (
    INSERT INTO billing.ledger_accounts (merchant_id, book, class, customer_id, account_type, currency,
        debits_must_not_exceed_credits, credits_must_not_exceed_debits)
    SELECT sqlc.arg(merchant_id)::uuid, sqlc.arg(book)::text, sqlc.arg(class)::text, a.customer_id, sqlc.arg(account_type)::text, a.currency,
        sqlc.arg(debits_must_not_exceed_credits)::boolean, sqlc.arg(credits_must_not_exceed_debits)::boolean
    FROM unnest(sqlc.arg(customer_ids)::uuid[], sqlc.arg(currencies)::text[]) AS a(customer_id, currency)
    ORDER BY a.customer_id, a.currency
    ON CONFLICT DO NOTHING
    RETURNING customer_id, currency
)
INSERT INTO billing.balance_watches (merchant_id, customer_id, currency, next_check_at, look_each_change)
SELECT sqlc.arg(merchant_id)::uuid, c.customer_id, c.currency, NULL::timestamptz, true
FROM created c
WHERE NOT EXISTS (
    SELECT 1 FROM billing.money_settings s
    WHERE s.merchant_id = sqlc.arg(merchant_id)::uuid AND s.customer_id = c.customer_id
      AND s.currency = c.currency AND s.billing_mode = 'arrears' AND s.collection_revoked_at IS NULL)
ORDER BY c.customer_id, c.currency
ON CONFLICT (merchant_id, customer_id, currency) DO NOTHING;

-- InsertLedgerTransfer is the units book's one durable write, effective when
-- it posts. ON CONFLICT DO NOTHING on
-- ledger_transfers_operation_once_key makes once-only a database fact: a
-- replay at the same coordinate inserts nothing and returns zero rows, whatever
-- the caller's lock order. Zero rows means already applied: ApplyIdempotent
-- reads the committed row.
-- name: InsertLedgerTransfer :one
INSERT INTO billing.ledger_transfers (
    merchant_id, book, debit_account_id, credit_account_id, amount, currency, transfer_type,
    allow_debit_negative_up_to, operation,
    source, source_id, credit_grant_id, customer_id, invoker_id, resource, invoice_id,
    effective_at
) VALUES (
    sqlc.arg(merchant_id)::uuid, 'units', sqlc.arg(debit_account_id)::uuid, sqlc.arg(credit_account_id)::uuid,
    sqlc.arg(amount)::bigint, sqlc.arg(currency)::text, sqlc.arg(transfer_type)::text,
    0, sqlc.arg(operation)::text,
    sqlc.arg(source)::text, sqlc.arg(source_id)::text, sqlc.narg(credit_grant_id)::uuid, sqlc.narg(customer_id)::uuid,
    sqlc.narg(invoker_id)::text, sqlc.narg(resource)::text, sqlc.narg(invoice_id)::uuid,
    now()
)
ON CONFLICT (merchant_id, customer_id, currency, transfer_type, operation, source, source_id, credit_grant_id, invoice_line_id, book)
DO NOTHING
RETURNING *;

-- InsertGeneralLedgerTransfer is the general ledger's one durable write, at
-- the instant its poster names. No floor relaxes a sign rule. Once-only as
-- InsertLedgerTransfer.
-- name: InsertGeneralLedgerTransfer :one
INSERT INTO billing.ledger_transfers (
    merchant_id, book, debit_account_id, credit_account_id, amount, currency, transfer_type,
    allow_debit_negative_up_to, operation, source, source_id, credit_grant_id, customer_id,
    invoker_id, resource, invoice_id, invoice_line_id, product_id, effective_at, revenue_schedule_id
) VALUES (
    sqlc.arg(merchant_id)::uuid, 'general', sqlc.arg(debit_account_id)::uuid, sqlc.arg(credit_account_id)::uuid,
    sqlc.arg(amount)::bigint, sqlc.arg(currency)::text, sqlc.arg(transfer_type)::text,
    0, sqlc.arg(operation)::text, sqlc.arg(source)::text, sqlc.arg(source_id)::text,
    sqlc.narg(credit_grant_id)::uuid, sqlc.narg(customer_id)::uuid,
    sqlc.narg(invoker_id)::text, sqlc.narg(resource)::text, sqlc.narg(invoice_id)::uuid,
    sqlc.narg(invoice_line_id)::uuid, sqlc.narg(product_id)::uuid,
    sqlc.arg(effective_at)::timestamptz, sqlc.narg(revenue_schedule_id)::uuid
)
ON CONFLICT (merchant_id, customer_id, currency, transfer_type, operation, source, source_id, credit_grant_id, invoice_line_id, book)
DO NOTHING
RETURNING *;

-- GetLedgerTransferAtCoordinate reads the row a conflicting insert lost to —
-- the full physical identity, lot, invoice line and book included, so a
-- multi-lot spend resolves the right leg.
-- name: GetLedgerTransferAtCoordinate :one
SELECT * FROM billing.ledger_transfers
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND book = sqlc.arg(book)::text
  AND customer_id IS NOT DISTINCT FROM sqlc.narg(customer_id)::uuid
  AND currency = sqlc.arg(currency)::text
  AND transfer_type = sqlc.arg(transfer_type)::text
  AND operation = sqlc.arg(operation)::text
  AND source = sqlc.arg(source)::text
  AND source_id = sqlc.arg(source_id)::text
  AND credit_grant_id IS NOT DISTINCT FROM sqlc.narg(credit_grant_id)::uuid
  AND invoice_line_id IS NOT DISTINCT FROM sqlc.narg(invoice_line_id)::uuid;

-- LedgerAccountBalance: net credit (credits - debits) from the maintained
-- account counters. A merchant-wide general-ledger account keeps none and
-- reads no row: its balance is its type total, kept at commit.
-- name: LedgerAccountBalance :one
SELECT (credits_posted - debits_posted)::bigint AS balance
FROM billing.ledger_accounts
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND id = sqlc.arg(account_id)::uuid
  AND (book = 'units' OR customer_id IS NOT NULL);

-- name: ListLedgerTransfersByCustomer :many
-- A customer's units-book movements in one currency, newest first.
SELECT * FROM billing.ledger_transfers
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND customer_id = sqlc.arg(customer_id)::uuid
  AND currency = sqlc.arg(currency)::text
  AND book = 'units'
  AND (sqlc.narg(after_at)::timestamptz IS NULL
   OR (created_at, id) < (sqlc.narg(after_at)::timestamptz, sqlc.narg(after_id)::uuid))
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(row_limit)::int;

-- name: ListLedgerTransfersByIDs :many
-- A customer's named units-book movements, newest first.
SELECT * FROM billing.ledger_transfers
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = ANY(sqlc.arg(ids)::uuid[])
  AND customer_id = sqlc.arg(customer_id)::uuid AND book = 'units'
ORDER BY created_at DESC, id DESC;

-- GetLedgerTransferByCoords: the latest units-book transfer at the full
-- operation coordinate. `operation` keeps a capture and a wasted-spend usage
-- charge that share one (source, source_id) apart.
-- name: GetLedgerTransferByCoords :one
SELECT * FROM billing.ledger_transfers
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND book = 'units'
  AND customer_id = sqlc.arg(customer_id)::uuid
  AND currency = sqlc.arg(currency)::text
  AND transfer_type = sqlc.arg(transfer_type)::text
  AND operation = sqlc.arg(operation)::text
  AND source = sqlc.arg(source)::text
  AND source_id = sqlc.arg(source_id)::text
ORDER BY created_at DESC
LIMIT 1;

-- GetLedgerSpendByCoords: the first posted spend movement for one money
-- operation at its idempotency coordinate.
-- name: GetLedgerSpendByCoords :one
SELECT * FROM billing.ledger_transfers
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND customer_id = sqlc.arg(customer_id)::uuid
  AND currency = sqlc.arg(currency)::text
  AND transfer_type IN ('credit_spend', 'owed_accrual')
  AND operation = sqlc.arg(operation)::text
  AND source = sqlc.arg(source)::text
  AND source_id = sqlc.arg(source_id)::text
ORDER BY created_at ASC
LIMIT 1;

-- ListLedgerConservationBreaches sums every balance per book and currency:
-- an account's counters, or the type total of a merchant-wide general-ledger
-- account, which keeps none.
-- name: ListLedgerConservationBreaches :many
SELECT sqlc.arg(merchant_id)::uuid AS merchant_id,
       book::text AS book,
       currency::text AS currency,
       SUM(net)::bigint AS net,
       COUNT(*)::bigint AS accounts
FROM (
    SELECT book, currency, credits_posted - debits_posted AS net
    FROM billing.ledger_accounts
    WHERE merchant_id = sqlc.arg(merchant_id)::uuid
      AND (book = 'units' OR customer_id IS NOT NULL)
    UNION ALL
    SELECT a.book, a.currency, COALESCE(t.credits_posted - t.debits_posted, 0)
    FROM billing.ledger_accounts a
    LEFT JOIN billing.ledger_type_totals t ON t.merchant_id = a.merchant_id AND t.book = a.book AND t.currency = a.currency
        AND t.account_type = a.account_type AND t.psp_id IS NOT DISTINCT FROM a.psp_id
        AND t.bank_account_id IS NOT DISTINCT FROM a.bank_account_id
    WHERE a.merchant_id = sqlc.arg(merchant_id)::uuid
      AND a.book = 'general' AND a.customer_id IS NULL
) balances
GROUP BY book, currency
HAVING SUM(net) <> 0
ORDER BY book, currency;

-- ListLedgerSignBreaches: general-ledger accounts across their sign rule, a
-- customer's by its counters, a merchant-wide one by its type total.
-- name: ListLedgerSignBreaches :many
SELECT currency::text AS currency, account_type::text AS account_type, psp_id, bank_account_id, customer_id,
       credits_posted::bigint AS credits_posted, debits_posted::bigint AS debits_posted
FROM (
    SELECT a.currency, a.account_type, a.psp_id, a.bank_account_id, a.customer_id,
           a.debits_must_not_exceed_credits AS dmnec, a.credits_must_not_exceed_debits AS cmned,
           COALESCE(CASE WHEN a.customer_id IS NULL THEN t.credits_posted ELSE a.credits_posted END, 0) AS credits_posted,
           COALESCE(CASE WHEN a.customer_id IS NULL THEN t.debits_posted ELSE a.debits_posted END, 0) AS debits_posted
    FROM billing.ledger_accounts a
    LEFT JOIN billing.ledger_type_totals t ON a.customer_id IS NULL AND t.merchant_id = a.merchant_id AND t.book = a.book
        AND t.currency = a.currency AND t.account_type = a.account_type AND t.psp_id IS NOT DISTINCT FROM a.psp_id
        AND t.bank_account_id IS NOT DISTINCT FROM a.bank_account_id
    WHERE a.merchant_id = sqlc.arg(merchant_id)::uuid AND a.book = 'general'
) balances
WHERE dmnec AND debits_posted > credits_posted OR cmned AND credits_posted > debits_posted
ORDER BY currency, account_type, psp_id, bank_account_id, customer_id;

-- ListLedgerSideWarnings: merchant-wide general-ledger accounts with no sign
-- rule whose type total sits on the side opposite their class's normal one.
-- name: ListLedgerSideWarnings :many
SELECT a.currency, a.account_type, a.class, a.psp_id, a.bank_account_id, t.credits_posted, t.debits_posted
FROM billing.ledger_accounts a
JOIN billing.ledger_type_totals t ON t.merchant_id = a.merchant_id AND t.book = a.book AND t.currency = a.currency
    AND t.account_type = a.account_type AND t.psp_id IS NOT DISTINCT FROM a.psp_id
    AND t.bank_account_id IS NOT DISTINCT FROM a.bank_account_id
WHERE a.merchant_id = sqlc.arg(merchant_id)::uuid
  AND a.book = 'general' AND a.customer_id IS NULL
  AND NOT a.debits_must_not_exceed_credits AND NOT a.credits_must_not_exceed_debits
  AND CASE WHEN a.class IN ('asset', 'contra_revenue', 'expense') THEN t.credits_posted > t.debits_posted
           ELSE t.debits_posted > t.credits_posted END
ORDER BY a.currency, a.account_type, a.psp_id, a.bank_account_id;

-- ListLedgerCounterDrifts rebuilds account counters from the immutable
-- transfer log and returns every account whose maintained projection differs.
-- A merchant-wide general-ledger account keeps none: its counters stay 0, and
-- its balance is checked as rollup drift.
-- name: ListLedgerCounterDrifts :many
WITH logged AS (
    SELECT account_id, SUM(credit)::bigint AS credits, SUM(debit)::bigint AS debits
    FROM (
        SELECT credit_account_id AS account_id, amount AS credit, 0::bigint AS debit
        FROM billing.ledger_transfers
        WHERE merchant_id = sqlc.arg(merchant_id)::uuid
        UNION ALL
        SELECT debit_account_id, 0::bigint, amount
        FROM billing.ledger_transfers
        WHERE merchant_id = sqlc.arg(merchant_id)::uuid
    ) legs
    GROUP BY account_id
)
SELECT a.id AS account_id,
       a.merchant_id,
       a.book,
       a.currency,
       a.account_type,
       a.customer_id,
       a.credits_posted AS stored_credits,
       COALESCE(l.credits, 0)::bigint AS logged_credits,
       a.debits_posted AS stored_debits,
       COALESCE(l.debits, 0)::bigint AS logged_debits
FROM billing.ledger_accounts a
LEFT JOIN logged l ON l.account_id = a.id AND (a.book = 'units' OR a.customer_id IS NOT NULL)
WHERE a.merchant_id = sqlc.arg(merchant_id)::uuid
  AND (a.credits_posted <> COALESCE(l.credits, 0)
    OR a.debits_posted <> COALESCE(l.debits, 0))
ORDER BY a.merchant_id, a.currency, a.id;

-- SumLedgerSpendByCoords: the total posted at one operation coordinate. A spend
-- fans out into one credit_spend per FIFO lot drawn plus at most one
-- owed_accrual, so only the sum is the operation's amount: a retry carrying a
-- different amount reuses the key with a changed body.
-- name: SumLedgerSpendByCoords :one
SELECT COALESCE(SUM(amount), 0)::bigint AS total, count(*)::bigint AS transfers
FROM billing.ledger_transfers
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND customer_id = sqlc.arg(customer_id)::uuid
  AND currency = sqlc.arg(currency)::text
  AND transfer_type IN ('credit_spend', 'owed_accrual')
  AND operation = sqlc.arg(operation)::text
  AND source = sqlc.arg(source)::text
  AND source_id = sqlc.arg(source_id)::text;

-- ListLedgerTypeTotalDrifts recomputes ledger_type_totals from the transfer
-- log and returns every key whose kept total differs; a missing row keeps 0.
-- name: ListLedgerTypeTotalDrifts :many
WITH legs AS (
    SELECT t.book, t.currency, a.account_type, a.psp_id, a.bank_account_id,
           CASE WHEN a.id = t.credit_account_id THEN t.amount ELSE 0 END AS credit,
           CASE WHEN a.id = t.debit_account_id THEN t.amount ELSE 0 END AS debit
    FROM billing.ledger_transfers t
    JOIN billing.ledger_accounts a ON a.merchant_id = t.merchant_id AND a.id IN (t.debit_account_id, t.credit_account_id)
    WHERE t.merchant_id = sqlc.arg(merchant_id)::uuid
), sides AS (
    SELECT book, currency, account_type, psp_id, bank_account_id, 0::bigint AS stored_credits, 0::bigint AS stored_debits, credit AS logged_credits, debit AS logged_debits
    FROM legs
    UNION ALL
    SELECT book, currency, account_type, psp_id, bank_account_id, credits_posted, debits_posted, 0, 0
    FROM billing.ledger_type_totals
    WHERE merchant_id = sqlc.arg(merchant_id)::uuid
)
SELECT book::text AS book,
       currency::text AS currency,
       account_type::text AS account_type,
       psp_id,
       bank_account_id,
       SUM(stored_credits)::bigint AS stored_credits,
       SUM(logged_credits)::bigint AS logged_credits,
       SUM(stored_debits)::bigint AS stored_debits,
       SUM(logged_debits)::bigint AS logged_debits
FROM sides
GROUP BY book, currency, account_type, psp_id, bank_account_id
HAVING SUM(stored_credits) <> SUM(logged_credits) OR SUM(stored_debits) <> SUM(logged_debits)
ORDER BY book, currency, account_type, psp_id, bank_account_id;

-- ListLedgerDailyDrifts recomputes ledger_daily from the transfer log and
-- returns every bucket whose kept amounts differ; a missing row keeps 0.
-- name: ListLedgerDailyDrifts :many
WITH legs AS (
    SELECT t.book, t.currency, a.account_type,
           date_bin('15 minutes', t.effective_at, '2000-01-01 00:00:00+00'::timestamptz) AS bucket_at,
           t.transfer_type, a.psp_id, a.bank_account_id, t.product_id,
           CASE WHEN a.id = t.credit_account_id THEN t.amount ELSE 0 END AS credit,
           CASE WHEN a.id = t.debit_account_id THEN t.amount ELSE 0 END AS debit
    FROM billing.ledger_transfers t
    JOIN billing.ledger_accounts a ON a.merchant_id = t.merchant_id AND a.id IN (t.debit_account_id, t.credit_account_id)
    WHERE t.merchant_id = sqlc.arg(merchant_id)::uuid
), sides AS (
    SELECT book, currency, account_type, bucket_at, transfer_type, psp_id, bank_account_id, product_id,
           0::bigint AS stored_credits, 0::bigint AS stored_debits, credit AS logged_credits, debit AS logged_debits
    FROM legs
    UNION ALL
    SELECT book, currency, account_type, bucket_at, transfer_type, psp_id, bank_account_id, product_id, credits, debits, 0, 0
    FROM billing.ledger_daily
    WHERE merchant_id = sqlc.arg(merchant_id)::uuid
)
SELECT book::text AS book,
       currency::text AS currency,
       account_type::text AS account_type,
       bucket_at::timestamptz AS bucket_at,
       transfer_type::text AS transfer_type,
       psp_id,
       bank_account_id,
       product_id,
       SUM(stored_credits)::bigint AS stored_credits,
       SUM(logged_credits)::bigint AS logged_credits,
       SUM(stored_debits)::bigint AS stored_debits,
       SUM(logged_debits)::bigint AS logged_debits
FROM sides
GROUP BY book, currency, account_type, bucket_at, transfer_type, psp_id, bank_account_id, product_id
HAVING SUM(stored_credits) <> SUM(logged_credits) OR SUM(stored_debits) <> SUM(logged_debits)
ORDER BY book, currency, account_type, bucket_at, transfer_type, psp_id, bank_account_id, product_id;

-- name: GetLedgerUnitScale :one
-- The scale the units book counts a merchant's unit at, once it counts it.
SELECT scale FROM billing.ledger_units WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND unit = sqlc.arg(unit)::text;

-- name: InsertLedgerUnit :exec
-- Records a merchant's unit as the units book first counts it.
INSERT INTO billing.ledger_units (merchant_id, unit, scale)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.arg(unit)::text, sqlc.arg(scale)::int)
ON CONFLICT (merchant_id, unit) DO NOTHING;

-- name: ListLedgerUnits :many
-- The merchant's units the units book counts, and their scales.
SELECT unit, scale FROM billing.ledger_units WHERE merchant_id = sqlc.arg(merchant_id)::uuid ORDER BY unit;
