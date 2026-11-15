//go:build integration

package critical_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/solanafake"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"
)

// planTier adds a plan to the subscribed product's tier group: gold costs
// more per hour than the 10 USD weekly plan, basic less.
func (s *solanaWorld) planTier(t *testing.T, sub *billing.Subscription, key string, rank int, amount int64, hours int) *billing.Price {
	t.Helper()
	_, err := s.alpha.UpdateProduct(t.Context(), sub.ProductID, billing.UpdateProductParams{TierGroup: catalog.Value("chain-plans"), TierRank: catalog.Value(2)})
	require.NoError(t, err)
	product, err := s.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "chain-" + key, DisplayName: "Chain " + key, Ownership: catalog.OwnershipUnique,
		TierGroup: ptr("chain-plans"), TierRank: rank, Entitlements: []string{"chain:" + key}})
	require.NoError(t, err)
	price, err := s.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, UnitAmount: amount, Currency: "USD", SellVia: []string{billing.SellViaSolana},
		BillingIntervalHours: ptr(hours), AccessDurationHours: ptr(hours)})
	require.NoError(t, err)
	return price
}

// prepareChange asks for the plan change and signs its transaction as the
// subscriber's wallet does. Nothing is sent.
func (s *solanaWorld) prepareChange(t *testing.T, sub *billing.Subscription, price *billing.Price) (billing.SubscriptionChange, *solanago.Transaction) {
	t.Helper()
	status, raw := s.request(t, http.MethodPost, "/v1/me/subscriptions/"+sub.ID.String()+"/change", s.aliceToken, map[string]any{"price_id": price.ID})
	require.Equal(t, http.StatusOK, status, string(raw))
	var change billing.SubscriptionChange
	require.NoError(t, json.Unmarshal(raw, &change))
	require.Equal(t, "requires_action", change.Status)
	require.NotNil(t, change.NextAction)
	require.Equal(t, "solana_sign_transactions", change.NextAction.Type)
	require.Len(t, change.NextAction.Transactions, 1)
	tx, err := solanago.TransactionFromBase64(change.NextAction.Transactions[0])
	require.NoError(t, err)
	_, err = tx.PartialSign(func(key solanago.PublicKey) *solanago.PrivateKey {
		if key.Equals(s.payer.PublicKey()) {
			return &s.payer.PrivateKey
		}
		return nil
	})
	require.NoError(t, err)
	require.NoError(t, tx.VerifySignatures())
	return change, tx
}

// confirmChange repeats the change request with the landed signature.
func (s *solanaWorld) confirmChange(t *testing.T, sub *billing.Subscription, price *billing.Price, signature string) billing.SubscriptionChange {
	t.Helper()
	status, raw := s.request(t, http.MethodPost, "/v1/me/subscriptions/"+sub.ID.String()+"/change", s.aliceToken, map[string]any{"price_id": price.ID, "signature": signature})
	require.Equal(t, http.StatusOK, status, string(raw))
	var change billing.SubscriptionChange
	require.NoError(t, json.Unmarshal(raw, &change))
	require.Equal(t, "succeeded", change.Status)
	require.NotNil(t, change.SubscriptionID)
	return change
}

// changeOperations are the statuses of the pending-change operations, oldest first.
func (s *solanaWorld) changeOperations(t *testing.T) []string {
	t.Helper()
	rows, err := s.pool.Query(t.Context(), "SELECT status FROM "+pgx.Identifier{s.schema, "provider_intents"}.Sanitize()+" WHERE intent_type = 'solana_tier_change' ORDER BY created_at, id")
	require.NoError(t, err)
	statuses, err := pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(t, err)
	return statuses
}

// look runs the operations due at the scenario's clock, delay later.
func (s *solanaWorld) look(t *testing.T, fleet *river.Client[pgx.Tx], delay time.Duration) {
	t.Helper()
	settleOperations(t, fleet)
	s.clock.Advance(delay)
	wakeOperations(t, fleet)
}

// changedTo waits for the customer's one active subscription to be on price
// and returns it; the subscription changed from is canceled.
func (s *solanaWorld) changedTo(t *testing.T, from *billing.Subscription, price *billing.Price) *billing.Subscription {
	t.Helper()
	var active *billing.Subscription
	require.Eventually(t, func() bool {
		subs, err := s.alpha.ListSubscriptions(t.Context(), billing.SubscriptionListParams{CustomerID: from.CustomerID, Status: billing.SubscriptionActive})
		if err != nil || len(subs.Items) != 1 || subs.Items[0].PriceID != price.ID {
			return false
		}
		active, err = s.alpha.GetSubscription(t.Context(), subs.Items[0].ID)
		return err == nil
	}, 20*time.Second, 20*time.Millisecond, "the landed change is recorded from the chain")
	old, err := s.alpha.GetSubscription(t.Context(), from.ID)
	require.NoError(t, err)
	require.Equal(t, billing.SubscriptionCanceled, old.Status)
	require.NotEqual(t, from.ID, active.ID)
	require.Equal(t, "solana", active.Rail)
	return active
}

func (s *solanaWorld) entitled(t *testing.T, customer billing.CustomerID, entitlement string) bool {
	t.Helper()
	access, err := s.alpha.CheckEntitlements(t.Context(), billing.CheckEntitlementsParams{CustomerID: customer, Entitlements: []string{entitlement}})
	require.NoError(t, err)
	return access.Entitlements[entitlement]
}

// charges are the amounts of the customer's succeeded payments of more than
// nothing that signature settled.
func (s *solanaWorld) charges(t *testing.T, customer billing.CustomerID, signature string) []int64 {
	t.Helper()
	payments, err := s.alpha.ListPayments(t.Context(), billing.PaymentListParams{CustomerID: customer})
	require.NoError(t, err)
	var amounts []int64
	for _, payment := range payments.Items {
		if payment.TransactionID == signature && payment.Amount > 0 {
			require.Equal(t, billing.PaymentSucceeded, payment.Status)
			amounts = append(amounts, payment.Amount)
		}
	}
	return amounts
}

func (s *solanaWorld) unbooked(t *testing.T) []billing.Finding {
	t.Helper()
	page, err := s.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: "consistency.ledger.unbooked"})
	require.NoError(t, err)
	return page.Items
}

func TestSolanaPlanChanges(t *testing.T) {
	usdc := solanago.MustPublicKeyFromBase58(solanafake.DevnetUSDCMint)
	t.Run("OR-J37-S64/an-upgrade-the-wallet-lands-and-never-confirms-is-recorded-from-the-chain", func(t *testing.T) {
		s := newSolanaWorld(t, true)
		_, sub := s.subscribe(t)
		gold := s.planTier(t, sub, "gold", 3, 40_000_000, 336)
		fleet := startFleet(t, s.world)
		settleOperations(t, fleet)

		change, tx := s.prepareChange(t, sub, gold)
		require.Equal(t, "now", change.Effective)
		require.Positive(t, change.AmountDueNow)
		require.Equal(t, []string{"pending"}, s.changeOperations(t), "the change is durable before the wallet signs")
		landedAt := s.clock.Now()
		sig, err := s.node.Land(tx, landedAt)
		require.NoError(t, err)
		// The tab closes: no request names the signature.
		s.look(t, fleet, 10*time.Second)
		upgraded := s.changedTo(t, sub, gold)
		require.Equal(t, landedAt.Add(336*time.Hour), *upgraded.CurrentPeriodEndsAt)
		require.True(t, s.entitled(t, sub.CustomerID, "chain:gold"))
		require.Equal(t, []int64{change.AmountDueNow}, s.charges(t, sub.CustomerID, sig.String()), "the landed transaction is the upgrade's one payment")
		require.EqualValues(t, 10_000_000+change.AmountDueNow, s.node.Balance(s.treasury.PublicKey(), usdc))
		settleOperations(t, fleet)
		require.Equal(t, []string{"succeeded"}, s.changeOperations(t))

		// The old period ends: its pull is not sent, and nothing is revoked.
		sends := s.wire.sends()
		s.look(t, fleet, sub.CurrentPeriodEndsAt.Add(pullMargin).Sub(s.clock.Now()))
		settleOperations(t, fleet)
		require.Equal(t, sends, s.wire.sends(), "the subscription changed from is never pulled again")
		require.True(t, s.entitled(t, sub.CustomerID, "chain:gold"))

		// The new plan is pulled at its own renewal.
		due := *upgraded.CurrentPeriodEndsAt
		s.look(t, fleet, due.Add(pullMargin).Sub(s.clock.Now()))
		require.Eventually(t, func() bool {
			current, err := s.alpha.GetSubscription(t.Context(), upgraded.ID)
			return err == nil && current.CurrentPeriodEndsAt != nil && current.CurrentPeriodEndsAt.After(due)
		}, 20*time.Second, 20*time.Millisecond, "the new plan renews")
		require.EqualValues(t, 50_000_000+change.AmountDueNow, s.node.Balance(s.treasury.PublicKey(), usdc))
		require.True(t, s.entitled(t, sub.CustomerID, "chain:gold"))
		require.Empty(t, s.unbooked(t))
	})
	t.Run("OR-J37-S65/a-downgrade-the-wallet-lands-and-never-confirms-is-recorded-from-the-chain", func(t *testing.T) {
		s := newSolanaWorld(t, true)
		_, sub := s.subscribe(t)
		basic := s.planTier(t, sub, "basic", 1, 5_000_000, 168)
		fleet := startFleet(t, s.world)
		settleOperations(t, fleet)

		change, tx := s.prepareChange(t, sub, basic)
		require.Equal(t, "period_end", change.Effective)
		require.Zero(t, change.AmountDueNow)
		sig, err := s.node.Land(tx, s.clock.Now())
		require.NoError(t, err)
		s.look(t, fleet, 10*time.Second)
		downgraded := s.changedTo(t, sub, basic)
		require.Equal(t, *sub.CurrentPeriodEndsAt, *downgraded.CurrentPeriodEndsAt, "the lower plan takes over when the paid period ends")
		require.True(t, s.entitled(t, sub.CustomerID, "chain:basic"))
		require.Empty(t, s.charges(t, sub.CustomerID, sig.String()), "a downgrade charges nothing")
		require.EqualValues(t, 10_000_000, s.node.Balance(s.treasury.PublicKey(), usdc))

		// At the old period end the new plan is pulled, the old one is not.
		due := *downgraded.CurrentPeriodEndsAt
		sends := s.wire.sends()
		s.look(t, fleet, due.Add(pullMargin).Sub(s.clock.Now()))
		require.Eventually(t, func() bool {
			current, err := s.alpha.GetSubscription(t.Context(), downgraded.ID)
			return err == nil && current.CurrentPeriodEndsAt != nil && current.CurrentPeriodEndsAt.After(due)
		}, 20*time.Second, 20*time.Millisecond, "the lower plan renews")
		settleOperations(t, fleet)
		require.Equal(t, sends+1, s.wire.sends())
		require.EqualValues(t, 15_000_000, s.node.Balance(s.treasury.PublicKey(), usdc))
		require.True(t, s.entitled(t, sub.CustomerID, "chain:basic"))
		require.Empty(t, s.unbooked(t))
	})
	t.Run("OR-J37-S66/a-prepared-change-that-never-landed-is-released", func(t *testing.T) {
		s := newSolanaWorld(t, true)
		_, sub := s.subscribe(t)
		gold := s.planTier(t, sub, "gold", 3, 40_000_000, 336)
		fleet := startFleet(t, s.world)
		settleOperations(t, fleet)

		// The wallet is shown the transaction and never sends it.
		s.prepareChange(t, sub, gold)
		s.look(t, fleet, 10*time.Second)
		settleOperations(t, fleet)
		require.Equal(t, []string{"pending"}, s.changeOperations(t), "its blockhash can still land: the change stays pending")

		// The cluster finalizes past the last block that could include it.
		s.node.AdvanceBlocks(200)
		s.look(t, fleet, time.Minute)
		require.Eventually(t, func() bool {
			states := s.changeOperations(t)
			return len(states) == 1 && states[0] == "superseded"
		}, 20*time.Second, 20*time.Millisecond, "a change proven never to have landed is released")
		current, err := s.alpha.GetSubscription(t.Context(), sub.ID)
		require.NoError(t, err)
		require.Equal(t, billing.SubscriptionActive, current.Status)
		require.Equal(t, sub.PriceID, current.PriceID)
		require.False(t, s.entitled(t, sub.CustomerID, "chain:gold"))

		// The subscription renews on its own plan as if nothing was asked.
		due := *sub.CurrentPeriodEndsAt
		s.look(t, fleet, due.Add(pullMargin).Sub(s.clock.Now()))
		s.renewed(t, sub, s.clock.Now().Add(168*time.Hour))
	})
	t.Run("OR-J37-S67/a-confirm-that-arrives-and-the-chain-read-record-one-change", func(t *testing.T) {
		s := newSolanaWorld(t, true)
		_, sub := s.subscribe(t)
		gold := s.planTier(t, sub, "gold", 3, 40_000_000, 336)
		fleet := startFleet(t, s.world)
		settleOperations(t, fleet)

		change, tx := s.prepareChange(t, sub, gold)
		sig, err := s.node.Land(tx, s.clock.Now())
		require.NoError(t, err)
		confirmed := s.confirmChange(t, sub, gold, sig.String())
		require.Equal(t, change.AmountDueNow, confirmed.AmountDueNow)
		upgraded := s.changedTo(t, sub, gold)
		require.Equal(t, upgraded.ID, *confirmed.SubscriptionID)

		// The operation reads the chain afterwards and finds the change recorded.
		s.look(t, fleet, 10*time.Second)
		require.Eventually(t, func() bool {
			states := s.changeOperations(t)
			return len(states) == 1 && states[0] == "succeeded"
		}, 20*time.Second, 20*time.Millisecond, "the chain read settles the same change")
		settleOperations(t, fleet)
		// A confirm arriving after the chain read answers the same change.
		again := s.confirmChange(t, sub, gold, sig.String())
		require.Equal(t, *confirmed.SubscriptionID, *again.SubscriptionID)
		s.changedTo(t, sub, gold)
		require.Equal(t, []int64{change.AmountDueNow}, s.charges(t, sub.CustomerID, sig.String()), "one upgrade charge")
		require.EqualValues(t, 10_000_000+change.AmountDueNow, s.node.Balance(s.treasury.PublicKey(), usdc))
		require.Empty(t, s.unbooked(t))
	})
	t.Run("OR-J37-S68/a-pull-the-old-subscription-refuses-while-its-change-is-pending-cancels-nothing", func(t *testing.T) {
		s := newSolanaWorld(t, true)
		_, sub := s.subscribe(t)
		gold := s.planTier(t, sub, "gold", 3, 40_000_000, 336)
		fleet := startFleet(t, s.world)
		settleOperations(t, fleet)

		// The change lands and no finalized read shows it yet; no request
		// names its signature.
		_, tx := s.prepareChange(t, sub, gold)
		sig, err := s.node.LandConfirmed(tx, s.clock.Now())
		require.NoError(t, err)

		// The old period ends: the chain refuses the old subscription's pull
		// as canceled, which the change did.
		sends := s.wire.sends()
		s.look(t, fleet, sub.CurrentPeriodEndsAt.Add(pullMargin).Sub(s.clock.Now()))
		require.Eventually(t, func() bool { return s.wire.sends() > sends }, 20*time.Second, 20*time.Millisecond, "the old subscription's pull is sent and refused")
		settleOperations(t, fleet)
		current, err := s.alpha.GetSubscription(t.Context(), sub.ID)
		require.NoError(t, err)
		require.Equal(t, billing.SubscriptionActive, current.Status, "a refusal while a plan change is pending is no cancellation")
		require.True(t, s.entitled(t, sub.CustomerID, "chain:paid"))
		require.Equal(t, []string{"pending"}, s.changeOperations(t))

		// The change finalizes and is recorded; the customer kept access throughout.
		s.node.Finalize(sig.String())
		s.look(t, fleet, 10*time.Minute)
		s.changedTo(t, sub, gold)
		require.True(t, s.entitled(t, sub.CustomerID, "chain:gold"))
		settleOperations(t, fleet)
		require.Empty(t, s.unbooked(t))
	})
}
