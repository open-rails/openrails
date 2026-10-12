-- Repair: none-needed A dropped column and a dropped table refuse no stored row, and nothing reads them.
-- A request names its merchant by its credential, never by its host: a
-- merchant has no API host, so the host and its unproven claims go.
ALTER TABLE billing.merchants DROP COLUMN api_host;
DROP TABLE billing.merchant_api_host_claims;
