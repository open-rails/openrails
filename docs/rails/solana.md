# Solana rail — merchant setup

> Which flows are supported on this rail, and how well each one is verified:
> [rail certification matrix](certification-matrix.md).

Solana is the self-custody rail: there is no third-party PSP account. You receive
funds directly into a wallet you control, and OpenRails signs recurring pulls with
a merchant key you provide. One-time payments use Solana Pay; recurring
subscriptions use the official on-chain Subscriptions Delegation Program
(`De1egAFMkMWZSN5rYXRj9CAdheBamobVNubTsi9avR44`).

### What you need

- **A receiving wallet you control.** Funds land in this wallet's associated
  token accounts (ATAs).
- **A signer for subscription pulls.** OpenRails must be able to sign
  `transfer_subscription` transactions each billing period. Two modes:
  - `local_keypair` — you provide the wallet's base58 private key as the
    provider-account secret `private_key`. It is held in the merchant secret
    store and loaded into memory only to sign.
  - `vault_transit` — the key lives in HashiCorp Vault Transit under the named
    key; OpenRails sends the serialized transaction message to Vault for signing
    and the private key never leaves Vault.
- **SOL in the signer wallet for gas.** The merchant signer is the fee payer on
  every pull (~5000 lamports each). A built-in monitor warns when the balance
  drops below ~0.05 SOL.
- Optionally, a **Helius RPC API key** (the default RPC provider).

### PSP manifest entry

Declared under `merchants.<slug>.psps.<key>.solana`. Unlike other rails,
`account_id` is **not** declared — the provider-account identity is always
derived from the signer's public key (a declared value is ignored with a warning).

```yaml
psps:
  solana:
    solana:
      signer: { mode: local_keypair }
      settings:
        # Optional destination wallet; defaults to the signer public key.
        recipient_wallet: 9hSR6S7WPtxmTojgo6GG3k4yDPecgJY292j7xrsUGWBu
        rpc_provider: helius     # helius | public; empty defaults to helius
        rpc_api_key: replace-with-helius-api-key   # forbidden with rpc_provider: public
        tokens:                  # accepted tokens: SYMBOL -> { name, mint }
                                 # decimals are NOT configurable: they are read from the
                                 # SPL mint on-chain, which is the source of truth.
                                 # A `decimals:` key here is rejected.
          SOL:  { name: Solana,   mint: So11111111111111111111111111111111111111112 }
          USDC: { name: USD Coin, mint: EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v }
      secrets:
        private_key: replace-with-base58-private-key   # local_keypair mode only

  # Alternative: Vault Transit signer — no private_key secret at all.
  solana-vault:
    solana:
      signer: { mode: vault_transit, key: openrails-solana-<slug> }
```

`settings` keys are strictly validated: a typo'd key fails the manifest push
loudly. Known keys: `rpc_provider`, `rpc_api_key`, `tokens`, `recipient_wallet`.

### One-time payments (buyer's view)

Prices are defined in fiat; the token amount is quoted at checkout from live
token pricing (quotes carry an expiry). `GET /v1/solana/tokens` (public) lists
the accepted tokens with current pricing.

1. `POST /v1/checkout` with `payment: { rail: "solana", token_symbol: "USDC" }`
   and optionally `flow`:
   - `transfer_request` (default) — the session returns a Solana Pay URL with a
     unique reference key; any wallet can pay it (QR-scannable).
   - `transaction_request` — the session returns a `solana_pay_url`; the wallet
     POSTs its account there and receives a server-built transaction to sign.
2. The buyer pays from their own wallet — no card, no stored credentials.
3. `POST /v1/me/checkout/:id/confirm` with
   `payment: { rail: "solana", signature: "...", wallet?: "..." }` verifies the
   transaction on-chain and grants access.

### Recurring subscriptions

Recurring billing runs on the Subscriptions Delegation Program (`De1eg…`), a
native on-chain program. The buyer signs (first time: an
`init_subscription_authority` transaction, then) a `subscribe` transaction that
delegates a spending allowance on their token account. OpenRails then pulls one
plan-amount per period via `transfer_subscription` — the merchant signer signs
and pays gas; funds move from the subscriber's ATA to the merchant's ATA.

- Catalog prices bill in `currency: usd`. Declaring `psps: [solana]` creates or
  reattaches a USDC plan by default. Use `psp_links.solana.token: USD1` to select
  USD1 instead, or supply `plan_pda` to attach an existing plan and resolve its
  configured token from the on-chain mint:

  ```yaml
  prices:
    - currency: usd
      unit_amount: 23_000_000
      duration: 30d
      auto_renew: true
      psps: [solana]
      # Optional:
      # psp_links:
      #   solana: {token: USD1}
      # Or attach an existing plan:
      #   solana: {plan_pda: 7Xy...PdA}
  ```

- `currency` is the price and ledger denomination; stablecoins remain Solana
  payment assets, not billing currencies. A recurring price has one immutable
  Solana Plan and therefore one token. `token` selects creation of a new Plan;
  `plan_pda` instead attaches an existing Plan and must be supplied without
  `token`, because its mint is authoritative on-chain. `mint_symbol` is stored
  snapshot metadata and is not manifest input.

- **Stablecoins only**: recurring plans are limited to an allowlist (currently
  `USDC`, `USD1`). On-chain plan amounts are immutable, so only a stablecoin
  keeps a fixed base-unit amount ≈ a fixed fiat amount. One-off purchases are
  unaffected and accept the full configured token set.
- **One pull per period, enforced twice.** The on-chain program independently
  caps pulls at one plan-amount per period (a second pull fails with
  `Custom:400` "period already paid"), so even a buggy biller cannot
  over-collect.
- **Missed periods are never back-billed.** If pulls were paused (outage,
  limited mode) across whole periods, resuming produces exactly ONE pull per
  subscription and the new period anchors at the pull moment. Entitlement lapsed
  with the unpaid period, so the subscriber pays one period and receives one.
- **Insufficient token balance** → the pull reverts atomically (no partial
  charge) and the subscription routes to the normal dunning state machine, with
  retries spaced relative to the billing cadence.
- **Delegate revoked** (the subscriber revokes the SPL token delegation in their
  wallet) → pulls can never succeed again; OpenRails treats it as terminal and
  cancels the subscription. No dunning.
- **RPC/gas failures** are operational: retried next run, never held against the
  subscriber.
- **Cancels are immediate and on-chain** (the user signs a
  `cancel_subscription`); there is no card-style "cancel at period end"
  deferral on this rail.

### Devnet testing

The network is derived structurally from the deployment's `test_mode`:
sandbox → **devnet**, live → mainnet. There is no independent network knob, and
a PSP declares no environment of its own (#882).

To exercise the flows on devnet:

- Fund the signer wallet with SOL for gas: `solana airdrop 1 <addr> --url devnet`.
- For recurring, the subscriber wallet needs **real devnet USDC** (e.g. the
  Circle faucet at faucet.circle.com) plus a little SOL — the recurring
  allowlist resolves the configured USDC mint, so a self-minted token won't work.
- Browser testing: point a wallet extension (Phantom/Backpack) at Devnet; the
  checkout returns `next_action: solana_sign_transactions` and the wallet
  approves each transaction (first-timer: init then subscribe).

Devnet money is fake: under test_mode the token-price provider falls back to
$1.00 parity when live pricing is unavailable, so devnet never requires a price
feed.

### Operational notes

- Recurring pulls are system-initiated provider writes: under
  `provider_write_mode: limited` or `readonly` the pull worker skips its runs
  entirely. When you return to `full`, each subscription gets exactly one pull
  (see "missed periods" above) — there is no catch-up billing.
- Keep the signer wallet topped up with SOL; the gas monitor warns below
  ~0.05 SOL (~10k pulls of runway). A pull that fails for lack of gas is
  retried, not dunned.
- Every pull is confirmed on-chain before being treated as success, and the
  transaction signature is durably recorded before submission, so a crash
  mid-pull is resolved by reading the chain — never by blindly re-charging.
