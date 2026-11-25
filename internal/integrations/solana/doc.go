// Package solana is the self-custody rail: RPC, Solana Pay settlement, plan
// publishing and subscription pulls on the Subscriptions Delegation Program
// (package subscriptions). There is no third-party account. A Solana PSP is
// a treasury address the merchant controls, on the network its environment
// names (mainnet, devnet, testnet); every endpoint, its secret rpc_url and the
// public one, must report that network's genesis hash before the PSP arms. A
// stored PSP's network never changes.
//
// Crankers are the deployment's keys (Config.Solana, solana.crankers):
// vault_transit (preferred) or local_keypair. The first publishes new plans;
// every listed cranker pulls the plans it owns and pays the fees, and each is
// pinned to its first public key. A plan whose owner no listed cranker holds
// cannot be pulled; openrails_solana_crankers and life.solana.cranker_low_gas
// warn about missing owners and balances below 0.05 SOL. Without a cranker,
// recurring Solana prices are refused at catalog apply and pulls park.
//
// Tokens are fixed by network: SOL and USDC on mainnet and devnet, SOL on
// testnet. Recurring plans are USDC, priced in USD, immutable and published on
// catalog apply before anything local is written; a plan's treasury is frozen
// into it. Collection is engine-owned (collection_policy: engine) and only the
// specialized cranker collects it.
//
// A pull is one operation per period, scheduled one minute after the period
// end. Before signing it freezes the agreement; the signature and its
// blockhash's last valid height are recorded before sending, and it is signed
// again only once the chain provably never landed it. A finalized pull must
// prove the program call, memo, token credit and destination; its block time
// anchors the paid period. Missed periods are never back-billed: a resume
// pulls once. Insufficient balance dunns; a revoked delegate cancels; RPC and
// gas failures retry without touching the subscriber. Cancellation is local
// and needs no wallet (payment_in_progress while a signed pull is unresolved);
// an on-chain cancel and tier changes are signed by the subscriber.
//
// Solana Pay: each attempt has one reference (solana_pay_references). Every
// replica's poller walks its finalized signature history from a cursor; each
// signature settles once into solana_pay_receipts. A Token-2022 fee is priced
// in at quote time and mints with a transfer hook are refused. The first
// transfer of at least the quote by expiry plus 30 minutes is credited
// (excess flagged overpaid); anything else is recorded for review with
// consistency.ledger.unbooked and never refunded automatically: close it with
// `openrails solana-pay resolve`. Open reviews and pending references refuse
// the billing archive; settled references are deleted after 7 days.
package solana
