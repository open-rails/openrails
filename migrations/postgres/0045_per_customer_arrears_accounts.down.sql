-- Re-home arrears back onto one merchant-wide account per (merchant, currency).
SET LOCAL statement_timeout = '60s';
SET LOCAL lock_timeout = '10s';

INSERT INTO openrails.ledger_accounts
    (merchant_id, customer_id, account_type, currency,
     debits_must_not_exceed_credits, credits_must_not_exceed_debits)
SELECT DISTINCT a.merchant_id, NULL, 'arrears_liability', a.currency, false, false
FROM openrails.ledger_accounts a
WHERE a.account_type = 'arrears_liability' AND a.customer_id IS NOT NULL
ON CONFLICT DO NOTHING;

UPDATE openrails.ledger_transfers t
   SET debit_account_id = sys.id
  FROM openrails.ledger_accounts cust, openrails.ledger_accounts sys
 WHERE t.transfer_type = 'owed_accrual'
   AND cust.id = t.debit_account_id AND cust.account_type = 'arrears_liability' AND cust.customer_id IS NOT NULL
   AND sys.merchant_id = t.merchant_id AND sys.account_type = 'arrears_liability'
   AND sys.customer_id IS NULL AND sys.currency = t.currency;

UPDATE openrails.ledger_transfers t
   SET credit_account_id = sys.id
  FROM openrails.ledger_accounts cust, openrails.ledger_accounts sys
 WHERE t.transfer_type = 'owed_payment'
   AND cust.id = t.credit_account_id AND cust.account_type = 'arrears_liability' AND cust.customer_id IS NOT NULL
   AND sys.merchant_id = t.merchant_id AND sys.account_type = 'arrears_liability'
   AND sys.customer_id IS NULL AND sys.currency = t.currency;

UPDATE openrails.ledger_accounts a
   SET credits_posted = COALESCE(l.credits, 0), debits_posted = COALESCE(l.debits, 0)
  FROM (SELECT acct AS account_id, SUM(credit)::bigint AS credits, SUM(debit)::bigint AS debits
          FROM (SELECT credit_account_id AS acct, amount AS credit, 0::bigint AS debit FROM openrails.ledger_transfers
                UNION ALL
                SELECT debit_account_id AS acct, 0::bigint AS credit, amount AS debit FROM openrails.ledger_transfers) legs
         GROUP BY acct) l
 WHERE a.account_type = 'arrears_liability' AND l.account_id = a.id;

DELETE FROM openrails.ledger_accounts a
 WHERE a.account_type = 'arrears_liability' AND a.customer_id IS NOT NULL
   AND NOT EXISTS (SELECT 1 FROM openrails.ledger_transfers t
                    WHERE t.debit_account_id = a.id OR t.credit_account_id = a.id);
