# Merchant custody configuration

Status: scoped read/write and PSP-reference HTTP contracts are locally qualified;
the complete merchant-owned card lifecycle and Settings controls remain under
qualification. No release or live vendor qualification is claimed.

Merchant custody is independent of the hosted shared network-customer product.
A merchant may use its PSP's NMI vault, its own Basis Theory custody with an NMI
PSP, or the separately configured hosted vault in its merchant partition.
Changing new-card custody does not migrate stored methods or accepted mandates.

Existing remote merchant configuration can set `card_vault` and `charge_psps`;
PSP create/update manages gateway accounts and credentials. Custodian documents
currently require operator manifests or direct Vault administration. The console
exposes PSP account/credential setup but no merchant custodian setup or custody
selection. A merchant cannot complete the Basis Theory path unaided.

The minimal extension reuses existing merchant permissions, configuration CAS,
secret storage and custody identities:

- GET `/v1/merchant/custodians` returns declared custodian settings and credential
  presence, never secret values.
- PUT `/v1/merchant/custodians/{key}` writes one existing configuration document
  with explicit revision and immutable vendor/environment/account identity.
- Existing PSP create/update accepts a declared merchant custodian reference.
- Existing Settings presents custody separately from charge PSP selection.

A new vendor account claim must prove control of that exact account. A successful
mock response is not vendor qualification. The current Basis Theory applications
API requires `application:read`; the supported application-key/ownership probe
must be verified before self-service acceptance is implemented. Operator
configuration remains an explicit trust boundary, not a fallback for an unproven
merchant API claim.

Qualification uses actual merchant HTTP/Go interfaces, PostgreSQL, a real isolated
Vault mount and an external provider protocol simulator. It covers permission
and merchant isolation, secret redaction, optimistic concurrency, immutable
ownership, and existing stored-method/subscription behavior during new-card
custody changes. No new configuration store or schema is needed.

## Vendor contract checked 2026-10-25

- [Get application by key](https://developers.basistheory.com/docs/api/applications#get-an-application-by-key)
  uses the submitted API key and returns its tenant. It requires
  `application:read`; configuration refuses a key that cannot prove that account.
- [Regional endpoints](https://developers.basistheory.com/docs/api/regional-endpoints)
  distinguish test tenants at `api.test.basistheory.com` from production regions.
  The legacy production endpoint is retained for existing configurations; test
  custody must use the test endpoint.
- [Token intents](https://developers.basistheory.com/docs/api/tokens/token-intents)
  expose vendor-attested tenant/type/creator metadata and expire separately from
  durable tokens. A caller's opaque ID alone is not an ownership assertion.
- [Ephemeral proxy](https://developers.basistheory.com/docs/api/proxies/invoke-ephemeral-proxies)
  forwards token expressions to the configured gateway. Gateway receipt evidence,
  not the proxy's retry behavior, determines whether an uncertain charge happened.

The core's existing proxy and mandate machinery remains the money authority.
An external fixture proves the implemented wire/identity/recovery contract. It
does not establish that an unprovided real account has those permissions or
qualify live card acceptance, cross-merchant sharing or vault portability.
