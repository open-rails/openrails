//go:build devnet

// Shared scaffolding for the recurring DEVNET integration tests (real USDC).
//
// The user prefers a FEW real on-chain integration tests over many mock/unit
// tests. All the funding/ATA/sign-send plumbing every devnet test needs lives
// here ONCE so the lifecycle / tier-change / failure / multi-sub / sustained
// suites stay small and read like the production flow they exercise.
//
// THE KEY HELPER is devnetSubscribe: it runs the FULL production subscribe —
// prepare -> (init, user-signed) -> re-prepare -> the ATOMIC co-signed
// [subscribe+transfer] step -> completePartialAndSend. The atomic subscribe step
// (#286) returns a PARTIALLY-signed tx: the cranker has pre-signed the transfer
// slot, and the wallet must complete ONLY its fee-payer slot WITHOUT clearing the
// cranker signature. The old hand-rolled loops sent the subscribe step through
// signAndSendBase64, which does tx.Signatures = nil — that DROPS the cranker
// signature and the on-chain [subscribe+transfer] would fail (the transfer's
// cranker-Caller slot would be unsigned). Routing every test through this one
// helper fixes that in a single place. It also means: after devnetSubscribe, the
// FIRST period is ALREADY PAID (it was pulled inside the atomic subscribe tx), so
// the first SEPARATE crank a test issues is period 2 and needs the ~1h rollover.
//
// Common prereqs for every devnet test:
//   - SOLANA_DEVNET_PAYER_KEY    : SOL-funded; doubles as the merchant/cranker key.
//   - SOLANA_DEVNET_SUBSCRIBER_KEY: holds faucet USDC (https://faucet.circle.com,
//     Solana devnet); FUNDS a FRESH subscriber each run so SubscriptionAuthority
//     state never accumulates.
//   - HELIUS_API_KEY             : devnet RPC endpoint.
package recurring

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"testing"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/programs/system"
	"github.com/gagliardetto/solana-go/programs/token"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/google/uuid"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/integrations/solana/subscriptions"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

// devnetUSDCMintStr is the devnet USDC mint (must match config DefaultDevnetTokens).
const devnetUSDCMintStr = "4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU"

// newDevnetPlanService builds a PlanService wired with the devnet token map, the
// same way production wiring supplies runtime-configured tokens. Without it the
// service has no USDC mint and PublishPlan fails with "no configured mint".
func newDevnetPlanService(submitter Submitter) *PlanService {
	return NewPlanService(submitter, "devnet", config.TokensForNetwork("devnet"))
}

// newDevnetPrepareSubscribeService / newDevnetPrepareTierChangeService likewise
// wire the devnet token map; both resolve the USDC mint via the configured
// tokens, so an empty map fails with "no configured mint".
func newDevnetPrepareSubscribeService(submitter Submitter, signer solanaint.Signer, rc *solanaint.RPCClient) *PrepareSubscribeService {
	return NewPrepareSubscribeService(submitter, signer, rc, "devnet", config.TokensForNetwork("devnet"))
}

func newDevnetPrepareTierChangeService(signer solanaint.Signer, rc *solanaint.RPCClient) *PrepareTierChangeService {
	return NewPrepareTierChangeService(signer, rc, "devnet", config.TokensForNetwork("devnet"))
}

// devnetEnv returns the named env var or skips the test when it is unset.
func devnetEnv(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Skipf("%s not set; skipping devnet integration test", name)
	}
	return v
}

// devnetClients builds the production RPC client + a raw client from HELIUS_API_KEY.
func devnetClients(t *testing.T) (*solanaint.RPCClient, *rpc.Client) {
	t.Helper()
	endpoint := "https://devnet.helius-rpc.com/?api-key=" + devnetEnv(t, "HELIUS_API_KEY")
	rc := solanaint.NewRPCClientWithConfig(solanaint.RPCClientConfig{Endpoint: endpoint, Network: "devnet"})
	return rc, rpc.New(endpoint)
}

// devnetKeys loads the merchant (payer/cranker) + funder keypairs from the env.
func devnetKeys(t *testing.T) (merchant, funder solanago.PrivateKey) {
	t.Helper()
	var err error
	merchant, err = solanago.PrivateKeyFromBase58(devnetEnv(t, "SOLANA_DEVNET_PAYER_KEY"))
	require.NoError(t, err)
	funder, err = solanago.PrivateKeyFromBase58(devnetEnv(t, "SOLANA_DEVNET_SUBSCRIBER_KEY"))
	require.NoError(t, err)
	return merchant, funder
}

// requireFunderUSDC skips unless the funder holds at least `need` USDC base units.
func requireFunderUSDC(ctx context.Context, t *testing.T, rc *solanaint.RPCClient, funder solanago.PrivateKey, usdc solanago.PublicKey, need uint64) {
	t.Helper()
	have, err := rc.GetTokenBalanceForMint(ctx, funder.PublicKey(), usdc)
	if err != nil || have < need {
		t.Skipf("funder %s has %d USDC base units (<%d needed) — fund via https://faucet.circle.com (Solana devnet, USDC) then re-run",
			funder.PublicKey(), have, need)
	}
	t.Logf("funder USDC balance: %d base units (need %d)", have, need)
}

// freshFundedSubscriber mints a random subscriber, funds its SOL gas + USDC ATA,
// and transfers `usdcFund` USDC into it from the funder. A fresh wallet each run
// keeps SubscriptionAuthority state from accumulating across runs.
func freshFundedSubscriber(ctx context.Context, t *testing.T, rc *solanaint.RPCClient, raw *rpc.Client, merchant, funder solanago.PrivateKey, usdc solanago.PublicKey, solLamports, usdcFund uint64) solanago.PrivateKey {
	t.Helper()
	sub, err := solanago.NewRandomPrivateKey()
	require.NoError(t, err)
	t.Logf("fresh subscriber: %s (funded %d USDC + %d lamports SOL)", sub.PublicKey(), usdcFund, solLamports)
	fundSOL(ctx, t, rc, raw, merchant, sub.PublicKey(), solLamports)
	ensureATA(ctx, t, rc, raw, merchant, sub.PublicKey(), usdc)
	if usdcFund > 0 {
		transferUSDC(ctx, t, rc, raw, funder, sub.PublicKey(), usdc, usdcFund)
	}
	return sub
}

// subscriptionRowFromHandle materialises the SolanaSubscription row the crank +
// cancel services consume, deriving every PDA from the plan handle + wallet.
func subscriptionRowFromHandle(h *PlanHandle, subscriberWallet string) *models.SolanaSubscription {
	merchant := solanago.MustPublicKeyFromBase58(h.MerchantAddress)
	mint := solanago.MustPublicKeyFromBase58(h.Mint)
	subscriber := solanago.MustPublicKeyFromBase58(subscriberWallet)
	planPDA := solanago.MustPublicKeyFromBase58(h.PlanPDA)
	subPDA, _, _ := subscriptions.DeriveSubscriptionPDA(planPDA, subscriber)
	saPDA, _, _ := subscriptions.DeriveSubscriptionAuthority(subscriber, mint)
	return &models.SolanaSubscription{
		ID: uuid.New(), SubscriptionID: uuid.New(),
		MerchantAddress: merchant.String(), Mint: mint.String(), SubscriberWallet: subscriber.String(),
		PlanPDA: planPDA.String(), SubscriptionPDA: subPDA.String(), AuthorityPDA: saPDA.String(),
	}
}

// fixedSubReader serves a single subscription row to PrepareCancelService.
type fixedSubReader struct{ row *models.SolanaSubscription }

func (f fixedSubReader) GetBySubscriptionID(context.Context, uuid.UUID) (*models.SolanaSubscription, error) {
	return f.row, nil
}

// memSecretGetter serves the merchant (cranker) key as the tenant's
// solana/private_key so the production keypair signer / Submitter resolve it.
type memSecretGetter struct{ key string }

func (m memSecretGetter) GetSecret(_ context.Context, _ merchant.ID, name string) (string, error) {
	return m.key, nil
}

// newMerchantSigner builds the production keypair signer backed by the merchant
// (cranker) key — the same key the Submitter resolves to co-sign atomic bundles.
func newMerchantSigner(merchant solanago.PrivateKey) solanaint.Signer {
	return solanaint.NewKeypairSigner(memSecretGetter{key: merchant.String()}, 0)
}

// publishUSDCPlan publishes a USDC plan (amount / periodHours) on-chain via the
// real PlanService and returns its handle.
func publishUSDCPlan(ctx context.Context, t *testing.T, planSvc *PlanService, amount, periodHours uint64) *PlanHandle {
	t.Helper()
	h, err := planSvc.PublishPlan(ctx, PublishPlanInput{
		MerchantID: dbtest.TestTenantID, PlanID: uint64(time.Now().UnixNano()),
		TokenSymbol: "USDC", AmountBaseUnits: amount, PeriodHours: periodHours,
	})
	require.NoError(t, err, "PublishPlan(%d/%dh) should create the plan on-chain", amount, periodHours)
	return h
}

// devnetSubscribe runs the FULL production subscribe for `sub` to plan `h`:
//
//	prepare -> if Step=="init": signAndSendBase64 (user-only) -> re-prepare
//	        -> Step=="subscribe": the ATOMIC co-signed [subscribe+transfer]
//	        -> completePartialAndSend (wallet completes ONLY its fee-payer slot,
//	           preserving the cranker's pre-signature).
//
// On return the subscription exists on-chain AND the FIRST period has already
// been pulled (atomically, inside the subscribe tx). It returns the crank row so
// callers can crank later periods / cancel. This is the ONE place the co-signed
// subscribe is handled correctly — every devnet test goes through it.
func devnetSubscribe(ctx context.Context, t *testing.T, rc *solanaint.RPCClient, raw *rpc.Client, prepSvc *PrepareSubscribeService, sub solanago.PrivateKey, h *PlanHandle, amount, periodHours uint64) *models.SolanaSubscription {
	t.Helper()
	prepare := func() *PrepareSubscribeResult {
		res, err := prepSvc.Prepare(ctx, PrepareSubscribeInput{
			MerchantID: dbtest.TestTenantID, SubscriberWallet: sub.PublicKey().String(),
			PlanID: h.PlanID, MintSymbol: "USDC", AmountBaseUnits: amount, PeriodHours: periodHours, PlanCreatedAt: h.CreatedAt,
		})
		require.NoError(t, err)
		return res
	}

	res := prepare()
	if res.Step == "init" {
		// init_subscription_authority is user-only signed.
		signAndSendBase64(ctx, t, rc, raw, sub, res.Transactions)
		res = prepare() // re-prepare: the authority now exists -> subscribe step.
	}
	require.Equal(t, "subscribe", res.Step, "after init the step must be subscribe")
	require.Len(t, res.Transactions, 1, "atomic subscribe is a single co-signed tx")
	// The atomic subscribe is partially signed (cranker slot done); the wallet
	// completes its fee-payer slot WITHOUT dropping the cranker signature.
	completePartialAndSend(ctx, t, rc, raw, sub, res.Transactions[0])
	t.Logf("subscribed (atomic [subscribe+transfer]) to plan %s — first period already pulled", h.PlanPDA)
	return subscriptionRowFromHandle(h, sub.PublicKey().String())
}

// ---- low-level on-chain plumbing (funding, ATAs, signing) ----

// ensureATA creates the canonical associated token account for (owner, mint) if
// missing (CreateIdempotent), paid + signed by `payer`.
func ensureATA(ctx context.Context, t *testing.T, rc *solanaint.RPCClient, raw *rpc.Client, payer solanago.PrivateKey, owner, mint solanago.PublicKey) {
	t.Helper()
	ata, _, err := subscriptions.DeriveATA(owner, mint, solanago.TokenProgramID)
	require.NoError(t, err)
	ix := solanago.NewInstruction(
		subscriptions.AssociatedTokenProgramID,
		solanago.AccountMetaSlice{
			solanago.NewAccountMeta(payer.PublicKey(), true, true),
			solanago.NewAccountMeta(ata, true, false),
			solanago.NewAccountMeta(owner, false, false),
			solanago.NewAccountMeta(mint, false, false),
			solanago.NewAccountMeta(solanago.SystemProgramID, false, false),
			solanago.NewAccountMeta(solanago.TokenProgramID, false, false),
		},
		[]byte{1}, // CreateIdempotent
	)
	bh, err := rc.GetLatestBlockhash(ctx)
	require.NoError(t, err)
	tx, err := solanago.NewTransaction([]solanago.Instruction{ix}, bh, solanago.TransactionPayer(payer.PublicKey()))
	require.NoError(t, err)
	_, err = tx.Sign(signerFor(payer))
	require.NoError(t, err)
	sig, err := raw.SendTransactionWithOpts(ctx, tx, rpc.TransactionOpts{SkipPreflight: true})
	require.NoError(t, err)
	_, _ = rc.WatchTransaction(ctx, sig, rpc.CommitmentConfirmed, 60*time.Second)
}

// transferUSDC moves `amount` of `mint` from the funder's ATA to the recipient's
// ATA (both canonical), signed by the funder.
func transferUSDC(ctx context.Context, t *testing.T, rc *solanaint.RPCClient, raw *rpc.Client, funder solanago.PrivateKey, to, mint solanago.PublicKey, amount uint64) {
	t.Helper()
	src, _, err := subscriptions.DeriveATA(funder.PublicKey(), mint, solanago.TokenProgramID)
	require.NoError(t, err)
	dst, _, err := subscriptions.DeriveATA(to, mint, solanago.TokenProgramID)
	require.NoError(t, err)
	ix := token.NewTransferInstruction(amount, src, dst, funder.PublicKey(), nil).Build()
	bh, err := rc.GetLatestBlockhash(ctx)
	require.NoError(t, err)
	tx, err := solanago.NewTransaction([]solanago.Instruction{ix}, bh, solanago.TransactionPayer(funder.PublicKey()))
	require.NoError(t, err)
	_, err = tx.Sign(signerFor(funder))
	require.NoError(t, err)
	sig, err := raw.SendTransactionWithOpts(ctx, tx, rpc.TransactionOpts{SkipPreflight: true})
	require.NoError(t, err)
	out, werr := rc.WatchTransaction(ctx, sig, rpc.CommitmentConfirmed, 60*time.Second)
	require.NoError(t, werr)
	require.Nil(t, out.OnChainError(), "USDC funding transfer failed")
}

// fundSOL transfers `lamports` SOL from `from` to `to`.
func fundSOL(ctx context.Context, t *testing.T, rc *solanaint.RPCClient, raw *rpc.Client, from solanago.PrivateKey, to solanago.PublicKey, lamports uint64) {
	t.Helper()
	bh, err := rc.GetLatestBlockhash(ctx)
	require.NoError(t, err)
	tx, err := solanago.NewTransaction([]solanago.Instruction{
		system.NewTransferInstruction(lamports, from.PublicKey(), to).Build(),
	}, bh, solanago.TransactionPayer(from.PublicKey()))
	require.NoError(t, err)
	_, err = tx.Sign(signerFor(from))
	require.NoError(t, err)
	sig, err := raw.SendTransactionWithOpts(ctx, tx, rpc.TransactionOpts{SkipPreflight: true})
	require.NoError(t, err)
	_, _ = rc.WatchTransaction(ctx, sig, rpc.CommitmentConfirmed, 60*time.Second)
}

// signerFor returns a single-key Solana signer callback.
func signerFor(key solanago.PrivateKey) func(solanago.PublicKey) *solanago.PrivateKey {
	return func(pk solanago.PublicKey) *solanago.PrivateKey {
		if key.PublicKey().Equals(pk) {
			return &key
		}
		return nil
	}
}

// signAndSendBase64 deserializes each unsigned base64 tx, signs it as the wallet
// (clearing any signatures), refreshes the blockhash, and submits + confirms,
// REQUIRING on-chain success. Use ONLY for single-signer (wallet-only) txs —
// init_subscription_authority, cancel/resume, the unsigned downgrade. The atomic
// subscribe + the co-signed upgrade are partially signed and MUST go through
// completePartialAndSend instead (this clears the cranker's pre-signature).
func signAndSendBase64(ctx context.Context, t *testing.T, rc *solanaint.RPCClient, raw *rpc.Client, wallet solanago.PrivateKey, txns []string) {
	t.Helper()
	for _, b64 := range txns {
		tx := decodeTxBase64(t, b64)
		bh, err := rc.GetLatestBlockhash(ctx)
		require.NoError(t, err)
		tx.Message.RecentBlockhash = bh
		tx.Signatures = nil
		_, err = tx.Sign(signerFor(wallet))
		require.NoError(t, err)
		sig, err := raw.SendTransactionWithOpts(ctx, tx, rpc.TransactionOpts{SkipPreflight: true, PreflightCommitment: rpc.CommitmentConfirmed})
		require.NoError(t, err)
		out, werr := rc.WatchTransaction(ctx, sig, rpc.CommitmentConfirmed, 90*time.Second)
		require.NoError(t, werr)
		require.Nil(t, out.OnChainError(), "tx %s failed on-chain", sig)
	}
}

// trySignSend is the non-fatal sibling of signAndSendBase64: it signs each tx as
// the wallet, submits + confirms, and returns (onchainErr, ok). ok=false means a
// tx reverted; the error string is returned for inspection (used by tests that
// EXPECT a revert, e.g. duplicate-subscribe). Single-signer txs only.
func trySignSend(ctx context.Context, t *testing.T, raw *rpc.Client, rc *solanaint.RPCClient, wallet solanago.PrivateKey, txns []string) (string, bool) {
	t.Helper()
	for _, b64 := range txns {
		tx := decodeTxBase64(t, b64)
		bh, err := rc.GetLatestBlockhash(ctx)
		require.NoError(t, err)
		tx.Message.RecentBlockhash = bh
		tx.Signatures = nil
		_, err = tx.Sign(signerFor(wallet))
		require.NoError(t, err)
		sig, err := raw.SendTransactionWithOpts(ctx, tx, rpc.TransactionOpts{SkipPreflight: true, PreflightCommitment: rpc.CommitmentConfirmed})
		if err != nil {
			return err.Error(), false
		}
		out, werr := rc.WatchTransaction(ctx, sig, rpc.CommitmentConfirmed, 90*time.Second)
		if werr != nil {
			return werr.Error(), false
		}
		if e := out.OnChainError(); e != nil {
			return e.Error(), false
		}
	}
	return "", true
}

// completePartialAndSend takes a PARTIALLY-signed bundle (the cranker slot is
// already signed against its blockhash) and has the wallet sign ONLY the
// fee-payer slot (index 0) WITHOUT touching the message — preserving the
// cranker's signature — then submits (skip-preflight) and confirms. This mirrors
// what the real browser wallet does (signAllTransactions over the exact bytes
// OpenRails returned) and is REQUIRED for the atomic subscribe (#286) and the
// co-signed tier-change upgrade (#272).
func completePartialAndSend(ctx context.Context, t *testing.T, rc *solanaint.RPCClient, raw *rpc.Client, wallet solanago.PrivateKey, b64 string) {
	t.Helper()
	tx := decodeTxBase64(t, b64)
	require.Equal(t, 2, int(tx.Message.Header.NumRequiredSignatures), "co-signed bundle has two signers")
	require.True(t, wallet.PublicKey().Equals(tx.Message.AccountKeys[0]), "wallet must be the fee payer (signer 0)")
	require.True(t, tx.Signatures[0].IsZero(), "fee-payer slot must be empty for the wallet")
	require.False(t, tx.Signatures[1].IsZero(), "cranker slot must already be pre-signed")

	msg, err := tx.Message.MarshalBinary()
	require.NoError(t, err)
	sig, err := wallet.Sign(msg)
	require.NoError(t, err)
	tx.Signatures[0] = sig
	require.NoError(t, tx.VerifySignatures(), "fully + validly signed after the wallet completes it")

	txid, err := raw.SendTransactionWithOpts(ctx, tx, rpc.TransactionOpts{SkipPreflight: true, PreflightCommitment: rpc.CommitmentConfirmed})
	require.NoError(t, err)
	out, werr := rc.WatchTransaction(ctx, txid, rpc.CommitmentConfirmed, 90*time.Second)
	require.NoError(t, werr)
	require.Nil(t, out.OnChainError(), "co-signed tx %s failed on-chain", txid)
}

// decodeTxBase64 deserializes a base64 transaction.
func decodeTxBase64(t *testing.T, b64 string) *solanago.Transaction {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(b64)
	require.NoError(t, err)
	tx, err := solanago.TransactionFromBytes(data)
	require.NoError(t, err)
	return tx
}

// awaitTokenCredit polls the (owner, mint) token balance until it equals `want`,
// failing only if it never reaches it within `timeout`. Devnet RPC is
// load-balanced, so a getTokenAccountBalance issued right after a tx confirms can
// hit a node that still lags the pull by a few seconds; the on-chain pull already
// succeeded — this just absorbs that read-after-confirm lag so a balance
// assertion doesn't flake.
func awaitTokenCredit(ctx context.Context, t *testing.T, rc *solanaint.RPCClient, owner, mint solanago.PublicKey, want uint64, timeout time.Duration, msgf string, args ...any) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last uint64
	for {
		last, _ = rc.GetTokenBalanceForMint(ctx, owner, mint)
		if last == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: balance for %s did not reach %d within %s (last read %d)",
				fmt.Sprintf(msgf, args...), owner, want, timeout, last)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("context expired waiting for token credit: %v", ctx.Err())
		case <-time.After(2 * time.Second):
		}
	}
}
