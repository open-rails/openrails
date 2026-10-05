# Provider sandbox posture (#1055)

Under `test_mode: sandbox`, every PSP credential must be proven to belong to a
test account before OpenRails sends it a mutation.

## When

Once per loaded credential set, never per payment:

- Runtime startup, in the background (`Runtime.StartProviderPosture`;
  standalone boot and `embed.New`): construction never waits on a provider.
  Embedded runtimes verify only their configured merchant.
- Credential create/rotate through the merchant API (the write is rejected
  on anything but a simulated verdict).
- The first mutation of a credential set this process has not verified yet
  (e.g. rotated by another process).

Verdicts live in the process registry (`internal/providerposture`), keyed by
rail + merchant + PSP + account + endpoint + credential fingerprint. Any
change is a new key and a fresh verification.

NMI clients have one constructor, `railresolve.NMIFactory`: it binds the
PSP's merchant, id, account, versioned security key and declared
`endpoint_deployment`. `nmi.NewAccountClient` refuses a client without that
identity, so no path can build a bare-key client whose key differs from the
verified one.

## Fail closed

Only `simulated` arms. `live`, `mismatched`, `unknown` (unavailable,
malformed) and `unsupported` disarm: mutations fail locally with
`providerposture.ErrDisarmed` before any bytes are sent; reads work. The
runtime still starts and stays ready; `Ready()` lists `psp_posture` as a
degraded optional dependency and the embedded `openrails_psp_posture` probe
fails while a loaded PSP is unverified or disarmed. `unknown` is re-verified
in the background and on the next mutation with capped full-jitter backoff
(≤30s), so a provider outage never needs a restart. `live` and `mismatched`
hold until the credential is reloaded.

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
