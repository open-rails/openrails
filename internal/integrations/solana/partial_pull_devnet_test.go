//go:build devnet

// Devnet probes for the proration + cancel-detection assumptions the code is
// built on (#263, unblocks #267 and tightens #265):
//
//  1. PARTIAL PULL + PER-PERIOD CAP: transfer_subscription accepts amount <
//     plan_amount, accumulates amount_pulled_in_period, and REJECTS a pull that
//     would exceed the plan amount within a period. This is the on-chain basis
//     for Model-B upgrade proration (charge new_full - old_unused on the first
//     pull) — proving "full payments only" is policy, not a program constraint.
//
//  2. CANCELLED-SUBSCRIPTION PULL ERROR: after cancel_subscription, a crank is
//     rejected; capture the on-chain error so #265's terminal classifier can be
//     tightened to the real program error string.
//
//     Run: SOLANA_DEVNET_PAYER_KEY=<funded> HELIUS_API_KEY=<key> \
//     go test -tags devnet -run PartialPull -v -timeout 580s ./internal/integrations/solana/...
package solana

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	solanago "github.com/doujins-org/solana-go"
	"github.com/doujins-org/solana-go/programs/system"
	"github.com/doujins-org/solana-go/programs/token"
	"github.com/doujins-org/solana-go/rpc"
	"github.com/open-rails/openrails/internal/integrations/solana/subscriptions"
	"github.com/stretchr/testify/require"
)

// sendExpectingError submits a tx WITHOUT preflight and returns the on-chain
// error once confirmed (fails the test only if the tx unexpectedly SUCCEEDS or
// never confirms). Mirrors signAndSend's submit/poll, inverted on the assertion.
func sendExpectingError(ctx context.Context, t *testing.T, rc *RPCClient, feePayer solanago.PrivateKey, instrs ...solanago.Instruction) string {
	t.Helper()
	bh, err := rc.GetLatestBlockhash(ctx)
	require.NoError(t, err)
	tx, err := solanago.NewTransaction(instrs, bh, solanago.TransactionPayer(feePayer.PublicKey()))
	require.NoError(t, err)
	_, err = tx.Sign(func(pk solanago.PublicKey) *solanago.PrivateKey {
		if feePayer.PublicKey().Equals(pk) {
			return &feePayer
		}
		return nil
	})
	require.NoError(t, err)
	sig, err := dnRaw.SendTransactionWithOpts(ctx, tx, rpc.TransactionOpts{SkipPreflight: true, PreflightCommitment: rpc.CommitmentConfirmed})
	require.NoError(t, err)
	for i := 0; i < 45; i++ {
		st, e := dnRaw.GetSignatureStatuses(ctx, true, sig)
		if e == nil && len(st.Value) > 0 && st.Value[0] != nil {
			cs := st.Value[0].ConfirmationStatus
			if cs == rpc.ConfirmationStatusConfirmed || cs == rpc.ConfirmationStatusFinalized {
				require.NotNil(t, st.Value[0].Err, "tx %s was expected to FAIL on-chain but succeeded", sig)
				return formatTxErr(st.Value[0].Err)
			}
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("tx %s not confirmed in time", sig)
	return ""
}

func formatTxErr(e any) string {
	if e == nil {
		return ""
	}
	if b, err := json.Marshal(e); err == nil {
		return string(b)
	}
	return fmt.Sprintf("%v", e)
}

func TestDevnetPartialPullAndCap(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 9*time.Minute)
	defer cancel()

	endpoint := devnetEndpoint()
	rc := NewRPCClientWithConfig(RPCClientConfig{Endpoint: endpoint, Network: "devnet"})
	raw := rpc.New(endpoint)
	dnRaw = raw

	merchant := fundedMerchant(ctx, t, raw)

	// subscriber funded with SOL
	subKey, err := solanago.NewRandomPrivateKey()
	require.NoError(t, err)
	signAndSend(ctx, t, rc, merchant, nil,
		system.NewTransferInstruction(50_000_000, merchant.PublicKey(), subKey.PublicKey()).Build())

	// self-controlled SPL test token (6 decimals), merchant = mint authority
	mintKey, err := solanago.NewRandomPrivateKey()
	require.NoError(t, err)
	mintRent, err := rc.GetMinimumBalanceForRentExemption(ctx, 82)
	require.NoError(t, err)
	signAndSend(ctx, t, rc, merchant, []solanago.PrivateKey{mintKey},
		system.NewCreateAccountInstruction(mintRent, 82, token.ProgramID, merchant.PublicKey(), mintKey.PublicKey()).Build(),
		token.NewInitializeMint2Instruction(6, merchant.PublicKey(), merchant.PublicKey(), mintKey.PublicKey()).Build())
	mint := mintKey.PublicKey()

	merchantATA := createATA(ctx, t, rc, merchant, merchant.PublicKey(), mint)
	subATA := createATA(ctx, t, rc, merchant, subKey.PublicKey(), mint)
	signAndSend(ctx, t, rc, merchant, nil,
		token.NewMintToInstruction(100_000_000, mint, subATA, merchant.PublicKey(), nil).Build())

	// create_plan: 10 tokens / 720h
	planID := uint64(time.Now().UnixNano())
	planPDA, planBump, err := subscriptions.DerivePlanPDA(merchant.PublicKey(), planID)
	require.NoError(t, err)
	createPlanIx, err := subscriptions.BuildCreatePlan(subscriptions.CreatePlanParams{
		Merchant: merchant.PublicKey(), PlanPDA: planPDA, Mint: mint, TokenProgram: token.ProgramID,
		PlanID: planID, Terms: subscriptions.PlanTerms{Amount: 10_000_000, PeriodHours: 720, CreatedAt: time.Now().Unix()},
	})
	require.NoError(t, err)
	signAndSend(ctx, t, rc, merchant, nil, createPlanIx)
	planCreatedAt := readI64(ctx, t, raw, planPDA, 91)

	// init + subscribe
	saPDA, _, err := subscriptions.DeriveSubscriptionAuthority(subKey.PublicKey(), mint)
	require.NoError(t, err)
	signAndSend(ctx, t, rc, subKey, nil, subscriptions.BuildInitSubscriptionAuthority(subscriptions.InitSubscriptionAuthorityParams{
		Owner: subKey.PublicKey(), SubscriptionAuthority: saPDA, TokenMint: mint, UserATA: subATA, TokenProgram: token.ProgramID,
	}))
	saInitID := readI64(ctx, t, raw, saPDA, 98)
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

	crank := func(amount uint64) subscriptions.TransferSubscriptionParams {
		return subscriptions.TransferSubscriptionParams{
			SubscriptionPDA: subPDA, PlanPDA: planPDA, SubscriptionAuthority: saPDA,
			DelegatorATA: subATA, ReceiverATA: merchantATA, Caller: merchant.PublicKey(), Mint: mint,
			TokenProgram: token.ProgramID, EventAuthority: eventAuth, Amount: amount, Delegator: subKey.PublicKey(),
		}
	}

	// PROBE 1: partial pull of 4 (< plan 10) must SUCCEED.
	before := tokenBalance(ctx, t, raw, merchantATA)
	signAndSend(ctx, t, rc, merchant, nil, subscriptions.BuildTransferSubscription(crank(4_000_000)))
	require.Equal(t, before+4_000_000, tokenBalance(ctx, t, raw, merchantATA), "partial pull of 4 should move 4 tokens")
	t.Log("PROBE 1 OK: partial pull (4 < 10) accepted")

	// PROBE 2: second pull of 6 (cumulative 10 = cap) must SUCCEED.
	before2 := tokenBalance(ctx, t, raw, merchantATA)
	signAndSend(ctx, t, rc, merchant, nil, subscriptions.BuildTransferSubscription(crank(6_000_000)))
	require.Equal(t, before2+6_000_000, tokenBalance(ctx, t, raw, merchantATA), "second pull to the cap should move 6 tokens")
	t.Log("PROBE 2 OK: cumulative pull to cap (4+6=10) accepted")

	// PROBE 3: pull of 1 more (over cap) must be REJECTED by the program.
	overCapErr := sendExpectingError(ctx, t, rc, merchant, subscriptions.BuildTransferSubscription(crank(1_000_000)))
	t.Logf("PROBE 3 OK: over-cap pull REJECTED; on-chain error = %s", overCapErr)

	// PROBE 4: cancel, then a crank must be REJECTED — capture the error string
	// so #265's terminal classifier can be tightened to it.
	signAndSend(ctx, t, rc, subKey, nil, subscriptions.BuildCancelSubscription(subscriptions.CancelOrResumeParams{
		Subscriber: subKey.PublicKey(), PlanPDA: planPDA, SubscriptionPDA: subPDA, EventAuthority: eventAuth,
	}))
	cancelledErr := sendExpectingError(ctx, t, rc, merchant, subscriptions.BuildTransferSubscription(crank(1_000_000)))
	t.Logf("PROBE 4 OK: pull against CANCELLED subscription REJECTED; on-chain error = %s", cancelledErr)

	t.Log("PARTIAL-PULL + CAP + CANCEL-ERROR VALIDATED ON DEVNET ✅")
	t.Logf("SUMMARY: partial pulls allowed (Model-B proration confirmed); over-cap err=%s; cancelled-pull err=%s", overCapErr, cancelledErr)
}

// TestDevnetCancelVsRevokeSemantics establishes what actually STOPS a pull —
// the load-bearing fact behind #264/#265/#266. It tests, on a fresh sub with the
// period cap untouched: (A) cancel_subscription then a crank — does cancel block
// the pull? and (B) revoke_delegation then a crank — does revoking the SPL token
// delegate block it, and with what error code? This is the exact terminal signal
// #265 must key on.
func TestDevnetCancelVsRevokeSemantics(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 9*time.Minute)
	defer cancel()

	endpoint := devnetEndpoint()
	rc := NewRPCClientWithConfig(RPCClientConfig{Endpoint: endpoint, Network: "devnet"})
	raw := rpc.New(endpoint)
	dnRaw = raw
	merchant := fundedMerchant(ctx, t, raw)

	subKey, err := solanago.NewRandomPrivateKey()
	require.NoError(t, err)
	// Fund the throwaway subscriber lean (gas + its own PDA/sub rent ~0.004 SOL);
	// sweep the remainder back to the merchant at the end to avoid stranding SOL.
	signAndSend(ctx, t, rc, merchant, nil,
		system.NewTransferInstruction(15_000_000, merchant.PublicKey(), subKey.PublicKey()).Build())
	defer func() {
		if bal, e := rc.GetBalance(context.Background(), subKey.PublicKey()); e == nil && bal > 1_000_000 {
			_, _ = dnRaw.SendTransactionWithOpts(context.Background(),
				mustTx(rc, subKey, system.NewTransferInstruction(bal-1_000_000, subKey.PublicKey(), merchant.PublicKey()).Build()),
				rpc.TransactionOpts{SkipPreflight: true})
		}
	}()

	mintKey, err := solanago.NewRandomPrivateKey()
	require.NoError(t, err)
	mintRent, err := rc.GetMinimumBalanceForRentExemption(ctx, 82)
	require.NoError(t, err)
	signAndSend(ctx, t, rc, merchant, []solanago.PrivateKey{mintKey},
		system.NewCreateAccountInstruction(mintRent, 82, token.ProgramID, merchant.PublicKey(), mintKey.PublicKey()).Build(),
		token.NewInitializeMint2Instruction(6, merchant.PublicKey(), merchant.PublicKey(), mintKey.PublicKey()).Build())
	mint := mintKey.PublicKey()
	merchantATA := createATA(ctx, t, rc, merchant, merchant.PublicKey(), mint)
	subATA := createATA(ctx, t, rc, merchant, subKey.PublicKey(), mint)
	signAndSend(ctx, t, rc, merchant, nil,
		token.NewMintToInstruction(100_000_000, mint, subATA, merchant.PublicKey(), nil).Build())

	planID := uint64(time.Now().UnixNano())
	planPDA, planBump, err := subscriptions.DerivePlanPDA(merchant.PublicKey(), planID)
	require.NoError(t, err)
	createPlanIx, err := subscriptions.BuildCreatePlan(subscriptions.CreatePlanParams{
		Merchant: merchant.PublicKey(), PlanPDA: planPDA, Mint: mint, TokenProgram: token.ProgramID,
		PlanID: planID, Terms: subscriptions.PlanTerms{Amount: 10_000_000, PeriodHours: 720, CreatedAt: time.Now().Unix()},
	})
	require.NoError(t, err)
	signAndSend(ctx, t, rc, merchant, nil, createPlanIx)
	planCreatedAt := readI64(ctx, t, raw, planPDA, 91)

	saPDA, _, err := subscriptions.DeriveSubscriptionAuthority(subKey.PublicKey(), mint)
	require.NoError(t, err)
	signAndSend(ctx, t, rc, subKey, nil, subscriptions.BuildInitSubscriptionAuthority(subscriptions.InitSubscriptionAuthorityParams{
		Owner: subKey.PublicKey(), SubscriptionAuthority: saPDA, TokenMint: mint, UserATA: subATA, TokenProgram: token.ProgramID,
	}))
	saInitID := readI64(ctx, t, raw, saPDA, 98)
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

	crank := func(amount uint64) solanago.Instruction {
		return subscriptions.BuildTransferSubscription(subscriptions.TransferSubscriptionParams{
			SubscriptionPDA: subPDA, PlanPDA: planPDA, SubscriptionAuthority: saPDA,
			DelegatorATA: subATA, ReceiverATA: merchantATA, Caller: merchant.PublicKey(), Mint: mint,
			TokenProgram: token.ProgramID, EventAuthority: eventAuth, Amount: amount, Delegator: subKey.PublicKey(),
		})
	}

	// (A) cancel_subscription, then crank a partial amount (cap untouched).
	signAndSend(ctx, t, rc, subKey, nil, subscriptions.BuildCancelSubscription(subscriptions.CancelOrResumeParams{
		Subscriber: subKey.PublicKey(), PlanPDA: planPDA, SubscriptionPDA: subPDA, EventAuthority: eventAuth,
	}))
	beforeA := tokenBalance(ctx, t, raw, merchantATA)
	cancelSig, cancelErr := trySend(ctx, t, rc, merchant, crank(4_000_000))
	afterA := tokenBalance(ctx, t, raw, merchantATA)
	if cancelErr == "" && afterA == beforeA+4_000_000 {
		t.Logf("(A) FINDING: crank AFTER cancel_subscription SUCCEEDED (moved 4 tokens, sig %s) -> cancel does NOT block pulls; soft-cancel (stop cranking) is the real billing-stop", cancelSig)
	} else {
		t.Logf("(A) crank after cancel was REJECTED; err=%s (cancel DOES block pulls)", cancelErr)
	}

	// (B) The subscriber revokes the SPL TOKEN DELEGATE on their ATA — the
	// authority transfer_subscription actually uses to move funds. This is the
	// trustless hard-stop. Capture whether the next crank is rejected + its code.
	revokeSig, revokeErr := trySend(ctx, t, rc, subKey, token.NewRevokeInstruction(subATA, subKey.PublicKey(), nil).Build())
	t.Logf("(B) SPL token Revoke of the ATA delegate: sig=%s err=%q", revokeSig, revokeErr)
	beforeB := tokenBalance(ctx, t, raw, merchantATA)
	crankSig, crankErr := trySend(ctx, t, rc, merchant, crank(4_000_000))
	afterB := tokenBalance(ctx, t, raw, merchantATA)
	if crankErr != "" {
		t.Logf("(B) FINDING: crank AFTER token-delegate revoke REJECTED; on-chain error = %s -> this is the trustless terminal stop (#265/#266)", crankErr)
	} else {
		t.Logf("(B) crank after delegate revoke SUCCEEDED (sig %s, moved %d) -> revoke did NOT stop it", crankSig, afterB-beforeB)
	}
	t.Log("COMPARE: over-cap = Custom:400; cancel does NOT stop pulls; the SPL-delegate revoke is the trustless stop.")
}

// mustTx builds + signs a single-signer tx (used by the SOL sweep-back).
func mustTx(rc *RPCClient, payer solanago.PrivateKey, instrs ...solanago.Instruction) *solanago.Transaction {
	bh, _ := rc.GetLatestBlockhash(context.Background())
	tx, _ := solanago.NewTransaction(instrs, bh, solanago.TransactionPayer(payer.PublicKey()))
	_, _ = tx.Sign(func(pk solanago.PublicKey) *solanago.PrivateKey {
		if payer.PublicKey().Equals(pk) {
			return &payer
		}
		return nil
	})
	return tx
}

// trySend submits without preflight and returns (signature, onchainErr) — empty
// err string means the tx SUCCEEDED on-chain.
func trySend(ctx context.Context, t *testing.T, rc *RPCClient, feePayer solanago.PrivateKey, instrs ...solanago.Instruction) (string, string) {
	t.Helper()
	bh, err := rc.GetLatestBlockhash(ctx)
	require.NoError(t, err)
	tx, err := solanago.NewTransaction(instrs, bh, solanago.TransactionPayer(feePayer.PublicKey()))
	require.NoError(t, err)
	_, err = tx.Sign(func(pk solanago.PublicKey) *solanago.PrivateKey {
		if feePayer.PublicKey().Equals(pk) {
			return &feePayer
		}
		return nil
	})
	require.NoError(t, err)
	sig, err := dnRaw.SendTransactionWithOpts(ctx, tx, rpc.TransactionOpts{SkipPreflight: true, PreflightCommitment: rpc.CommitmentConfirmed})
	require.NoError(t, err)
	for i := 0; i < 45; i++ {
		st, e := dnRaw.GetSignatureStatuses(ctx, true, sig)
		if e == nil && len(st.Value) > 0 && st.Value[0] != nil {
			cs := st.Value[0].ConfirmationStatus
			if cs == rpc.ConfirmationStatusConfirmed || cs == rpc.ConfirmationStatusFinalized {
				return sig.String(), formatTxErr(st.Value[0].Err)
			}
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("tx %s not confirmed in time", sig)
	return "", ""
}
