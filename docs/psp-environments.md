# PSP environments

Every PSP declares whether it moves real money, and OpenRails asks the
provider before it trusts the answer.

```yaml
psps:
  mobius:
    rail: nmi
    environment: sandbox # live | test | sandbox
```

| `environment` | Meaning | Rails |
|---|---|---|
| `live` | Real money. | every rail |
| `test` | The provider's own system; no money moves. | NMI (a gateway account in test mode), Stripe (`sk_test_`/`rk_test_` keys) |
| `sandbox` | The provider's separate sandbox system. | NMI (a sandbox.nmi.com account), CCBill (its sandbox), Solana (devnet) |

The field is required, and a rail refuses an environment it does not have. It is
part of the PSP's identity: the same account id in another environment is
another PSP.

A deployment is all live or all non-live. Mixing is refused at boot (declared
PSPs, and the PSPs the database already holds), and a PSP created or loaded
later in the other class is refused. Fake providers (`provider_sandbox`
gateways, an http HyperSwitch fixture, the `Deps` test seams) are refused beside
a live PSP.

## What follows from it

| Rail | `live` | `test` | `sandbox` |
|---|---|---|---|
| NMI | regular gateway | regular gateway | sandbox.nmi.com |
| Stripe | `sk_live_`/`rk_live_` key | `sk_test_`/`rk_test_` key | — |
| CCBill | api.ccbill.com | — | sandbox-api.ccbill.com |
| Solana | mainnet | — | devnet (DUSD rebills; $1.00 price fallback) |

## When it is verified

Once per loaded credential set, never per payment:

- At startup, in the background: `openrails.New` and the standalone server
  never wait on a provider. An embedded engine verifies only its declared
  merchant.
- When a credential is created or rotated through the admin API (the write
  is refused on anything but a matching verdict).
- When a PSP's configuration changes, on every instance as it reloads the
  merchant. A PSP disarmed at startup is offered again once its credential is
  fixed, without a restart.
- At the first mutation of a credential set this process has not verified yet.

A verdict is keyed by rail, merchant, PSP, account, environment, endpoint and
credential fingerprint: any change is a fresh verification.

## Fail closed

Only the verdict the environment expects arms: `live` for a live PSP,
`simulated` for a test or sandbox one. Anything else (`mismatched`, `unknown`,
`unsupported`, or the opposite verdict) disarms: mutations fail locally before
any bytes are sent; reads work. The engine still starts and stays ready, and the
`openrails_psp_posture` probe fails while a loaded PSP is unverified or
disarmed. `unknown` is verified again in the background and on the next
mutation with capped backoff (at most 30 seconds). A disagreeing answer holds
until the PSP's configuration changes.

## Signals

Authoritative only; never a hostname or a label.

| PSP | Signal |
|---|---|
| NMI `live` | Query API `test_mode_status` is `false` (read-only) |
| NMI `test` | Query API `test_mode_status` is `true` (read-only) |
| NMI `sandbox` | An authorization on the non-issued test card is approved, then voided (NMI has no read-only sandbox signal) |
| NMI through a custodian proxy (Basis Theory, HyperSwitch) | The same as NMI, for the forwarded key and destination |
| Stripe | The key's prefix agrees with the environment; a test key's account is the declared `acct_…` and its balance reads `livemode: false` |
| Solana `sandbox` | Every RPC endpoint's genesis hash is devnet or testnet |
| CCBill `sandbox` | None exists (DataLink `testMode=1` only selects synthetic reports): mutations are always refused |
