//go:build devnet

// Devnet validation of the ATOMIC upgrade transaction (#267): the whole tier
// change is ONE transaction OpenRails builds + the user signs (with the merchant
// co-signing the pull).
//
//	UPGRADE = [cancel(old), subscribe(new), transfer_subscription(new, prorated)]
//	          signers: subscriber (cancel+subscribe+fee) + merchant (transfer caller)
//
//	Run: SOLANA_DEVNET_PAYER_KEY=<funded> HELIUS_API_KEY=<key> \
//		go test -tags devnet -run UpgradeBundle -v -timeout 300s ./internal/integrations/solana/...
package solana

import (
	"context"
	"testing"
	"time"

	solanago "github.com/doujins-org/solana-go"
	"github.com/doujins-org/solana-go/programs/system"
	"github.com/doujins-org/solana-go/programs/token"
	"github.com/doujins-org/solana-go/rpc"
	"github.com/open-rails/openrails/internal/integrations/solana/subscriptions"
	"github.com/stretchr/testify/require"
)

type devnetPlan struct {
	pda       solanago.PublicKey
	id        uint64
	bump      uint8
	createdAt int64
}

func TestDevnetUpgradeBundleAtomic(t *testing.T) {
	ctx, cancelCtx := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancelCtx()

	endpoint := devnetEndpoint()
	rc := NewRPCClientWithConfig(RPCClientConfig{Endpoint: endpoint, Network: "devnet"})
	raw := rpc.New(endpoint)
	dnRaw = raw
	merchant := fundedMerchant(ctx, t, raw)

	subKey, err := solanago.NewRandomPrivateKey()
	require.NoError(t, err)
	signAndSend(ctx, t, rc, merchant, nil,
		system.NewTransferInstruction(20_000_000, merchant.PublicKey(), subKey.PublicKey()).Build())
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

	eventAuth, _, err := subscriptions.DeriveEventAuthority()
	require.NoError(t, err)
	saPDA, _, err := subscriptions.DeriveSubscriptionAuthority(subKey.PublicKey(), mint)
	require.NoError(t, err)

	publishPlan := func(amount uint64) devnetPlan {
		id := uint64(time.Now().UnixNano())
		pda, bump, e := subscriptions.DerivePlanPDA(merchant.PublicKey(), id)
		require.NoError(t, e)
		ix, e := subscriptions.BuildCreatePlan(subscriptions.CreatePlanParams{
			Merchant: merchant.PublicKey(), PlanPDA: pda, Mint: mint, TokenProgram: token.ProgramID,
			PlanID: id, Terms: subscriptions.PlanTerms{Amount: amount, PeriodHours: 720, CreatedAt: time.Now().Unix()},
		})
		require.NoError(t, e)
		signAndSend(ctx, t, rc, merchant, nil, ix)
		return devnetPlan{pda: pda, id: id, bump: bump, createdAt: readI64(ctx, t, raw, pda, 91)}
	}

	// OLD plan A (10) + init authority + subscribe.
	planA := publishPlan(10_000_000)
	signAndSend(ctx, t, rc, subKey, nil, subscriptions.BuildInitSubscriptionAuthority(subscriptions.InitSubscriptionAuthorityParams{
		Owner: subKey.PublicKey(), SubscriptionAuthority: saPDA, TokenMint: mint, UserATA: subATA, TokenProgram: token.ProgramID,
	}))
	initID := readI64(ctx, t, raw, saPDA, 98)
	subA, _, err := subscriptions.DeriveSubscriptionPDA(planA.pda, subKey.PublicKey())
	require.NoError(t, err)
	signAndSend(ctx, t, rc, subKey, nil, subscriptions.BuildSubscribe(subscriptions.SubscribeParams{
		Subscriber: subKey.PublicKey(), Merchant: merchant.PublicKey(), PlanPDA: planA.pda, SubscriptionPDA: subA,
		SubscriptionAuthorityPDA: saPDA, EventAuthority: eventAuth, PlanID: planA.id, PlanBump: planA.bump,
		ExpectedMint: mint, ExpectedAmount: 10_000_000, ExpectedPeriodHours: 720, ExpectedCreatedAt: planA.createdAt, ExpectedSubscriptionAuthInitID: initID,
	}))
	t.Log("setup OK: subscribed to plan A (10)")

	// NEW plan B (50).
	planB := publishPlan(50_000_000)
	subB, _, err := subscriptions.DeriveSubscriptionPDA(planB.pda, subKey.PublicKey())
	require.NoError(t, err)

	// ATOMIC UPGRADE: cancel(A) + subscribe(B) + transfer(B, prorated) in ONE tx,
	// co-signed by subscriber (fee + cancel + subscribe) and merchant (transfer caller).
	const prorated = uint64(31_330_000) // 50 - old_unused(~18.67)
	before := tokenBalance(ctx, t, raw, merchantATA)
	bundle := []solanago.Instruction{
		subscriptions.BuildCancelSubscription(subscriptions.CancelOrResumeParams{
			Subscriber: subKey.PublicKey(), PlanPDA: planA.pda, SubscriptionPDA: subA, EventAuthority: eventAuth,
		}),
		subscriptions.BuildSubscribe(subscriptions.SubscribeParams{
			Subscriber: subKey.PublicKey(), Merchant: merchant.PublicKey(), PlanPDA: planB.pda, SubscriptionPDA: subB,
			SubscriptionAuthorityPDA: saPDA, EventAuthority: eventAuth, PlanID: planB.id, PlanBump: planB.bump,
			ExpectedMint: mint, ExpectedAmount: 50_000_000, ExpectedPeriodHours: 720, ExpectedCreatedAt: planB.createdAt, ExpectedSubscriptionAuthInitID: initID,
		}),
		subscriptions.BuildTransferSubscription(subscriptions.TransferSubscriptionParams{
			SubscriptionPDA: subB, PlanPDA: planB.pda, SubscriptionAuthority: saPDA, DelegatorATA: subATA, ReceiverATA: merchantATA,
			Caller: merchant.PublicKey(), Mint: mint, TokenProgram: token.ProgramID, EventAuthority: eventAuth,
			Amount: prorated, Delegator: subKey.PublicKey(),
		}),
	}
	// subscriber = fee payer; merchant co-signs (the transfer caller).
	signAndSend(ctx, t, rc, subKey, []solanago.PrivateKey{merchant}, bundle...)
	after := tokenBalance(ctx, t, raw, merchantATA)
	require.Equal(t, before+prorated, after, "atomic upgrade should immediately pull the prorated amount")
	require.NotZero(t, tokenBalance(ctx, t, raw, merchantATA))
	t.Logf("✅ ATOMIC UPGRADE OK: ONE tx did cancel(A)+subscribe(B)+pull(%d); merchant %d->%d", prorated, before, after)
}
