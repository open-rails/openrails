# Solana rail — merchant setup

> Which flows are supported on this rail, and how well each one is verified:
> [rail certification matrix](certification-matrix.md).

Solana is the self-custody rail: there is no third-party PSP account. Payments
land directly in a wallet the merchant controls, its **treasury**. One-time
payments use Solana Pay; recurring subscriptions use the official on-chain
Subscriptions Delegation Program (`De1egAFMkMWZSN5rYXRj9CAdheBamobVNubTsi9avR44`).

### Treasury and crankers

Two kinds of key, owned by different parties:

- **Treasury** (the merchant's): the address every payment lands in, and the
  Solana PSP's identity. OpenRails never holds its key.
- **Crankers** (the deployment's): keys that publish the merchants' recurring
  plans, pull each renewal and pay the fees. They never receive money: a
  plan's only destination is the treasury it was published with, and the
  program refuses a pull into anything else.

So a SaaS operator runs its crankers for all its merchants, and each merchant
brings only a treasury address. Without a cranker, Solana rebilling is off:
one-time payments still work, recurring prices are not offered on Solana, and
`catalog apply` refuses a recurring Solana price, saying why.

### PSP manifest entry

Declared under `merchants.<slug>.psps.<key>` with `rail: solana`:

```yaml
psps:
  solana:
    rail: solana
    environment: devnet          # mainnet (default) | devnet | testnet
    treasury: 9hSR6S7WPtxmTojgo6GG3k4yDPecgJY292j7xrsUGWBu   # required
    secrets:
      # Optional: your RPC provider's endpoint, tried before the network's
      # public one. It carries the provider's key, so it is a secret.
      rpc_url: https://devnet.helius-rpc.com/?api-key=replace-with-helius-api-key
```

Those are all its fields; any other is refused. The environment is the
network. Every endpoint the PSP uses, its `rpc_url` and the network's public
endpoint, must report that network's genesis hash before the PSP arms; one on
another network disarms it with a message naming the endpoint. The `rpc_url`
never appears in a log, an error or a browser document. It must be https.
`provider_sandbox.solana_rpc_url` replaces the public endpoint, for tests.

### Crankers

The crankers are deployment configuration: `solana.crankers` in the
standalone server's `config.yaml` (or `SOLANA_CRANKERS`, a JSON list, from the
environment or a mounted secret file), or `Config.Solana` for an embedded
engine. They are an ordered list:

```yaml
solana:
  crankers:
    - mode: vault_transit        # the key never leaves Vault Transit (preferred)
      key: openrails-solana-cranker
    - mode: local_keypair
      private_key: replace-with-base58-keypair   # a secret
```

- The **first** cranker publishes new plans. A plan's owner is fixed on-chain.
- **Every** listed cranker pulls the renewals of the plans it owns. To move to
  a new key, list it first and keep the old one listed while its plans have
  subscribers.
- A plan whose owner no listed cranker holds cannot be pulled: its renewals
  fail with that reason and are never held against the subscriber, and the
  `openrails_solana_crankers` probe warns with the subscriptions and plans
  affected. A lost cranker key loses those renewals.
- Each cranker is pinned to the public key it first had. A Vault Transit key
  rotated since is refused, never silently used as another cranker; restore
  the key, or create a new Transit key and list it.

Crankers sign on whichever network each Solana PSP names, so each needs SOL
there for fees. The optional `openrails_solana_crankers` probe and the operator
alert raised after a pull a cranker signed warn below 0.05 SOL. They check the publishing cranker before
its first subscription, and older plan owners while their subscriptions remain
active, using each obligation's own merchant and configured PSP network (including archived
PSPs). Unused keys do not need funding merely because they are listed.

A warning names the wallet, merchant, PSP, network and observed balance. Fund
that wallet on that network; funding the treasury does not fund a different
cranker. The threshold is an operational warning, not a guarantee of a number
of future payments. Successful network and balance checks are reused for up to
five minutes, with a fresh check after the target or RPC configuration changes.
A failed check reports an unknown balance, never a zero balance or an expired
healthy result. The next signed pull checks again. These warnings never fail the
engine's mandatory readiness check or alter customer access. There is no
automatic top-up.

Existing subscriptions and pending subscription payments keep their recorded
PSP for transaction preparation, receipt verification, renewals and on-chain
cancellation, including when that PSP is archived. Removing its declaration
leaves its account and original network in the database. Existing billing can
continue through that network's public RPC; keep the old declaration if it
needs a private endpoint. It never switches to the replacement account.
A missing account record, wrong rail or foreign merchant is refused.
Recovery checks any recorded signature before another charge.

A stored Solana PSP's network cannot be changed. In particular, devnet and
testnet share an account identity for the same treasury, so a network edit is
rejected and the original account is preserved. Use a different treasury for
the other test network. Rotating an RPC endpoint on the same network is allowed.
This does not repair network history overwritten before this restriction.

A treasury is frozen into each plan when it is published. Changing a PSP's
treasury applies to one-time payments at once, and the next catalog apply
(even of the same document) publishes new plans for recurring prices, so new
subscriptions pay the new treasury; existing subscriptions keep paying the
treasury their plan was published with.

DEV lists its Transit key `openrails-solana-doujins` (address
`CXFYwCTBtzsF3qS5zZyWqMKYwBVFHUHCrtTiJkAa5c1w`) as its cranker; that address
is also its treasury.

#### Accepted tokens

The accepted set is fixed by network: **SOL and USDC** on mainnet and devnet
(Circle's devnet USDC, `4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU`), **SOL**
on testnet. USDC is the recurring token. `GET /v1/me/config`
(`payment.solana.tokens`) advertises exactly this set. Decimals are read from
the mint on-chain.

### One-time payments (buyer's view)

Prices are defined in fiat; the token amount is quoted at purchase from live
token pricing (quotes carry an expiry). `GET /v1/me/config` lists the
merchant's accepted tokens to its customers in `payment.solana.tokens`.

1. An order of one-time lines lists the Solana PSP among its
   `payment_options` (`accepts: ["solana_pay"]`). Paying it with
   `payment: {token_symbol}` answers `payment.status: requires_action` and
   `next_action: {type: "solana_pay", url}`: a Solana Pay transaction request,
   `solana:<billing base>/v1/orders/{id}/solana-pay` (QR-scannable). Its
   `payload` carries the quote: `token_symbol`, `token_amount`, `recipient`,
   `reference`, `quote_expires_at`.
2. The wallet reads the label (`GET`), then posts its account (`POST`) and
   signs the transaction OpenRails built: the quote's transfer to the treasury,
   stamped with the reference and a memo. One transaction is offered at a time;
   once the quote expired with nothing landed, the wallet's request gets a fresh
   quote.
3. OpenRails watches the reference on chain and settles the order (`complete`)
   in the transaction that records the transfer. `POST /v1/me/orders/{id}/confirm`
   asks for an immediate read.

Transfer requests (a wallet building the transfer from a URL) are not
offered: every transaction is OpenRails', so a transfer without its memo is
not a payment.

### Recurring subscriptions

Recurring billing runs on the Subscriptions Delegation Program (`De1eg…`), a
native on-chain program. The buyer signs (first time: an
`init_subscription_authority` transaction, then) a `subscribe` transaction that
delegates a spending allowance on their token account. OpenRails then pulls one
plan-amount per period via `transfer_subscription`: the plan's cranker signs
and pays gas; funds move from the subscriber's ATA to the treasury's ATA.

A subscription is bought on an order of its one line, paid like a one-time
order: `payment: {token_symbol}` (the plan's token) answers `solana_pay` with
`payload.period_hours`, and the wallet's transaction request is answered with
the subscribe transaction for its account: (`init_subscription_authority`
for a first-timer,) `subscribe`, and a cranker-co-signed pull of the full
first period, stamped with the order's reference.

A subscription activates only from that first payment: signed by the wallet
that asked for it, carrying the reference, subscribing to the plan's terms and
pulling the full first period into the treasury's ATA, landed within the
offer's validity. OpenRails finds it on the reference, or reads it at once
when `POST /v1/me/orders/{id}/confirm` names its `signature`; the order
completes with the subscription on its line. A signature settles one order
only, across every merchant and PSP.

- Solana recurring prices are denominated in USD, such as `amount: 23 USD`.
  When the merchant has a Solana PSP, `catalog apply` publishes the price's
  USDC plan, owned by the first cranker and paying the merchant's treasury.
  `sell_via` can restrict a price to Solana; omitted, it also sells by card
  through the merchant's card vault and charge PSPs. A one-time price needs no plan:

  ```yaml
  prices:
    premium-monthly:
      amount: 23 USD
      access_duration: 30 days
      billing_interval: 30 days
      sell_via: [solana]
  ```

- `currency` is the price and ledger denomination, fiat or SOL; stablecoins
  are Solana payment assets, and a price in one is refused. A recurring price
  has one immutable Solana plan. `mint_symbol` is stored snapshot metadata and
  is not manifest input.

- **Publishing on apply.** The plan is published before anything local is
  written, so a failed publish leaves the catalog unchanged. Its id derives
  from the price and mint: re-applying, or applying again after a failed
  commit, reattaches the same plan, and a plan already on chain with other
  terms, or paying another treasury, is refused. Plans are immutable, so new
  terms are a new price with its own plan; the old plan stays bound to the
  archived price its subscribers hold. Publishing is a catalog provider write:
  under `provider_write_mode` limited or readonly, apply binds a plan already
  on chain and otherwise refuses the price, naming it and the mode.

- **USDC only**: on-chain plan amounts are immutable, so only a stablecoin
  keeps a fixed base-unit amount ≈ a fixed fiat amount. One-time purchases
  also accept SOL. Testnet has no USDC, so it offers no recurring.
- **OpenRails owns collection.** Solana subscriptions use `collection_policy:
  engine`; only the specialized cranker collects them. The generic saved-card
  collector never admits their renewals. Upgrades correct the former provider
  classification while retaining existing rail identifiers, paid access and
  historical settled bill snapshots.
- **An accepted episode and its realized service period are separate facts.**
  Before signing, the durable pull intent freezes the customer and subscription,
  price/product terms, amount and currency precision, mint and precision,
  treasury, plan/accounts, duration and due episode. The exact signed execution
  identity, signature and submission time are recorded before sending. A
  confirmed transaction must prove the expected program pull and intent memo,
  exact token credit and destination. Its chain block time anchors the realized
  paid period. Completion and restart repair use the same accepted episode to
  record one engine-owned order, actual payment and canonical order attempt,
  and grant that one period. Later catalog edits and verifier clocks cannot
  reprice or extend it. Missing or mismatched evidence stays unresolved.
- **Each attempt freezes its terms when it executes.** An attempt with nothing
  signed, or whose signed transaction reverted, freezes its agreement from the
  subscription as it stands, a due scheduled change applied, under the
  subscription lock and its claim. A signed pull without retained terms keeps
  its signature for evidence-based recovery; current catalog data cannot
  supply its missing historical agreement.
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
- **Each period's pull is its own operation**, scheduled by the write that
  makes it due (enrollment, a renewal, a declined pull's retry, a resume) one
  minute after the period end: the chain's clock may lag. A pull the chain
  refuses because its clock has not reached the period end is retried once it
  has, never taken as a period already paid.
- **RPC/gas failures** are operational: the pull's operation retries on its own
  backoff, never held against the subscriber. An episode whose operation ended
  without advancing the subscription (a period already paid on chain) waits
  for the operator its finding names.
- **Cancellation is pending until the paid period ends and needs no wallet.**
  `POST /v1/me/subscriptions/{id}/cancel` records it immediately and stops the
  cranker from pulling another renewal. Paid access continues until the period
  ends; `/v1/me/subscriptions/{id}/resume` undoes an ordinary customer cancel
  before then. The subscription account can remain on chain: no pull is made.
  A renewal already signed and unresolved must settle before cancellation can
  be accepted (`payment_in_progress`).
- **An optional on-chain cancellation is signed by the subscriber.** Send
  `revoke_on_chain: true` on the cancel request to receive a
  `solana_sign_transactions` next action. The local cancellation has already
  taken effect; the wallet signs and sends the additional on-chain cancel and
  repeats the request with `signature`. Wallet cancellations and delegate
  revocations are also detected when a due pull is refused, without dunning.
  They cannot be undone locally.
- **Tier changes are signed by the subscriber.** The change route answers a
  `solana_sign_transactions` next action; repeat with the landed signature.
  A merchant cannot sign this action for the customer.

### Devnet testing

The network is the PSP's `environment`; a test PSP declares `devnet`:

```yaml
      solana:
        rail: solana
        environment: devnet
        treasury: CXFYwCTBtzsF3qS5zZyWqMKYwBVFHUHCrtTiJkAa5c1w
```

To exercise the flows on devnet:

- Fund the crankers with SOL for gas: `solana airdrop 1 <addr> --url devnet`.
- For recurring, the subscriber wallet needs **devnet USDC** (the Circle
  faucet at faucet.circle.com) plus a little SOL.
- Browser testing: point a wallet extension (Phantom/Backpack) at Devnet; a
  subscription's order answers `next_action: solana_pay` and the wallet
  approves one transaction (a first-timer's folds
  `initialize_subscription_authority` in front of subscribe + first pull, using
  the program's `UNKNOWN_INIT_ID` same-slot check).

Devnet and testnet money is fake: their token prices fall back to $1.00 parity
when live pricing is unavailable, so they never require a price feed.

### Operational notes

- Recurring pulls are system-initiated provider writes: under
  `provider_write_mode: limited` or `readonly` a merchant's pulls park and are
  re-checked every few minutes. When you return to `full`, each subscription
  gets exactly one pull (see "missed periods" above) — there is no catch-up
  billing. Without a cranker the pulls park.
- Keep the crankers topped up with SOL; the gas monitor warns below
  ~0.05 SOL (~10k pulls of runway). A pull that fails for lack of gas is
  retried, not dunned.
- Every pull is confirmed on-chain before being treated as success, and the
  transaction signature is durably recorded before submission, so a crash
  mid-pull is resolved by reading the chain — never by blindly re-charging.
