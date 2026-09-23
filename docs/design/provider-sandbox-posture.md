# Provider sandbox posture (#1055)

Under `test_mode: sandbox`, every PSP credential must be proven to belong to a
test account before OpenRails sends it a mutation.

## When

Once per loaded credential set, never per payment:

- Runtime startup (`Runtime.VerifyProviderPosture`; standalone boot and
  `embed.New`). Embedded runtimes verify only their configured merchant.
- Credential create/rotate through the merchant API (the write is rejected
  on anything but a simulated verdict).
- The first mutation of a credential set this process has not verified yet
  (e.g. rotated by another process).

Verdicts live in the process registry (`internal/providerposture`), keyed by
rail + merchant + PSP + account + endpoint + credential fingerprint. Any
change is a new key and a fresh verification.

## Fail closed

Only `simulated` arms. `live`, `mismatched`, `unknown` (unavailable,
malformed) and `unsupported` disarm: mutations fail locally with
`providerposture.ErrDisarmed` before any bytes are sent; reads work. The
runtime still starts; `Ready()` fails its `psp_posture` dependency while a
loaded PSP is disarmed, so a host that wants hard failure checks `Ready`.
`unknown` is re-verified no sooner than 30s later (by `Ready` or the next
mutation), so a network blip does not need a restart. `live` and
`mismatched` hold until the credential is reloaded.

## Signals (authoritative only; never hostname or label inference)

| Rail | Signal |
|---|---|
| NMI `endpoint_deployment: gateway` | Query API `test_mode_status` = `true` (read-only) |
| NMI `endpoint_deployment: sandbox` | Auth on non-issued test card approved, then voided (NMI has no read-only sandbox signal) |
| NMI via custodian proxy (Basis Theory, HyperSwitch) | Same as NMI for the forwarded key and destination |
| Stripe | `sk_test_`/`rk_test_` prefix, `GET /v1/account` id = declared `acct_…`, `GET /v1/balance` `livemode:false` |
| Solana | Every RPC endpoint's `getGenesisHash` is devnet or testnet |
| CCBill | None exists (DataLink `testMode=1` only selects synthetic reports): mutations are always refused under sandbox |

## Choke points

NMI `sendDirectRequest`/`sendV5Request` (mutating), the Basis Theory and
HyperSwitch NMI proxy chargers, the Stripe `stripeapi` transport (non-GET),
CCBill DataLink `CancelSubscription`, Solana `SendTransaction*`.

## Fixtures

A loopback fake is exempt only with an explicit marker (`LoopbackFixture`,
`config.ProviderSandbox`, an injected Stripe transport, or a code-level
endpoint override seam) and only while the destination is a literal loopback
IP.
