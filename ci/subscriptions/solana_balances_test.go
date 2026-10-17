//go:build e2e && integration

package subscriptions_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/engine"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchantdocs"
	"github.com/open-rails/openrails/internal/merchants"
	riverjobs "github.com/open-rails/openrails/internal/river"
)

func TestSolanaBalanceWarnsBeforeSubscribersAndRecovers(t *testing.T) {
	s := openSolanaShop(t)
	rt := engine.Graph(s.w.rt).Runtime
	ctx := t.Context()
	beforeTxs := s.fake.Transactions(s.cranker.PublicKey())
	s.fake.SetSOLBalance(s.cranker.PublicKey(), 0)
	s.fake.SetSOLBalance(s.treasury, 1_000_000_000)
	balances, err := rt.SolanaCrankerBalances(ctx)
	require.NoError(t, err)
	require.Len(t, balances, 1)
	require.Equal(t, s.cranker.PublicKey(), balances[0].Wallet)
	require.NotEqual(t, s.treasury, balances[0].Wallet)
	require.True(t, balances[0].Low())
	require.ErrorContains(t, s.w.probe("openrails_solana_crankers"), "0 SOL")
	require.NoError(t, s.w.rt.Ready(ctx), "a funding warning must not stop the engine")
	job, err := s.w.jobs.Insert(ctx, riverjobs.SolanaGasAlertArgs{}, &river.InsertOpts{Queue: "billing"})
	require.NoError(t, err)
	s.w.waitJob(job.Job.ID)
	s.fake.SetSOLBalance(s.cranker.PublicKey(), 1_000_000_000)
	require.Error(t, s.w.probe("openrails_solana_crankers"), "a fresh cached sample is reused")
	s.w.clock.Advance(solanaint.BalanceCacheTTL)
	require.NoError(t, s.w.probe("openrails_solana_crankers"))
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = rt.SolanaCrankerBalances(canceled)
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, beforeTxs, s.fake.Transactions(s.cranker.PublicKey()), "monitoring never submits a transaction")
}

func TestSolanaBalanceKeepsArchivedObligationAndChecksOtherTargets(t *testing.T) {
	s := openSolanaShop(t)
	buyer, sub := s.subscribed(t)
	oldPSP := s.w.psp["solana"].UUID()
	newer := solanago.NewWallet().PrivateKey
	s.fake.SetSOLBalance(newer.PublicKey(), 1_000_000_000)
	s.sol.crankers = []solanago.PrivateKey{newer, s.cranker}
	s.w.restart()
	s.fake.SetSOLBalance(s.cranker.PublicKey(), 1)
	rt := engine.Graph(s.w.rt).Runtime
	mid := s.w.client[embedded].MerchantID()
	balances, err := rt.SolanaCrankerBalances(t.Context())
	require.NoError(t, err)
	require.Len(t, balances, 2, "older plan owner and new publisher are both checked")
	before, err := rt.MerchantConfig.Get(t.Context(), mid)
	require.NoError(t, err)
	next := before.Clone()
	old := next.PSPs["solana"]
	old.Value.Archived = true
	next.PSPs["solana"] = old
	replacement := old
	replacement.Value.Archived = false
	replacement.Value.AccountID = solanago.NewWallet().PublicKey().String()
	replacement.Value.Environment = "testnet"
	next.PSPs["new-solana"] = replacement
	rt.MerchantConfig.Source().(*merchantdocs.FileSource).Put(mid, next)
	served, err := rt.MerchantConfig.Reload(t.Context(), mid)
	require.NoError(t, err)
	require.Empty(t, served.Rejected)
	// The loopback node is devnet. The new testnet PSP must fail its network
	// check without concealing the old owner's valid devnet warning.
	balances, err = rt.SolanaCrankerBalances(t.Context())
	require.ErrorContains(t, err, "declares testnet")
	require.Len(t, balances, 1)
	require.Equal(t, oldPSP, balances[0].PSPID)
	require.Equal(t, s.cranker.PublicKey(), balances[0].Wallet)
	require.Equal(t, "devnet", balances[0].Network)
	require.True(t, balances[0].Low())
	worker := riverjobs.SolanaGasAlertWorker{Check: rt.SolanaCrankerBalances}
	require.Error(t, worker.Work(t.Context(), nil), "a partially failed sweep must retry")
	require.Equal(t, billing.SubscriptionStatus("active"), s.w.subscription(embedded, sub).Status)
	// Restore the new PSP's network and fund the old wallet; neither operation
	// changes an accepted subscription or executes a payment.
	replacement.Value.Environment = "devnet"
	next.PSPs["new-solana"] = replacement
	rt.MerchantConfig.Source().(*merchantdocs.FileSource).Put(mid, next)
	served, err = rt.MerchantConfig.Reload(t.Context(), mid)
	require.NoError(t, err)
	require.Empty(t, served.Rejected)
	s.fake.SetSOLBalance(s.cranker.PublicKey(), 1_000_000_000)
	s.w.clock.Advance(solanaint.BalanceCacheTTL)
	require.NoError(t, s.w.probe("openrails_solana_crankers"))
	status, body := buyer.cancel(sub, map[string]any{"reason": "done"})
	require.Equal(t, http.StatusOK, status, "%v", body)
	balances, err = rt.SolanaCrankerBalances(t.Context())
	require.NoError(t, err)
	require.Len(t, balances, 1, "a canceled obligation no longer requires the old wallet")
	require.Equal(t, newer.PublicKey(), balances[0].Wallet)
	require.True(t, buyer.entitled(s.key), "monitoring does not change paid access")
}

func TestSolanaBalanceMerchantIsolationAndRPCRecovery(t *testing.T) {
	s := openSolanaShop(t)
	rt := engine.Graph(s.w.rt).Runtime
	mid := s.w.client[embedded].MerchantID()
	other := billing.MerchantID(uuid.New())
	_, _, err := rt.Merchants.RegisterForRestore(t.Context(), other, "balance-other-"+uuid.NewString()[:8])
	require.NoError(t, err)
	otherTreasury := solanago.NewWallet().PublicKey().String()
	declaration := merchantdocs.Set{HasMerchant: true, Merchant: merchantdocs.Doc[merchantdocs.Merchant]{Value: merchantdocs.Merchant{}}, PSPs: map[string]merchantdocs.Doc[merchantdocs.PSP]{
		"solana": {Value: merchantdocs.PSP{Rail: "solana", Environment: "testnet", AccountID: otherTreasury}},
	}}
	rt.MerchantConfig.Source().(*merchantdocs.FileSource).Put(other, declaration)
	served, err := rt.MerchantConfig.Reload(t.Context(), other)
	require.NoError(t, err)
	require.Empty(t, served.Rejected)
	rt.DeclareMerchants([]billing.MerchantID{other, mid}, nil)
	balances, err := rt.SolanaCrankerBalances(t.Context())
	require.ErrorContains(t, err, other.String())
	require.Len(t, balances, 1)
	require.Equal(t, mid, balances[0].MerchantID)
	foreignPSP := merchants.PspID("solana", "testnet", otherTreasury)
	_, _, err = rt.SolanaRPCResolver.ResolvePSP(t.Context(), mid, foreignPSP)
	require.Error(t, err, "another merchant cannot borrow the PSP identity")
	// A scoped pass remains within that merchant, even with multiple declarations.
	balances, err = rt.SolanaCrankerBalances(merchant.WithID(t.Context(), mid))
	require.NoError(t, err)
	require.Len(t, balances, 1)
	// After expiry, an RPC failure cannot keep the prior healthy balance alive.
	s.w.clock.Advance(solanaint.BalanceCacheTTL)
	stalled, release := s.fake.Hold()
	done := make(chan error, 1)
	ctx, cancel := context.WithTimeout(merchant.WithID(t.Context(), mid), 100*time.Millisecond)
	defer cancel()
	go func() { _, err := rt.SolanaCrankerBalances(ctx); done <- err }()
	select {
	case <-stalled:
	case <-time.After(time.Second):
		t.Fatal("read did not reach node")
	}
	err = <-done
	release()
	require.ErrorIs(t, err, context.DeadlineExceeded)
	balances, err = rt.SolanaCrankerBalances(merchant.WithID(t.Context(), mid))
	require.NoError(t, err)
	require.Len(t, balances, 1)
	require.False(t, balances[0].Low())
}
