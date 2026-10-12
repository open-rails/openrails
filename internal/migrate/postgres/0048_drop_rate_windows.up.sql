-- Repair: none-needed A dropped table refuses no stored row, and nothing reads it.
-- Rate limits, lockouts and captcha challenges are in Redis or each process's
-- memory, and AuthKit spends DPoP proofs; nothing reads this table.
DROP TABLE billing.rate_windows;
