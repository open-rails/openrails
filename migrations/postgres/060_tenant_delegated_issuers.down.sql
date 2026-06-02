-- Down for 051 — drop the federated delegated-token issuer registry (issue #259).
SET lock_timeout = '10s';

DROP TABLE IF EXISTS billing.tenant_delegated_issuers;
