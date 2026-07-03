# ADR: Custodial merchant secrets — one process-global Vault token, OpenRails-owned isolation

**Status:** accepted (2026-07-02, #724; folded from #722)
**Context docs:** `docs/vault.md` (capability model, policies), `docs/vault-secret-ops.md` (operator runbook)

## Decision

OpenRails is the **custodian** of merchant provider credentials. The process authenticates to
Vault **once** (token / AppRole / Kubernetes — `internal/integrations/vault/auth.go`) and that
single token spans **all** merchant namespaces. Merchant isolation is **OpenRails' job**, not
Vault's:

1. **Addressing.** Every secret is addressed by `(merchant id, name)`. The Vault store maps this
   to `secret/openrails/merchants/<merchant-slug>/<name>` through exactly one namespace builder
   (`vaultSecretStore.pathFor`, `internal/merchants/secrets_vault.go`); provider-account names
   come from exactly one name builder (`merchants.RailMerchantAccountSecretName`,
   `internal/merchants/secrets.go`, the durable `rail_merchant_accounts/...` shape, #683).
   No other code constructs these paths — enforced by the grep-style guard test
   `TestNoAdHocSecretPathConstruction` (`internal/merchants/secret_path_guard_test.go`), which
   fails the unit suite on any non-allowlisted occurrence of the path fragments.
2. **Request scoping.** Which merchant id a request may use is decided by OpenRails' auth +
   RLS machinery (`MerchantTx` / the `app.merchant_id` GUC) long before a Vault path is built.
   Vault never sees, and cannot check, merchant identity.
3. **DB parity.** The DEK-encrypted Postgres store is the degenerate fallback backend and keeps
   the SAME `(merchant, name)` contract; `TestBackendParity_CycleRotationIsolation`
   (`internal/merchantsecrets/vault_scenarios_integration_test.go`) pins the two stores'
   cycle/rotation/isolation/status semantics against a real Vault and real Postgres so they
   cannot drift.

## Threat model

| Threat | Answer |
|---|---|
| Merchant A reads/overwrites B's credentials via the API | Impossible without a bug in OpenRails' merchant resolution: paths are derived from the resolved merchant id, never from request-supplied path fragments. The name parser rejects malformed names; slugs are DB-resolved (`NewDBMerchantSlugResolver`). |
| Ad-hoc path construction silently escapes the namespace | Guard test fails the build/review loop (see above). |
| Vault outage read as "secret absent" (would disable webhook verification, cancel work) | Typed split: `ErrSecretNotFound` (terminal absence) vs `ErrSecretBackendUnavailable` (retry). Outage semantics pinned by container pause/unpause tests: boot fails loudly, uncached reads fail closed, unpause recovers without restart. |
| Compromised OpenRails process | Out of scope BY DESIGN: the process is the custodian, so its token legitimately spans all merchants. Blast-radius reduction is policy-side (`secret/data/openrails/*` only — docs/vault.md) and operational (short-TTL AppRole tokens, revocation). Revocation ⇒ loud failure; re-login is supervisor/restart-owned, never emergent (pinned in `TestTokenLifecycle_RenewalAndRevocation`). |
| Token capability drift (e.g. policy narrowed to read-only) | #661 probe at boot gates features (`gateSecretBackend`, route gates); Vault's runtime 403 stays the real boundary and surfaces as `ErrSecretBackendUnavailable`, never as silent success. |

## Non-goals (deliberate)

- **Per-merchant Vault tokens/policies.** One merchant ↔ one org, but ONE process serves them
  all; minting/renewing/revoking N tokens adds a distributed-lifecycle problem with no isolation
  gain (the process would still hold all N). Isolation stays in OpenRails' addressing + RLS.
- **Merchant-visible Vault.** Merchants interact with credentials only through the OpenRails API
  (`PUT /v1/merchant/payment-providers`, status lists are redacted). Vault is an implementation
  detail of the hosted platform, never a merchant-facing surface.
- **Cross-backend replication.** `secret_backend` is declared intent; there is no auto-fallback
  and no sync between Vault and the DB store (a fallback store would run silently empty).

## Revisit trigger

**BYO-Vault** (a merchant/org bringing its own Vault, or regulatory demands for
customer-managed keys): that breaks the custodial premise — per-tenant Vault connections,
per-tenant tokens, and merchant-visible policy become real requirements. Revisit this ADR then;
until then, per-merchant tokens are complexity without a threat-model payoff.
