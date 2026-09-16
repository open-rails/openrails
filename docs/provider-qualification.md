# Provider sandbox qualification

NMI accounts require a fresh test-mode probe before each sandbox arm through
manifest reconciliation or the merchant provider API. Simulated approval permits
the arm. A live decline, gateway error, transport failure or unavailable effective
credential refuses it. The effective stored key is checked when an update omits
credentials; a secret-backend failure cannot bypass qualification.

The SQL probe verdict cache is removed from the fresh schema. The lost capability
is avoiding repeated probes during repeated configuration/boot attempts. Provider
probing itself remains core. A bounded cache may be reconsidered only if measured
load justifies it, with explicit qualification freshness rules; no replacement is
implemented here.

Production mode does not run sandbox probes. Credential validation, PSP account
identity, stored-card portability and account-updater batches remain unchanged.
