-- or#897 / or#878 ruling: arrears liability becomes PER-DEBTOR.
--
-- 0001 modelled arrears_liability as ONE merchant-wide system account
-- (customer_id NULL). That account can answer "how much does this merchant have
-- outstanding in total" and nothing else, so per-payer exposure had to be summed
-- over that payer's whole owed_accrual/owed_payment history — O(records) on the
-- ADMISSION HOT PATH. That is a direct violation of the work-scales-with-
-- activity law, and the degradation is silent: it gets slower as a customer
-- transacts more, which is exactly when admission matters most. It is also just
-- the wrong double-entry shape; receivables are per-debtor sub-accounts.
--
-- After this migration, outstanding owed is the customer's arrears account
-- balance, negated — an O(1) counter read, symmetric with prepaid balance.
--
-- HARD CUT (prelaunch, no compat shims): the per-customer accounts are THE
-- representation. The merchant-wide account is not kept alongside — two
-- representations of one liability is the same disease as the two exposure
-- substrates this ruling removed. Merchant-wide totals are a SUM over the
-- per-customer accounts, for reporting only, never on the hot path.
--
-- CONSERVATION: the re-homing below moves which ACCOUNT each arrears leg points
-- at; it never changes an amount, never adds or drops a transfer, and never
-- makes a transfer one-sided. Every transfer keeps exactly one debit and one
-- credit, so the ledger-wide sum of (credits - debits) is unchanged. The
-- counters are then recomputed from the transfer log itself — the same
-- projection the trigger maintains — so accounts and log agree by construction.
-- Proven, not asserted: the migration test runs the or#833 integrity checks
-- (conservation + counter drift) across the boundary.
--
-- Why UPDATE rather than compensating transfers: this is a one-time schema-era
-- correction executed by the migration owner, not by openrails_app (which still
-- holds SELECT,INSERT only — LED-5 is untouched). A compensating-transfer
-- re-home would invent money movements that never happened and permanently
-- pollute every per-customer arrears history with a synthetic leg, to describe
-- a change that is not a money movement at all.

SET LOCAL statement_timeout = '60s';
SET LOCAL lock_timeout = '10s';

-- 1. One arrears account per (merchant, customer, currency) that has arrears.
--    debits_must_not_exceed_credits stays FALSE: the account is MEANT to go
--    negative — that negative balance is the debt.
INSERT INTO openrails.ledger_accounts
    (merchant_id, customer_id, account_type, currency,
     debits_must_not_exceed_credits, credits_must_not_exceed_debits)
SELECT DISTINCT t.merchant_id, t.customer_id, 'arrears_liability', t.currency, false, false
FROM openrails.ledger_transfers t
WHERE t.transfer_type IN ('owed_accrual', 'owed_payment')
  AND t.customer_id IS NOT NULL
ON CONFLICT DO NOTHING;

-- 2. Re-home the legs. An accrual DEBITS the liability; a payment CREDITS it.
UPDATE openrails.ledger_transfers t
   SET debit_account_id = c.id
  FROM openrails.ledger_accounts sys, openrails.ledger_accounts c
 WHERE t.transfer_type = 'owed_accrual'
   AND t.customer_id IS NOT NULL
   AND sys.id = t.debit_account_id
   AND sys.account_type = 'arrears_liability'
   AND sys.customer_id IS NULL
   AND c.merchant_id = t.merchant_id
   AND c.customer_id = t.customer_id
   AND c.account_type = 'arrears_liability'
   AND c.currency = t.currency;

UPDATE openrails.ledger_transfers t
   SET credit_account_id = c.id
  FROM openrails.ledger_accounts sys, openrails.ledger_accounts c
 WHERE t.transfer_type = 'owed_payment'
   AND t.customer_id IS NOT NULL
   AND sys.id = t.credit_account_id
   AND sys.account_type = 'arrears_liability'
   AND sys.customer_id IS NULL
   AND c.merchant_id = t.merchant_id
   AND c.customer_id = t.customer_id
   AND c.account_type = 'arrears_liability'
   AND c.currency = t.currency;

-- 3. Rebuild the counters for every arrears account from the immutable log. The
--    trigger only fires on INSERT, so a re-homed row moves no counters by
--    itself; this is the same projection the trigger maintains, recomputed.
UPDATE openrails.ledger_accounts a
   SET credits_posted = COALESCE(l.credits, 0),
       debits_posted  = COALESCE(l.debits, 0)
  FROM (
        SELECT acct AS account_id,
               SUM(credit)::bigint AS credits,
               SUM(debit)::bigint  AS debits
        FROM (
            SELECT credit_account_id AS acct, amount AS credit, 0::bigint AS debit
            FROM openrails.ledger_transfers
            UNION ALL
            SELECT debit_account_id AS acct, 0::bigint AS credit, amount AS debit
            FROM openrails.ledger_transfers
        ) legs
        GROUP BY acct
       ) l
 WHERE a.account_type = 'arrears_liability'
   AND l.account_id = a.id;

-- Any arrears account with no remaining legs is genuinely zero.
UPDATE openrails.ledger_accounts a
   SET credits_posted = 0, debits_posted = 0
 WHERE a.account_type = 'arrears_liability'
   AND NOT EXISTS (
        SELECT 1 FROM openrails.ledger_transfers t
         WHERE t.debit_account_id = a.id OR t.credit_account_id = a.id);

-- 4. Retire the merchant-wide accounts. Deleting is safe ONLY because step 3
--    proved them empty; a system account still carrying legs would fail here
--    and stop the migration rather than orphan a transfer.
DELETE FROM openrails.ledger_accounts a
 WHERE a.account_type = 'arrears_liability'
   AND a.customer_id IS NULL
   AND NOT EXISTS (
        SELECT 1 FROM openrails.ledger_transfers t
         WHERE t.debit_account_id = a.id OR t.credit_account_id = a.id);

COMMENT ON COLUMN openrails.ledger_accounts.account_type IS 'Account role within a (merchant, currency) ledger. arrears_liability is PER-CUSTOMER (or#897): its negated balance is that payer''s outstanding owed, read O(1) on the admission path. customer_balance is per-customer; processor_clearing / platform_revenue / expired_credits / revoked_credits / fx_liquidity / world are merchant-wide system accounts.';
