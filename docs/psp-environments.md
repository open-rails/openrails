# PSP environments

Every PSP declares whether it moves real money, and OpenRails asks the
provider before it trusts the answer.

```yaml
psps:
  mobius:
    rail: nmi
    environment: sandbox
```

A PSP's rail decides its environments; omitted, it is the rail's real-money
one, so production declares none. Any other value is refused, naming the
rail's.

| Rail | Real money (default) | Non-live |
|---|---|---|
| NMI | `live`: a regular gateway | `test`: a gateway account in test mode; `sandbox`: a sandbox.nmi.com account |
| Stripe | `live`: `sk_live_`/`rk_live_` keys | `test`: `sk_test_`/`rk_test_` keys |
| CCBill | `live`: api.ccbill.com | `sandbox`: sandbox-api.ccbill.com |
| Solana | `mainnet` | `devnet`, `testnet` ($1.00 price fallback) |

The PSP's identity is its rail, money class (real money or not) and account:
a PSP re-declared between two non-live environments of its rail keeps its id
and subscriptions, its stored environment is corrected, and its posture is
verified again before it is used. Real money and non-live are different PSPs;
a key cannot move between them.

A merchant is live, the default, or a sandbox:

```yaml
merchants:
  acme-sandbox:
    sandbox: true      # its PSPs move no money
```

A live merchant's PSPs move real money; a sandbox merchant's move none. A PSP
that disagrees with its merchant is refused alone, with why, and the rest of
the merchant serves; one deployment serves live and sandbox merchants side by
side. A merchant's sandbox is another merchant: there is no test mode inside a
merchant. `provider_write_mode` stays deployment-wide. Fake providers
(`provider_sandbox` gateways, an http HyperSwitch fixture, the `Deps` test
seams) are refused beside a real-money PSP.

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

Only the verdict the environment expects arms: `live` for a real-money PSP,
`simulated` for a non-live one. Anything else (`mismatched`, `unknown`,
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
| Solana | Every RPC endpoint (its `rpc_url` and the network's public one) reports the declared network's genesis hash |
| CCBill `sandbox` | None exists (DataLink `testMode=1` only selects synthetic reports): mutations are always refused |
