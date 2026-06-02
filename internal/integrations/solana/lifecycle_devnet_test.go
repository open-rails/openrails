//go:build devnet

// Full on-chain lifecycle integration test for Solana recurring subscriptions
// (issues #254/#255/#256/#257), run live against devnet. It mints a
// self-controlled SPL test token (so we don't depend on a USDC faucet), then
// drives the WHOLE flow against the deployed program and asserts the crank
// actually moves tokens:
//
//	create_plan -> init_subscription_authority -> subscribe -> transfer_subscription
//	(CRANK) -> cancel_subscription -> resume_subscription
//
//	Run: SOLANA_DEVNET_PAYER_KEY=<funded> HELIUS_API_KEY=<key> \
//		go test -tags devnet -run Lifecycle -v -timeout 580s ./internal/integrations/solana/...
package solana

import (
	"context"
	"encoding/binary"
	"strconv"
	"testing"
	"time"

	solanago "github.com/doujins-org/solana-go"
	"github.com/doujins-org/solana-go/programs/system"
	"github.com/doujins-org/solana-go/programs/token"
	"github.com/doujins-org/solana-go/rpc"
	"github.com/open-rails/openrails/internal/integrations/solana/subscriptions"
	"github.com/stretchr/testify/require"
)

// dnRaw is the raw devnet RPC client (set by the test) used to submit with
// SkipPreflight — the public RPC's preflight simulation runs against a bank that
// lags just-confirmed txns, so any tx depending on a freshly-created account
// fails simulation even though it executes fine on-chain.
var dnRaw *rpc.Client

// signAndSend builds + signs a tx and submits it WITHOUT preflight, then waits for
// confirmation and asserts on-chain success (a skipped-preflight tx that fails
// still gets confirmed, so we must check the status error).
func signAndSend(ctx context.Context, t *testing.T, rc *RPCClient, feePayer solanago.PrivateKey, signers []solanago.PrivateKey, instrs ...solanago.Instruction) solanago.Signature {
	t.Helper()
	bh, err := rc.GetLatestBlockhash(ctx)
	require.NoError(t, err)
	tx, err := solanago.NewTransaction(instrs, bh, solanago.TransactionPayer(feePayer.PublicKey()))
	require.NoError(t, err)
	all := append([]solanago.PrivateKey{feePayer}, signers...)
	_, err = tx.Sign(func(pk solanago.PublicKey) *solanago.PrivateKey {
		for i := range all {
			if all[i].PublicKey().Equals(pk) {
				return &all[i]
			}
		}
		return nil
	})
	require.NoError(t, err)
	sig, err := dnRaw.SendTransactionWithOpts(ctx, tx, rpc.TransactionOpts{SkipPreflight: true, PreflightCommitment: rpc.CommitmentConfirmed})
	require.NoError(t, err)
	for i := 0; i < 45; i++ {
		st, e := dnRaw.GetSignatureStatuses(ctx, true, sig)
		if e == nil && len(st.Value) > 0 && st.Value[0] != nil {
			require.Nil(t, st.Value[0].Err, "tx %s failed on-chain", sig)
			cs := st.Value[0].ConfirmationStatus
			if cs == rpc.ConfirmationStatusConfirmed || cs == rpc.ConfirmationStatusFinalized {
				return sig
			}
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("tx %s not confirmed in time", sig)
	return sig
}

// createATA creates the CANONICAL associated token account for (owner, mint) —
// the Subscriptions program requires userAta to be the derived ATA (error 108
// otherwise). Idempotent CreateIdempotent with the modern 6-account layout.
func createATA(ctx context.Context, t *testing.T, rc *RPCClient, payer solanago.PrivateKey, owner, mint solanago.PublicKey) solanago.PublicKey {
	t.Helper()
	ataAddr, _, err := subscriptions.DeriveATA(owner, mint, token.ProgramID)
	require.NoError(t, err)
	ix := solanago.NewInstruction(
		subscriptions.AssociatedTokenProgramID,
		solanago.AccountMetaSlice{
			solanago.NewAccountMeta(payer.PublicKey(), true, true),
			solanago.NewAccountMeta(ataAddr, true, false),
			solanago.NewAccountMeta(owner, false, false),
			solanago.NewAccountMeta(mint, false, false),
			solanago.NewAccountMeta(solanago.SystemProgramID, false, false),
			solanago.NewAccountMeta(token.ProgramID, false, false),
		},
		[]byte{1}, // CreateIdempotent
	)
	signAndSend(ctx, t, rc, payer, nil, ix)
	return ataAddr
}

// accountData fetches an account's data at CONFIRMED commitment, retrying past
// the read-after-write lag (reads can briefly race a just-confirmed write).
func accountData(ctx context.Context, t *testing.T, raw *rpc.Client, acct solanago.PublicKey) []byte {
	t.Helper()
	for i := 0; i < 25; i++ {
		ai, e := raw.GetAccountInfoWithOpts(ctx, acct, &rpc.GetAccountInfoOpts{Commitment: rpc.CommitmentConfirmed})
		if e == nil && ai != nil && ai.Value != nil {
			return ai.Value.Data.GetBinary()
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("account %s not visible in time", acct)
	return nil
}

// readI64 reads a little-endian int64 at byteOffset from an account's data.
func readI64(ctx context.Context, t *testing.T, raw *rpc.Client, acct solanago.PublicKey, byteOffset int) int64 {
	t.Helper()
	data := accountData(ctx, t, raw, acct)
	require.GreaterOrEqual(t, len(data), byteOffset+8)
	return int64(binary.LittleEndian.Uint64(data[byteOffset : byteOffset+8]))
}

func tokenBalance(ctx context.Context, t *testing.T, raw *rpc.Client, ataAddr solanago.PublicKey) uint64 {
	t.Helper()
	for i := 0; i < 25; i++ {
		res, e := raw.GetTokenAccountBalance(ctx, ataAddr, rpc.CommitmentConfirmed)
		if e == nil && res != nil && res.Value != nil {
			v, perr := strconv.ParseUint(res.Value.Amount, 10, 64)
			require.NoError(t, perr)
			return v
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("token balance for %s not visible in time", ataAddr)
	return 0
}

func TestDevnetFullSubscriptionLifecycle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 9*time.Minute)
	defer cancel()

	endpoint := devnetEndpoint()
	rc := NewRPCClientWithConfig(RPCClientConfig{Endpoint: endpoint, Network: "devnet"})
	raw := rpc.New(endpoint)
	dnRaw = raw

	merchant := fundedMerchant(ctx, t, raw) // env payer (funded) or airdrop

	// --- subscriber: fresh keypair, funded with SOL from merchant ---
	subKey, err := solanago.NewRandomPrivateKey()
	require.NoError(t, err)
	signAndSend(ctx, t, rc, merchant, nil,
		system.NewTransferInstruction(50_000_000, merchant.PublicKey(), subKey.PublicKey()).Build())
	t.Logf("funded subscriber %s with 0.05 SOL", subKey.PublicKey())

	// --- self-controlled SPL test token (merchant = mint authority) ---
	mintKey, err := solanago.NewRandomPrivateKey()
	require.NoError(t, err)
	mintRent, err := rc.GetMinimumBalanceForRentExemption(ctx, 82)
	require.NoError(t, err)
	signAndSend(ctx, t, rc, merchant, []solanago.PrivateKey{mintKey},
		system.NewCreateAccountInstruction(mintRent, 82, token.ProgramID, merchant.PublicKey(), mintKey.PublicKey()).Build(),
		token.NewInitializeMint2Instruction(6, merchant.PublicKey(), merchant.PublicKey(), mintKey.PublicKey()).Build())
	mint := mintKey.PublicKey()
	t.Logf("created test mint %s (6 decimals)", mint)

	// --- canonical ATAs: merchant (receiver) + subscriber (delegator), then mint to subscriber ---
	merchantATA := createATA(ctx, t, rc, merchant, merchant.PublicKey(), mint)
	t.Logf("merchant token account %s", merchantATA)
	subATA := createATA(ctx, t, rc, merchant, subKey.PublicKey(), mint)
	t.Logf("subscriber token account %s", subATA)
	signAndSend(ctx, t, rc, merchant, nil,
		token.NewMintToInstruction(100_000_000, mint, subATA, merchant.PublicKey(), nil).Build()) // 100 tokens to subscriber
	t.Log("minted 100 tokens to subscriber")

	// --- create_plan: 10 tokens / 720h ---
	planID := uint64(time.Now().UnixNano())
	planPDA, planBump, err := subscriptions.DerivePlanPDA(merchant.PublicKey(), planID)
	require.NoError(t, err)
	createPlanIx, err := subscriptions.BuildCreatePlan(subscriptions.CreatePlanParams{
		Merchant: merchant.PublicKey(), PlanPDA: planPDA, Mint: mint, TokenProgram: token.ProgramID,
		PlanID: planID, Terms: subscriptions.PlanTerms{Amount: 10_000_000, PeriodHours: 720, CreatedAt: time.Now().Unix()},
	})
	require.NoError(t, err)
	signAndSend(ctx, t, rc, merchant, nil, createPlanIx)
	planCreatedAt := readI64(ctx, t, raw, planPDA, 91) // read what the program actually stored
	t.Logf("create_plan OK; on-chain created_at=%d", planCreatedAt)

	// --- init_subscription_authority (subscriber signs; sets SA as delegate on subATA) ---
	saPDA, _, err := subscriptions.DeriveSubscriptionAuthority(subKey.PublicKey(), mint)
	require.NoError(t, err)
	signAndSend(ctx, t, rc, subKey, nil, subscriptions.BuildInitSubscriptionAuthority(subscriptions.InitSubscriptionAuthorityParams{
		Owner: subKey.PublicKey(), SubscriptionAuthority: saPDA, TokenMint: mint, UserATA: subATA, TokenProgram: token.ProgramID,
	}))
	saInitID := readI64(ctx, t, raw, saPDA, 98)
	t.Logf("init_subscription_authority OK; SA=%s initId=%d", saPDA, saInitID)

	// --- subscribe (subscriber signs; snapshots plan terms) ---
	subPDA, _, err := subscriptions.DeriveSubscriptionPDA(planPDA, subKey.PublicKey())
	require.NoError(t, err)
	eventAuth, _, err := subscriptions.DeriveEventAuthority()
	require.NoError(t, err)
	signAndSend(ctx, t, rc, subKey, nil, subscriptions.BuildSubscribe(subscriptions.SubscribeParams{
		Subscriber: subKey.PublicKey(), Merchant: merchant.PublicKey(), PlanPDA: planPDA, SubscriptionPDA: subPDA,
		SubscriptionAuthorityPDA: saPDA, EventAuthority: eventAuth,
		PlanID: planID, PlanBump: planBump, ExpectedMint: mint, ExpectedAmount: 10_000_000, ExpectedPeriodHours: 720,
		ExpectedCreatedAt: planCreatedAt, ExpectedSubscriptionAuthInitID: saInitID,
	}))
	t.Logf("subscribe OK; subscription=%s", subPDA)

	// --- CRANK: transfer_subscription (merchant pulls 10 tokens) ---
	beforeMerchant := tokenBalance(ctx, t, raw, merchantATA)
	beforeSub := tokenBalance(ctx, t, raw, subATA)
	signAndSend(ctx, t, rc, merchant, nil, subscriptions.BuildTransferSubscription(subscriptions.TransferSubscriptionParams{
		SubscriptionPDA: subPDA, PlanPDA: planPDA, SubscriptionAuthority: saPDA,
		DelegatorATA: subATA, ReceiverATA: merchantATA, Caller: merchant.PublicKey(), Mint: mint,
		TokenProgram: token.ProgramID, EventAuthority: eventAuth, Amount: 10_000_000, Delegator: subKey.PublicKey(),
	}))
	afterMerchant := tokenBalance(ctx, t, raw, merchantATA)
	afterSub := tokenBalance(ctx, t, raw, subATA)
	require.Equal(t, beforeMerchant+10_000_000, afterMerchant, "merchant should receive 10 tokens from the crank")
	require.Equal(t, beforeSub-10_000_000, afterSub, "subscriber should be debited 10 tokens by the crank")
	t.Logf("CRANK OK: merchant %d->%d, subscriber %d->%d", beforeMerchant, afterMerchant, beforeSub, afterSub)

	// --- cancel + resume (subscriber signs) ---
	signAndSend(ctx, t, rc, subKey, nil, subscriptions.BuildCancelSubscription(subscriptions.CancelOrResumeParams{
		Subscriber: subKey.PublicKey(), PlanPDA: planPDA, SubscriptionPDA: subPDA, EventAuthority: eventAuth,
	}))
	t.Log("cancel_subscription OK")
	signAndSend(ctx, t, rc, subKey, nil, subscriptions.BuildResumeSubscription(subscriptions.CancelOrResumeParams{
		Subscriber: subKey.PublicKey(), PlanPDA: planPDA, SubscriptionPDA: subPDA, EventAuthority: eventAuth,
	}))
	t.Log("resume_subscription OK")

	t.Log("FULL LIFECYCLE VALIDATED ON DEVNET ✅")
}
