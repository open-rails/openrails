//go:build e2e && integration

package subscriptions_test

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/integrations/solana/subscriptions"
	"github.com/open-rails/openrails/internal/solanafake"
)

// crankPass runs one Solana crank pass and wakes the world.
func (s *solanaShop) crankPass(t *testing.T) {
	t.Helper()
	res, err := s.w.jobs.Insert(t.Context(), solanaCrankPass{}, &river.InsertOpts{Queue: openrails.QueueBilling})
	require.NoError(t, err)
	s.w.waitJob(res.Job.ID)
	s.w.wake()
}

// subscribe has a new buyer subscribe to the shop's plan with its first
// payment.
func (s *solanaShop) subscribe(t *testing.T) billing.SubscriptionID {
	t.Helper()
	b := s.buyer(t, false)
	c := s.purchase(t, b, b.wallet.PublicKey())
	done, err := b.confirm(c, s.land(t, signAs(t, c.bundle, b.wallet), s.w.clock.Now()))
	require.NoError(t, err)
	require.NotNil(t, done.SubscriptionID)
	return *done.SubscriptionID
}

// probe is the engine's named optional-dependency check.
func (w *world) probe(name string) error {
	w.t.Helper()
	for _, p := range w.rt.Probes() {
		if p.Name == name {
			return p.Check(w.t.Context())
		}
	}
	w.t.Fatalf("probe %s not registered", name)
	return nil
}

// planOn is the price's bound plan on chain.
func planOn(t *testing.T, fake *solanafake.Node, price *billing.Price) *subscriptions.PlanAccount {
	t.Helper()
	require.NotNil(t, price.SolanaPlan, "price %s sells on a published plan", price.ID)
	raw, ok := fake.Account(solanago.MustPublicKeyFromBase58(*price.SolanaPlan))
	require.True(t, ok, "price %s binds a plan on chain", price.ID)
	plan, err := subscriptions.DecodePlanAccount(raw)
	require.NoError(t, err)
	return plan
}

// Crankers are listed in order: a cranker listed first publishes new plans,
// and the cranker that owns a plan keeps pulling it while it stays listed.
func TestSolanaCrankersKeepPullingThePlansTheyOwn(t *testing.T) {
	s := openSolanaShop(t)
	w, sol := s.w, s.sol
	s.subscribe(t)
	require.Equal(t, uint64(solanaPlanAmount), s.fake.Balance(s.treasury, s.mint))

	newer := solanago.NewWallet().PrivateKey
	sol.crankers = []solanago.PrivateKey{newer, s.cranker}
	w.restart()
	w.advance(monthHours*time.Hour + time.Hour)
	s.crankPass(t)
	w.until(func() bool { return s.fake.Balance(s.treasury, s.mint) == 2*solanaPlanAmount }, "the old cranker pulls the plan it owns")

	key, err := w.applyCatalog(`  "{key}-monthly":
    currency: usd
    unit_amount: 7000000
    billing_interval_hours: 720
    access_duration_hours: 720
`)
	require.NoError(t, err)
	price, err := priceByKey(t.Context(), w.client[embedded], key, key+"-monthly")
	require.NoError(t, err)
	plan := planOn(t, s.fake, price)
	require.Equal(t, newer.PublicKey(), plan.Owner, "the first listed cranker publishes new plans")
	require.Equal(t, [4]solanago.PublicKey{s.treasury}, plan.Destinations)
	require.Zero(t, s.fake.Balance(newer.PublicKey(), s.mint))
	require.Zero(t, s.fake.Balance(s.cranker.PublicKey(), s.mint))
}

// A plan whose owner no listed cranker holds cannot be pulled: the
// deployment warns with the subscriptions and plans affected, the renewal
// fails without holding it against the subscriber, and the plan is no longer
// sold. Listed again, the cranker pulls it.
func TestSolanaUnlistedCrankerWarnsAndPullsNothing(t *testing.T) {
	s := openSolanaShop(t)
	w, sol := s.w, s.sol
	sub := s.subscribe(t)
	old := s.cranker
	newer := solanago.NewWallet().PrivateKey

	sol.crankers = []solanago.PrivateKey{newer}
	w.restart()
	err := w.probe("openrails_solana_crankers")
	require.ErrorContains(t, err, fmt.Sprintf("1 active subscriptions on 1 plans are owned by %s", old.PublicKey()))
	require.ErrorContains(t, err, "no listed Solana cranker owns the plan")

	again := s.buyer(t, false)
	created := again.order(http.MethodPost, "/orders", "sol-"+uuid.NewString(), map[string]any{"lines": []any{map[string]any{"price_id": s.price}}})
	require.Equal(t, http.StatusCreated, created.status, "%v", created.body)
	paid := again.order(http.MethodPost, "/orders/"+created.body["id"].(string)+"/pay", "sol-"+uuid.NewString(),
		map[string]any{"payment": map[string]any{"token_symbol": "USDC"}, "expected_total": created.body["total"]})
	require.Equal(t, http.StatusBadRequest, paid.status, "a plan no listed cranker owns is not sold: %v", paid.body)

	w.advance(monthHours*time.Hour + time.Hour)
	s.crankPass(t)
	require.Equal(t, uint64(solanaPlanAmount), s.fake.Balance(s.treasury, s.mint), "nothing is pulled")
	require.Equal(t, billing.SubscriptionStatus("active"), w.subscription(embedded, sub).Status, "the deployment's failure is never the subscriber's")

	sol.crankers = []solanago.PrivateKey{newer, old}
	w.restart()
	require.NoError(t, w.probe("openrails_solana_crankers"))
	s.crankPass(t)
	w.until(func() bool { return s.fake.Balance(s.treasury, s.mint) == 2*solanaPlanAmount }, "listed again, the cranker pulls its plan")
}

// A treasury is frozen into each plan when it is published. Changing the
// Solana PSP's treasury applies to new plans: existing subscriptions keep
// paying the treasury their plan was published with.
func TestSolanaTreasuryChangeTakesNewPlansOnly(t *testing.T) {
	w := prepareWorld(t, 12)
	sol := withSolana(t, w)
	w.start()
	ctx := t.Context()
	product := "treasury-" + uuid.NewString()[:8]
	apply := func() *billing.Price {
		t.Helper()
		params, err := catalog.ParseApplicationYAML(fmt.Appendf(nil, `schema_version: 1
products:
  %[1]s:
    display_name: Treasury
    entitlements: ["%[1]s"]
    prices:
      monthly:
        currency: usd
        unit_amount: %[2]d
        billing_interval_hours: 720
        access_duration_hours: 720
`, product, solanaPlanAmount))
		require.NoError(t, err)
		_, err = w.client[embedded].ApplyCatalog(ctx, params, billing.ApplyCatalogParams{})
		require.NoError(t, err)
		price, err := priceByKey(ctx, w.client[embedded], product, "monthly")
		require.NoError(t, err)
		return price
	}
	mint := solanago.MustPublicKeyFromBase58(solanafake.DevnetUSDCMint)
	first := sol.treasury
	price := apply()
	require.Equal(t, [4]solanago.PublicKey{first}, planOn(t, sol.fake, price).Destinations)
	s := &solanaShop{w: w, sol: sol, fake: sol.fake, cranker: sol.cranker, treasury: first, mint: mint, price: price.ID.String(), key: product}
	s.subscribe(t)
	require.Equal(t, uint64(solanaPlanAmount), sol.fake.Balance(first, mint))

	second := solanago.NewWallet().PublicKey()
	sol.fake.Fund(second, mint, 0)
	sol.treasury = second
	w.restart()
	price = apply()
	require.Equal(t, [4]solanago.PublicKey{second}, planOn(t, sol.fake, price).Destinations, "a new plan pays the new treasury")
	s.subscribe(t)
	require.Equal(t, uint64(solanaPlanAmount), sol.fake.Balance(second, mint), "a new subscription pays the new treasury")

	w.advance(monthHours*time.Hour + time.Hour)
	s.crankPass(t)
	w.until(func() bool {
		return sol.fake.Balance(first, mint) == 2*solanaPlanAmount && sol.fake.Balance(second, mint) == 2*solanaPlanAmount
	}, "each subscription renews into the treasury its plan was published with")
}
