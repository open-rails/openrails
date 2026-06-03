//go:build devnet

// End-to-end validation of the REAL recurring services against devnet with REAL
// USDC (#263) — PlanService.PublishPlan -> PrepareSubscribeService -> wallet
// signs -> CrankService.Crank (pulls real USDC) -> PrepareCancelService. This
// exercises the production code paths (allowlist resolution, the verified
// instruction builders, submit+confirm), not raw builders. No DB: the
// membership/ledger side (EnrollService) is unit-tested + covered in the docker
// stack; this proves the on-chain service layer works with real USDC.
//
// Prereqs: fund the subscriber's USDC ATA via https://faucet.circle.com (Solana
// devnet, USDC). The test funds the subscriber's SOL gas from the payer.
//
//	SOLANA_DEVNET_PAYER_KEY=<funded> SOLANA_DEVNET_SUBSCRIBER_KEY=<usdc-funded> \
//	HELIUS_API_KEY=<key> go test -tags devnet -run ServiceLayerUSDC -v \
//	  -timeout 480s ./internal/modules/solana/recurring/...
package recurring

import (
	"context"
	"encoding/base64"
	"os"
	"testing"
	"time"

	solanago "github.com/doujins-org/solana-go"
	"github.com/doujins-org/solana-go/programs/system"
	"github.com/doujins-org/solana-go/programs/token"
	"github.com/doujins-org/solana-go/rpc"
	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/integrations/solana/subscriptions"
	"github.com/open-rails/openrails/pkg/tenant"
	"github.com/stretchr/testify/require"
)

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

type fixedSubReader struct{ row *models.SolanaSubscription }

func (f fixedSubReader) GetBySubscriptionID(context.Context, uuid.UUID) (*models.SolanaSubscription, error) {
	return f.row, nil
}

// memSecretGetter serves the merchant (cranker) key as the tenant's
// solana/private_key, so the production keypair signer / Submitter resolve it.
type memSecretGetter struct{ key string }

func (m memSecretGetter) GetSecret(_ context.Context, _ tenant.ID, name string) (string, error) {
	return m.key, nil
}

func devnetEnv(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Skipf("%s not set; skipping devnet service-layer test", name)
	}
	return v
}

func TestDevnetServiceLayerUSDC(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	endpoint := "https://devnet.helius-rpc.com/?api-key=" + devnetEnv(t, "HELIUS_API_KEY")
	rc := solanaint.NewRPCClientWithConfig(solanaint.RPCClientConfig{Endpoint: endpoint, Network: "devnet"})
	raw := rpc.New(endpoint)

	merchant, err := solanago.PrivateKeyFromBase58(devnetEnv(t, "SOLANA_DEVNET_PAYER_KEY"))
	require.NoError(t, err)
	// The env wallet holds the faucet USDC; it FUNDS a FRESH subscriber each run so
	// the SubscriptionAuthority state never accumulates (mirrors the mechanics tests
	// and avoids the repeat-subscribe Custom:519).
	funder, err := solanago.PrivateKeyFromBase58(devnetEnv(t, "SOLANA_DEVNET_SUBSCRIBER_KEY"))
	require.NoError(t, err)

	// USDC devnet mint (must match config DefaultDevnetTokens).
	usdc := solanago.MustPublicKeyFromBase58("4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU")
	funderUSDC, err := rc.GetTokenBalanceForMint(ctx, funder.PublicKey(), usdc)
	if err != nil || funderUSDC < 3_000_000 {
		t.Skipf("funder %s has %d USDC base units (<3 USDC) — fund via https://faucet.circle.com (Solana devnet, USDC) then re-run",
			funder.PublicKey(), funderUSDC)
	}
	t.Logf("funder USDC balance: %d base units", funderUSDC)

	// Fresh subscriber each run.
	subscriber, err := solanago.NewRandomPrivateKey()
	require.NoError(t, err)
	t.Logf("fresh subscriber: %s", subscriber.PublicKey())
	fundSOL(ctx, t, rc, raw, merchant, subscriber.PublicKey(), 30_000_000)
	ensureATA(ctx, t, rc, raw, merchant, subscriber.PublicKey(), usdc) // subscriber USDC ATA
	transferUSDC(ctx, t, rc, raw, funder, subscriber.PublicKey(), usdc, 2_000_000)
	t.Log("funded fresh subscriber with 3 USDC + SOL gas")

	// The cranker pulls USDC INTO the merchant's USDC ATA — ensure it exists
	// (idempotent). In production this is a tenant-setup step (the receiving ATA
	// for the cranker/merchant wallet must exist before the first pull).
	ensureATA(ctx, t, rc, raw, merchant, merchant.PublicKey(), usdc)

	// Production Submitter + services backed by the merchant (cranker) key.
	signer := solanaint.NewKeypairSigner(memSecretGetter{key: merchant.String()}, 0)
	submitter := NewSignerSubmitter(signer, rc)
	planSvc := NewPlanService(submitter, "devnet")
	prepSvc := NewPrepareSubscribeService(submitter, rc, "devnet")
	crankSvc := NewCrankService(submitter)

	// 1) Publish a 1-USDC / 720h plan on-chain (real PlanService path).
	const amount = uint64(1_000_000) // 1 USDC
	handle, err := planSvc.PublishPlan(ctx, PublishPlanInput{
		TenantID: tenant.DefaultID, PlanID: uint64(time.Now().UnixNano()),
		TokenSymbol: "USDC", AmountBaseUnits: amount, PeriodHours: 720,
	})
	require.NoError(t, err, "PublishPlan should create the plan on-chain")
	t.Logf("1) PublishPlan OK: plan_pda=%s sig=%s", handle.PlanPDA, handle.Signature)

	// 2) Prepare the subscribe transaction(s) and sign+send them as the wallet.
	for step := 0; step < 2; step++ {
		res, perr := prepSvc.Prepare(ctx, PrepareSubscribeInput{
			TenantID: tenant.DefaultID, SubscriberWallet: subscriber.PublicKey().String(),
			PlanID: handle.PlanID, MintSymbol: "USDC", AmountBaseUnits: amount, PeriodHours: 720, PlanCreatedAt: handle.CreatedAt,
		})
		require.NoError(t, perr)
		t.Logf("2) Prepare step=%s authorityExists=%v txns=%d", res.Step, res.AuthorityExists, len(res.Transactions))
		signAndSendBase64(ctx, t, rc, raw, subscriber, res.Transactions)
		if res.Step == "subscribe" {
			break // returning/after-init: subscription now exists
		}
	}

	// 3) Crank: the merchant pulls 1 USDC from the subscriber (real CrankService).
	row := subscriptionRowFromHandle(handle, subscriber.PublicKey().String())
	beforeMerchant, _ := rc.GetTokenBalanceForMint(ctx, merchant.PublicKey(), usdc)
	sig, cerr := crankSvc.Crank(ctx, tenant.DefaultID, row, amount)
	require.NoError(t, cerr, "Crank should pull 1 USDC")
	afterMerchant, _ := rc.GetTokenBalanceForMint(ctx, merchant.PublicKey(), usdc)
	require.Equal(t, beforeMerchant+amount, afterMerchant, "merchant should receive 1 USDC from the crank")
	t.Logf("3) Crank OK: pulled 1 USDC; merchant %d->%d; sig=%s", beforeMerchant, afterMerchant, sig)

	// 4) Cancel: build the on-chain cancel_subscription tx (PrepareCancelService
	// path) and sign+send it as the wallet.
	cancelSvc := NewPrepareCancelService(fixedSubReader{row: row}, rc)
	cres, xerr := cancelSvc.Prepare(ctx, row.SubscriptionID)
	require.NoError(t, xerr)
	signAndSendBase64(ctx, t, rc, raw, subscriber, []string{cres.Transaction})
	t.Logf("4) Cancel OK: cancel_subscription submitted for %s", cres.SubscriptionPDA)

	t.Log("✅ SERVICE LAYER VALIDATED ON DEVNET WITH REAL USDC: publish -> subscribe -> crank -> cancel")
}

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
	_, err = tx.Sign(func(pk solanago.PublicKey) *solanago.PrivateKey {
		if payer.PublicKey().Equals(pk) {
			return &payer
		}
		return nil
	})
	require.NoError(t, err)
	sig, err := raw.SendTransactionWithOpts(ctx, tx, rpc.TransactionOpts{SkipPreflight: true})
	require.NoError(t, err)
	_, _ = rc.WatchTransaction(ctx, sig, rpc.CommitmentConfirmed, 60*time.Second)
}

// transferUSDC moves `amount` of `mint` from the funder's ATA to the recipient's
// ATA (both canonical), signed by the funder.
func transferUSDC(ctx context.Context, t *testing.T, rc *solanaint.RPCClient, raw *rpc.Client, funder solanago.PrivateKey, to solanago.PublicKey, mint solanago.PublicKey, amount uint64) {
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
	_, err = tx.Sign(func(pk solanago.PublicKey) *solanago.PrivateKey {
		if funder.PublicKey().Equals(pk) {
			return &funder
		}
		return nil
	})
	require.NoError(t, err)
	sig, err := raw.SendTransactionWithOpts(ctx, tx, rpc.TransactionOpts{SkipPreflight: true})
	require.NoError(t, err)
	out, werr := rc.WatchTransaction(ctx, sig, rpc.CommitmentConfirmed, 60*time.Second)
	require.NoError(t, werr)
	require.Nil(t, out.OnChainError(), "USDC funding transfer failed")
}

func fundSOL(ctx context.Context, t *testing.T, rc *solanaint.RPCClient, raw *rpc.Client, from solanago.PrivateKey, to solanago.PublicKey, lamports uint64) {
	t.Helper()
	bh, err := rc.GetLatestBlockhash(ctx)
	require.NoError(t, err)
	tx, err := solanago.NewTransaction([]solanago.Instruction{
		system.NewTransferInstruction(lamports, from.PublicKey(), to).Build(),
	}, bh, solanago.TransactionPayer(from.PublicKey()))
	require.NoError(t, err)
	_, err = tx.Sign(func(pk solanago.PublicKey) *solanago.PrivateKey {
		if from.PublicKey().Equals(pk) {
			return &from
		}
		return nil
	})
	require.NoError(t, err)
	sig, err := raw.SendTransactionWithOpts(ctx, tx, rpc.TransactionOpts{SkipPreflight: true})
	require.NoError(t, err)
	_, _ = rc.WatchTransaction(ctx, sig, rpc.CommitmentConfirmed, 60*time.Second)
}

// signAndSendBase64 deserializes each unsigned base64 tx, signs it as the
// subscriber (the wallet), refreshes the blockhash, and submits + confirms.
func signAndSendBase64(ctx context.Context, t *testing.T, rc *solanaint.RPCClient, raw *rpc.Client, wallet solanago.PrivateKey, txns []string) {
	t.Helper()
	for _, b64 := range txns {
		data, err := base64.StdEncoding.DecodeString(b64)
		require.NoError(t, err)
		tx, err := solanago.TransactionFromBytes(data)
		require.NoError(t, err)
		bh, err := rc.GetLatestBlockhash(ctx)
		require.NoError(t, err)
		tx.Message.RecentBlockhash = bh
		tx.Signatures = nil
		_, err = tx.Sign(func(pk solanago.PublicKey) *solanago.PrivateKey {
			if wallet.PublicKey().Equals(pk) {
				return &wallet
			}
			return nil
		})
		require.NoError(t, err)
		sig, err := raw.SendTransactionWithOpts(ctx, tx, rpc.TransactionOpts{SkipPreflight: true, PreflightCommitment: rpc.CommitmentConfirmed})
		require.NoError(t, err)
		out, werr := rc.WatchTransaction(ctx, sig, rpc.CommitmentConfirmed, 90*time.Second)
		require.NoError(t, werr)
		require.Nil(t, out.OnChainError(), "tx %s failed on-chain", sig)
	}
}
