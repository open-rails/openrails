-- parent: 47 sha256:76bc3367799f204fff6fa3b0d7bb43ab1df524110135158f2c994b2147aa4269
-- Repair: none-needed A dropped table refuses no stored row, and nothing reads it.
-- Rate limits, lockouts and captcha challenges are in Redis or each process's
-- memory, and AuthKit spends DPoP proofs; nothing reads this table.
DROP TABLE billing.rate_windows;
