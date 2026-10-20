//go:build e2e && integration

package subscriptions_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/failpoint"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchantdocs"
	"github.com/open-rails/openrails/internal/solanafake"
)

// Keep the original endpoint available for its archived account. The new
// account has no copy of the original plan, wallet funds or payment history.
func replaceSolanaAccount(t *testing.T, s *solanaShop) *solanafake.Node {
	t.Helper()
	rt := engine.Graph(s.w.rt).Runtime
	mid := s.w.client[embedded].MerchantID()
	next, err := rt.MerchantConfig.Get(t.Context(), mid)
	require.NoError(t, err)
	next = next.Clone()
	old := next.PSPs["solana"]
	old.Value.Archived = true
	old.Value.Secrets = map[string]string{"rpc_url": s.fake.URL()}
	next.PSPs["solana"] = old
	other := solanafake.New()
	t.Cleanup(other.Close)
	other.SetNetwork("testnet")
	other.SetClock(s.w.clock.Now)
	replacement := old
	replacement.Value.Archived = false
	replacement.Value.Environment = "testnet"
	replacement.Value.AccountID = solanago.NewWallet().PublicKey().String()
	replacement.Value.Secrets = map[string]string{"rpc_url": other.URL()}
	next.PSPs["replacement"] = replacement
	rt.MerchantConfig.Source().(*merchantdocs.FileSource).Put(mid, next)
	served, err := rt.MerchantConfig.Reload(t.Context(), mid)
	require.NoError(t, err)
	require.Empty(t, served.Rejected)
	return other
}

func TestSolanaPendingSubscriptionKeepsOriginalAccount(t *testing.T) {
	s := openSolanaShop(t)
	b := s.buyer(t, false)
	order := s.order(t, b)
	other := replaceSolanaAccount(t, s)
	rt := engine.Graph(s.w.rt).Runtime
	mid := s.w.client[embedded].MerchantID()
	ctx := db.WithPSPID(merchant.WithID(t.Context(), mid), s.w.psp["solana"].UUID())
	require.Equal(t, "devnet", rt.SolanaRPCResolver.Network(ctx))
	require.Equal(t, "testnet", rt.SolanaRPCResolver.Network(merchant.WithID(t.Context(), mid)))

	status, encoded := s.request(t, order, b.wallet.PublicKey())
	require.Equal(t, http.StatusOK, status, encoded)
	tx, err := solanago.TransactionFromBase64(encoded)
	require.NoError(t, err)
	purchase := s.offer(t, order)
	signature := s.land(t, signAs(t, tx, b.wallet), s.w.clock.Now())
	done, err := b.confirm(purchase, signature)
	require.NoError(t, err)
	require.NotNil(t, done.SubscriptionID)
	require.Equal(t, s.w.psp["solana"], s.w.subscription(embedded, *done.SubscriptionID).PSPID)
	require.True(t, b.entitled(s.key))

	// Confirmation is repeatable, and renewal stays on the original account.
	_, err = b.confirm(purchase, signature)
	require.NoError(t, err)
	require.Len(t, completed(s.w.payments(embedded, b.id)), 1)
	s.w.advance(monthHours*time.Hour + time.Hour)
	s.crankPass(t)
	s.w.until(func() bool { return len(completed(s.w.payments(embedded, b.id))) == 2 }, "original account renews")
	require.Equal(t, uint64(2*solanaPlanAmount), s.fake.Balance(s.treasury, s.mint))
	require.Zero(t, other.Transactions(s.cranker.PublicKey()), "the replacement network receives no payment")

	status, body := b.cancel(*done.SubscriptionID, map[string]any{"reason": "done", "revoke_on_chain": true})
	require.Equal(t, http.StatusOK, status, "%v", body)
	next := body["next_action"].(map[string]any)
	transactions := next["transactions"].([]any)
	require.Len(t, transactions, 1)
	tx, err = solanago.TransactionFromBase64(transactions[0].(string))
	require.NoError(t, err)
	signature = s.land(t, signAs(t, tx, b.wallet), s.w.clock.Now())
	require.True(t, s.canceledOnChain(t, *done.SubscriptionID))
	status, body = b.cancel(*done.SubscriptionID, map[string]any{"reason": "done", "signature": signature})
	require.Equal(t, http.StatusOK, status, "%v", body)
	require.True(t, b.entitled(s.key), "cancel keeps the paid period")
}

func TestSolanaAccountChangeRecoversRecordedPull(t *testing.T) {
	s := openSolanaShop(t)
	b, sub := s.subscribed(t)
	remove := failpoint.Set(func(_ context.Context, site failpoint.Site) error {
		if site.Kind == "solana_pull" && site.Subscription == sub.UUID() && site.Point == failpoint.AfterProvider {
			return errors.New("stop after the original account's pull landed")
		}
		return nil
	})
	defer remove()
	s.w.advance(monthHours*time.Hour + time.Hour)
	job, err := s.w.jobs.Insert(t.Context(), solanaCrankPass{}, &river.InsertOpts{Queue: openrails.QueueBilling})
	require.NoError(t, err)
	s.w.waitJob(job.Job.ID)
	require.Equal(t, uint64(2*solanaPlanAmount), s.fake.Balance(s.treasury, s.mint))
	require.Len(t, completed(s.w.payments(embedded, b.id)), 1)
	other := replaceSolanaAccount(t, s)
	remove()
	rt := engine.Graph(s.w.rt).Runtime
	mid := s.w.client[embedded].MerchantID()
	source := rt.MerchantConfig.Source().(*merchantdocs.FileSource)
	next, err := rt.MerchantConfig.Get(t.Context(), mid)
	require.NoError(t, err)
	next = next.Clone()
	delete(next.PSPs, "solana")
	source.Put(mid, next)
	_, err = rt.MerchantConfig.Reload(t.Context(), mid)
	require.NoError(t, err)

	// The declaration is gone, but the persisted account still identifies
	// devnet. Its public endpoint can recover the original receipt.
	ctx := db.WithPSPID(merchant.WithID(t.Context(), mid), s.w.psp["solana"].UUID())
	client, err := rt.SolanaRPCResolver.Resolve(ctx, mid)
	require.NoError(t, err)
	require.Equal(t, "devnet", client.Network())
	require.Equal(t, "devnet", rt.SolanaRPCResolver.Network(ctx))
	s.w.advance(12 * time.Hour)
	s.w.wake()
	s.w.until(func() bool { return len(completed(s.w.payments(embedded, b.id))) == 2 }, "original receipt settles")
	s.crankPass(t)
	require.Equal(t, uint64(2*solanaPlanAmount), s.fake.Balance(s.treasury, s.mint), "recovery never pulls again")
	require.Zero(t, other.Transactions(s.cranker.PublicKey()))
	require.Equal(t, billing.SubscriptionStatus("active"), s.w.subscription(embedded, sub).Status)
	require.True(t, b.entitled(s.key))
}

func TestSolanaPinnedAccountNeverFallsBack(t *testing.T) {
	s := openSolanaShop(t)
	rt := engine.Graph(s.w.rt).Runtime
	mid := s.w.client[embedded].MerchantID()
	ctx := merchant.WithID(t.Context(), mid)
	for _, tc := range []struct {
		name string
		id   uuid.UUID
	}{
		{"missing", uuid.New()},
		{"other rail", s.w.psp["stripe"].UUID()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pinned := db.WithPSPID(ctx, tc.id)
			client, err := rt.SolanaRPCResolver.Resolve(pinned, mid)
			require.Error(t, err)
			require.Nil(t, client)
			network, tokens := rt.SolanaRPCResolver.RecurringTokens(pinned)
			require.Empty(t, network)
			require.Empty(t, tokens)
		})
	}
	other := billing.MerchantID(uuid.New())
	_, _, err := rt.Merchants.RegisterForRestore(t.Context(), other, "solana-other-"+uuid.NewString()[:8])
	require.NoError(t, err)
	client, err := rt.SolanaRPCResolver.Resolve(db.WithPSPID(merchant.WithID(t.Context(), other), s.w.psp["solana"].UUID()), other)
	require.Error(t, err)
	require.Nil(t, client, "another merchant cannot resolve this account")
}

func TestSolanaAccountNetworkCannotBeRewritten(t *testing.T) {
	s := openSolanaShop(t)
	b, sub := s.subscribed(t)
	rt := engine.Graph(s.w.rt).Runtime
	mid := s.w.client[embedded].MerchantID()
	id := s.w.psp["solana"].UUID()
	ctx := merchant.WithID(t.Context(), mid)
	source := rt.MerchantConfig.Source().(*merchantdocs.FileSource)
	before, err := rt.MerchantConfig.Get(ctx, mid)
	require.NoError(t, err)
	next := before.Clone()
	item := next.PSPs["solana"]
	item.Value.Environment = "testnet"
	next.PSPs["solana"] = item
	source.Put(mid, next)
	served, err := rt.MerchantConfig.Reload(ctx, mid)
	require.NoError(t, err)
	require.Contains(t, served.Rejected[merchantdocs.PSPDoc("solana")], "network is fixed at devnet")
	require.Equal(t, "devnet", served.PSPs["solana"].Value.Environment)
	scope, ok, err := rt.Merchants.PSPScopeByID(ctx, mid, id)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "devnet", scope.Environment)

	// The database guard also covers a registration racing the preflight.
	require.NoError(t, rt.DB.RunInMerchantScope(ctx, mid, "check fixed Solana network", func(ctx context.Context) error {
		_, err := rt.DB.Gen(ctx).RegisterPSP(ctx, gen.RegisterPSPParams{ID: id, MerchantID: mid.UUID(), Key: "solana", Rail: "solana", Environment: "testnet", AccountID: s.treasury.String()})
		require.ErrorIs(t, err, pgx.ErrNoRows)
		row, err := rt.DB.Gen(ctx).GetPSP(ctx, gen.GetPSPParams{MerchantID: mid.UUID(), ID: id})
		require.NoError(t, err)
		require.Equal(t, "devnet", row.Environment)
		// A new process has no previous served document to fall back to.
		fresh, err := rt.Merchants.SyncConfig(ctx, mid, merchantdocs.Set{}, next)
		require.NoError(t, err)
		require.Contains(t, fresh.Rejected[merchantdocs.PSPDoc("solana")], "network is fixed at devnet")
		require.NotContains(t, fresh.PSPs, "solana")
		return nil
	}))

	// Rotating the endpoint on the same network is still accepted.
	item.Value.Environment = "devnet"
	item.Value.Secrets = map[string]string{"rpc_url": s.fake.URL() + "/rotated-endpoint"}
	next.PSPs["solana"] = item
	source.Put(mid, next)
	served, err = rt.MerchantConfig.Reload(ctx, mid)
	require.NoError(t, err)
	require.Empty(t, served.Rejected)
	s.w.advance(monthHours*time.Hour + time.Hour)
	s.crankPass(t)
	s.w.until(func() bool { return len(completed(s.w.payments(embedded, b.id))) == 2 }, "network rejection preserves renewal")
	require.Equal(t, billing.SubscriptionStatus("active"), s.w.subscription(embedded, sub).Status)
}
