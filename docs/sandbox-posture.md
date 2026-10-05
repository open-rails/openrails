# Sandbox posture

Under `test_mode: sandbox`, every PSP credential must be proven to belong to a
test account before OpenRails sends it a mutation. Configuration is not
trusted; the credential is asked.

## When

Once per loaded credential set, never per payment:

- At startup, in the background: `openrails.New` and the standalone server
  never wait on a provider. An embedded engine verifies only its declared
  merchant.
- When a credential is created or rotated through the merchant API (the write
  is refused on anything but a simulated verdict).
- At the first mutation of a credential set this process has not verified yet
  (for example one rotated by another process).

A verdict is keyed by rail, merchant, PSP, account, endpoint and credential
fingerprint: any change is a fresh verification.

## Fail closed

Only a `simulated` verdict arms. `live`, `mismatched`, `unknown` (unavailable or
malformed) and `unsupported` disarm: mutations fail locally before any bytes
are sent; reads work. The engine still starts and stays ready, and the
`openrails_psp_posture` probe (`Client.Probes`) fails while a loaded PSP is
unverified or disarmed. `unknown` is verified again in the background and on
the next mutation with capped backoff (at most 30 seconds), so a provider
outage never needs a restart. `live` and `mismatched` hold until the
credential is reloaded.

## Signals

Authoritative only; never a hostname or a label.

| Rail | Signal |
|---|---|
| NMI `endpoint_deployment: gateway` | Query API `test_mode_status` is `true` (read-only) |
| NMI `endpoint_deployment: sandbox` | An authorization on the non-issued test card is approved, then voided (NMI has no read-only sandbox signal) |
| NMI through a custodian proxy (Basis Theory, HyperSwitch) | The same as NMI, for the forwarded key and destination |
| Stripe | An `sk_test_` or `rk_test_` key, whose account is the declared `acct_…` and whose balance reads `livemode: false` |
| Solana | Every RPC endpoint's genesis hash is devnet or testnet |
| CCBill | None exists (DataLink `testMode=1` only selects synthetic reports): mutations are always refused under sandbox |

Under `test_mode: live` the converse holds for Stripe: a test key refuses boot
instead of silently disabling the rail. See
[operating modes](operations.md#operating-modes-the-safety-levers).
