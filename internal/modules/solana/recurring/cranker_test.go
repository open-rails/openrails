package recurring

import (
	"context"
	"testing"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/models"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/integrations/solana/subscriptions"
	solanatokens "github.com/open-rails/openrails/internal/modules/solana/tokens"
	"github.com/open-rails/openrails/internal/solanafake"
)

// One deployment cranker serves every merchant: it owns each merchant's plan,
// whose only destination is that merchant's treasury, and pulls each
// subscriber's payment there. It can pull into nothing else: a pull aimed at
// another treasury is refused on-chain.
func TestOneCrankerServesTwoMerchants(t *testing.T) {
	ctx := context.Background()
	fake := solanafake.New()
	t.Cleanup(fake.Close)
	mint := solanago.MustPublicKeyFromBase58(solanafake.DevnetUSDCMint)
	cranker := randKey(t)
	rpc := solanaint.NewRPCClientWithConfig(solanaint.RPCClientConfig{PublicURL: fake.URL(), Network: "devnet"})
	submitter := NewSignerSubmitter(NewCrankers(nil, Cranker{Name: "one", Signer: solanaint.NewKeypairSigner(cranker)}), func(context.Context, billing.MerchantID) (*solanaint.RPCClient, error) { return rpc, nil })
	merchants := map[billing.MerchantID]solanago.PublicKey{
		billing.MerchantID(uuid.New()): randKey(t).PublicKey(),
		billing.MerchantID(uuid.New()): randKey(t).PublicKey(),
	}
	treasury := func(_ context.Context, mid billing.MerchantID) (solanago.PublicKey, error) {
		return merchants[mid], nil
	}
	plans := NewPlanServiceWithReader(submitter, rpc, NetworkTokens("devnet", solanatokens.Accepted("devnet")), treasury)
	crank := NewCrankService(submitter)

	rows := map[billing.MerchantID]*models.SolanaSubscription{}
	var planID uint64
	for mid, treasury := range merchants {
		planID++
		handle, err := plans.PublishPlan(ctx, PublishPlanInput{MerchantID: mid, PlanID: planID, TokenSymbol: "USDC", AmountBaseUnits: 5_000_000, AmountDecimals: 6, PeriodHours: 720})
		require.NoError(t, err)
		require.Equal(t, cranker.PublicKey().String(), handle.MerchantAddress, "the cranker owns every plan")
		raw, ok := fake.Account(solanago.MustPublicKeyFromBase58(handle.PlanPDA))
		require.True(t, ok)
		plan, err := subscriptions.DecodePlanAccount(raw)
		require.NoError(t, err)
		require.Equal(t, [4]solanago.PublicKey{treasury}, plan.Destinations, "the merchant's treasury is the plan's only destination")

		subscriber := randKey(t).PublicKey()
		fake.Fund(subscriber, mint, 20_000_000)
		planPDA := solanago.MustPublicKeyFromBase58(handle.PlanPDA)
		sub, _, err := subscriptions.DeriveSubscriptionPDA(planPDA, subscriber)
		require.NoError(t, err)
		authority, _, err := subscriptions.DeriveSubscriptionAuthority(subscriber, mint)
		require.NoError(t, err)
		rows[mid] = &models.SolanaSubscription{MerchantID: mid.UUID(), SubscriberWallet: subscriber.String(), AuthorityPDA: authority.String(),
			SubscriptionPDA: sub.String(), PlanPDA: handle.PlanPDA, MerchantAddress: handle.MerchantAddress, Treasury: treasury.String(), Mint: mint.String()}
	}

	for mid, row := range rows {
		_, err := crank.Crank(ctx, mid, row, 5_000_000)
		require.NoError(t, err)
		require.Equal(t, uint64(5_000_000), fake.Balance(merchants[mid], mint), "the pull lands in its own merchant's treasury")
	}
	require.Zero(t, fake.Balance(cranker.PublicKey(), mint), "nothing lands with the cranker")

	// Aimed at the other merchant's treasury, a pull is refused on-chain.
	var first, second billing.MerchantID
	for mid := range rows {
		if first.IsZero() {
			first = mid
		} else {
			second = mid
		}
	}
	redirected := *rows[first]
	redirected.Treasury = merchants[second].String()
	_, err := crank.Crank(ctx, first, &redirected, 5_000_000)
	require.Error(t, err)
	require.Equal(t, uint64(5_000_000), fake.Balance(merchants[second], mint), "the other treasury received nothing more")
	require.Equal(t, uint64(5_000_000), fake.Balance(merchants[first], mint))
}

// memoryPins pins crankers in memory, as billing.solana_crankers does.
type memoryPins map[string]solanago.PublicKey

func (p memoryPins) Pin(_ context.Context, name string, key solanago.PublicKey) (solanago.PublicKey, error) {
	if pinned, ok := p[name]; ok {
		return pinned, nil
	}
	p[name] = key
	return key, nil
}

// rotatingSigner is a Transit key whose public key an operator can rotate.
type rotatingSigner struct{ key solanago.PrivateKey }

func (r *rotatingSigner) PublicKey(context.Context) (solanago.PublicKey, error) {
	return r.key.PublicKey(), nil
}

func (r *rotatingSigner) SignMessage(_ context.Context, message []byte) (solanago.Signature, error) {
	return r.key.Sign(message)
}

// Crankers are listed in order: the first publishes new plans, and every
// listed cranker keeps pulling the plans it owns. A plan whose owner is no
// longer listed cannot be pulled, which is the deployment's failure, never the
// subscriber's.
func TestCrankersPullThePlansTheyOwn(t *testing.T) {
	ctx := context.Background()
	fake := solanafake.New()
	t.Cleanup(fake.Close)
	mint := solanago.MustPublicKeyFromBase58(solanafake.DevnetUSDCMint)
	rpc := solanaint.NewRPCClientWithConfig(solanaint.RPCClientConfig{PublicURL: fake.URL(), Network: "devnet"})
	resolve := func(context.Context, billing.MerchantID) (*solanaint.RPCClient, error) { return rpc, nil }
	mid, treasury := billing.MerchantID(uuid.New()), randKey(t).PublicKey()
	tokens := NetworkTokens("devnet", solanatokens.Accepted("devnet"))
	old, current := randKey(t), randKey(t)
	cranker := func(name string, key solanago.PrivateKey) Cranker {
		return Cranker{Name: name, Signer: solanaint.NewKeypairSigner(key)}
	}
	subscribe := func(handle *PlanHandle) *models.SolanaSubscription {
		subscriber := randKey(t).PublicKey()
		fake.Fund(subscriber, mint, 20_000_000)
		plan := solanago.MustPublicKeyFromBase58(handle.PlanPDA)
		sub, _, err := subscriptions.DeriveSubscriptionPDA(plan, subscriber)
		require.NoError(t, err)
		authority, _, err := subscriptions.DeriveSubscriptionAuthority(subscriber, mint)
		require.NoError(t, err)
		return &models.SolanaSubscription{MerchantID: mid.UUID(), SubscriberWallet: subscriber.String(), AuthorityPDA: authority.String(),
			SubscriptionPDA: sub.String(), PlanPDA: handle.PlanPDA, MerchantAddress: handle.MerchantAddress, Treasury: handle.Treasury, Mint: mint.String()}
	}
	publish := func(submitter Submitter, planID uint64) *PlanHandle {
		handle, err := NewPlanServiceWithReader(submitter, rpc, tokens, treasuryOf(treasury)).PublishPlan(ctx, PublishPlanInput{
			MerchantID: mid, PlanID: planID, TokenSymbol: "USDC", AmountBaseUnits: 5_000_000, AmountDecimals: 6, PeriodHours: 720})
		require.NoError(t, err)
		return handle
	}

	before := NewSignerSubmitter(NewCrankers(nil, cranker("old", old)), resolve)
	oldPlan := publish(before, 1)
	require.Equal(t, old.PublicKey().String(), oldPlan.MerchantAddress)
	oldSub := subscribe(oldPlan)

	// A new cranker listed first publishes new plans; the old one still pulls
	// its own.
	both := NewSignerSubmitter(NewCrankers(nil, cranker("current", current), cranker("old", old)), resolve)
	newPlan := publish(both, 2)
	require.Equal(t, current.PublicKey().String(), newPlan.MerchantAddress, "the first listed cranker publishes")
	newSub := subscribe(newPlan)
	crank := NewCrankService(both)
	_, err := crank.Crank(ctx, mid, oldSub, 5_000_000)
	require.NoError(t, err, "the old cranker pulls the plan it owns")
	_, err = crank.Crank(ctx, mid, newSub, 5_000_000)
	require.NoError(t, err)
	require.Equal(t, uint64(10_000_000), fake.Balance(treasury, mint))

	// Unlisted, the old cranker's plans cannot be pulled: an operational
	// failure, not the subscriber's.
	_, err = NewCrankService(NewSignerSubmitter(NewCrankers(nil, cranker("current", current)), resolve)).Crank(ctx, mid, oldSub, 5_000_000)
	require.ErrorIs(t, err, ErrUnheldPlan)
	require.Equal(t, Operational, ClassifyCrankError(err).Category)
	require.Equal(t, uint64(10_000_000), fake.Balance(treasury, mint))
}

// A cranker is pinned to its first public key: a Vault Transit rotation that
// changes it is refused instead of silently becoming another cranker.
func TestCrankerIsPinnedToItsFirstKey(t *testing.T) {
	ctx := context.Background()
	pins := memoryPins{}
	transit := &rotatingSigner{key: randKey(t)}
	first := transit.key.PublicKey()
	crankers := NewCrankers(pins, Cranker{Name: "vault_transit:openrails-solana", Signer: transit})
	_, key, err := crankers.Publisher(ctx)
	require.NoError(t, err)
	require.Equal(t, first, key)
	require.Equal(t, first, pins["vault_transit:openrails-solana"])

	transit.key = randKey(t)
	for _, restarted := range []*Crankers{crankers, NewCrankers(pins, Cranker{Name: "vault_transit:openrails-solana", Signer: transit})} {
		_, _, err = restarted.Publisher(ctx)
		var changed *CrankerKeyChangedError
		require.ErrorAs(t, err, &changed)
		require.Equal(t, first, changed.Pinned)
		require.ErrorContains(t, err, "the key it was pinned to")
		_, err = restarted.Owner(ctx, first)
		require.ErrorAs(t, err, &changed, "a plan it owns is not pulled with another key")
		require.True(t, IsOperationalFailure(err))
	}
}
