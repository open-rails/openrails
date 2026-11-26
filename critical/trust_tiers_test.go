//go:build integration

package critical_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
)

// tieredSales is a world of $10 Stripe sales whose merchant graduates its
// customers by tiers: the default tier spends $5 a month, a graduated one
// $50. Alice and Bob are funded to spend; credit grants are no payment.
func tieredSales(t *testing.T, tiers ...billing.TrustTier) *sales {
	t.Helper()
	s := newSales(t, func(w *world) {
		m := cfgMerchant(w.cfg, "alpha")
		m.BillingPolicies = []billing.BillingPolicy{
			{Name: "starter", SpendWindows: []billing.BudgetWindow{{Key: "starter-month", WindowSeconds: 30 * 24 * 3600, Limit: 5_000_000, Currency: "USD"}}},
			{Name: "graduated", SpendWindows: []billing.BudgetWindow{{Key: "graduated-month", WindowSeconds: 30 * 24 * 3600, Limit: 50_000_000, Currency: "USD"}}},
		}
		m.BillingPolicyBindings = []billing.BillingPolicyBinding{{PolicyName: "starter"}}
		for _, tier := range tiers {
			m.BillingPolicyBindings = append(m.BillingPolicyBindings, billing.BillingPolicyBinding{PolicyName: "graduated", Tier: tier.Name})
		}
		m.TrustTiers = tiers
		setCfgMerchant(&w.cfg, "alpha", m)
	})
	for _, c := range []billing.CustomerID{alice(s), bob(s)} {
		fund(t, s.a.alpha, c, "funded-"+c.String(), 1_000_000_000)
	}
	return s
}

// freeTier is the default tier.
const freeTier = "free"

func alice(s *sales) billing.CustomerID { return billing.CustomerID(uuid.MustParse(s.a.alice.ID)) }
func bob(s *sales) billing.CustomerID   { return billing.CustomerID(uuid.MustParse(s.a.bob.ID)) }

func usdTier(name string, netPaid int64, days int) billing.TrustTier {
	return billing.TrustTier{Name: name, Requirements: []billing.TrustTierRequirement{{Currency: "USD", NetPaidAmount: netPaid, DaysSinceFirstPayment: days}}}
}

// requireTrust asserts a customer's effective USD trust tier, where it came
// from and the net amount paid the rules read.
func requireTrust(t *testing.T, s *sales, customer billing.CustomerID, level string, source billing.TrustLevelSource, netPaid int64) billing.EffectiveTrustLevel {
	t.Helper()
	c, err := s.a.alpha.GetCustomer(t.Context(), customer)
	require.NoError(t, err)
	for _, l := range c.TrustLevels {
		if l.Currency == "USD" {
			require.Equal(t, level, l.TrustLevel)
			require.Equal(t, source, l.Source)
			require.EqualValues(t, netPaid, l.NetPaidAmount)
			return l
		}
	}
	t.Fatalf("customer %s has no USD trust level: %+v", customer, c.TrustLevels)
	return billing.EffectiveTrustLevel{}
}

// requireGraduated asserts which policy a gate decides a $20 request
// against: the graduated tier's $50 window admits it, the default's $5
// refuses it.
func requireGraduated(t *testing.T, s *sales, customer billing.CustomerID, id string, graduated bool) {
	t.Helper()
	d := admitOne(t, s.a.alpha, reservation(customer, id, 20_000_000))
	if graduated {
		require.True(t, d.Allowed(), "the graduated tier's window admits it")
		d.release(t)
		return
	}
	requireDenied(t, d, billing.DenyBudgetExceeded)
}

// viewSets are the limit sets a gate's sync resolves for each customer's own
// requests in USD, naming level when it is set.
func viewSets(t *testing.T, s *sales, level *string, customers ...billing.CustomerID) []*billing.LimitSet {
	t.Helper()
	var views []billing.ViewParams
	for _, c := range customers {
		views = append(views, billing.ViewParams{CustomerID: c, Invoker: c.String(), InvokerType: billing.InvokerTypeCustomer, TrustLevel: level, Roles: []uuid.UUID{}, Currency: "USD"})
	}
	out, err := s.a.alpha.SyncLeases(t.Context(), billing.SyncLeasesParams{LeaseID: billing.LeaseID(uuid.Must(uuid.NewV7())), Views: views})
	require.NoError(t, err)
	require.Len(t, out.LimitSets, len(customers))
	return out.LimitSets
}

// requireSet asserts a limit set's trust tier, where it came from, and the
// capacity its tier's policy gives its one spend window.
func requireSet(t *testing.T, set *billing.LimitSet, level string, source billing.TrustLevelSource, capacity int64) {
	t.Helper()
	require.NotNil(t, set)
	require.Nil(t, set.Refusal)
	require.Equal(t, level, set.TrustLevel)
	require.Equal(t, source, set.TrustLevelSource)
	require.Len(t, set.Capacities, 1)
	require.EqualValues(t, capacity, set.Capacities[0].Capacity, "the %s tier's policy", level)
}

func TestTrustTiers(t *testing.T) {
	t.Run("OR-J23-S43/a-customer-graduates-once-its-net-amount-paid-reaches-a-tier", func(t *testing.T) {
		s := tieredSales(t, usdTier("builder", 15_000_000, 0))
		first := s.buy(t, "amount-1")
		paid := requireTrust(t, s, alice(s), freeTier, billing.TrustLevelDefault, 10_000_000)
		require.NotNil(t, paid.FirstPaidAt)
		require.True(t, paid.FirstPaidAt.Equal(first.CreatedAt), "the first payment starts the days")
		requireGraduated(t, s, alice(s), "short", false)
		s.buy(t, "amount-2")
		paid = requireTrust(t, s, alice(s), "builder", billing.TrustLevelRule, 20_000_000)
		require.True(t, paid.FirstPaidAt.Equal(first.CreatedAt), "a later payment keeps the first")
		requireGraduated(t, s, alice(s), "graduated", true)
		requireTrust(t, s, bob(s), freeTier, billing.TrustLevelDefault, 0)
	})

	t.Run("OR-J23-S44/a-tier-asking-days-since-the-first-payment-is-reached-once-they-pass", func(t *testing.T) {
		s := tieredSales(t, usdTier("patient", 0, 7))
		requireTrust(t, s, alice(s), freeTier, billing.TrustLevelDefault, 0)
		s.buy(t, "days-1")
		// Payments come first: none is dated ahead of the database's clock.
		first := *requireTrust(t, s, alice(s), freeTier, billing.TrustLevelDefault, 10_000_000).FirstPaidAt
		advanceTo(s.a.world, first.Add(7*24*time.Hour-time.Minute))
		requireTrust(t, s, alice(s), freeTier, billing.TrustLevelDefault, 10_000_000)
		requireSet(t, viewSets(t, s, nil, alice(s))[0], freeTier, billing.TrustLevelDefault, 5_000_000)
		advanceTo(s.a.world, first.Add(7*24*time.Hour))
		requireTrust(t, s, alice(s), "patient", billing.TrustLevelRule, 10_000_000)
		sets := viewSets(t, s, nil, alice(s), bob(s))
		requireSet(t, sets[0], "patient", billing.TrustLevelRule, 50_000_000)
		requireSet(t, sets[1], freeTier, billing.TrustLevelDefault, 5_000_000)
	})

	t.Run("OR-J23-S45/a-tier-asking-an-amount-and-days-needs-both", func(t *testing.T) {
		s := tieredSales(t, usdTier("veteran", 15_000_000, 7))
		s.buy(t, "both-alice-1")
		s.buyAs(t, s.a.bobToken, "both-bob-1")
		s.buyAs(t, s.a.bobToken, "both-bob-2")
		requireTrust(t, s, alice(s), freeTier, billing.TrustLevelDefault, 10_000_000)
		first := *requireTrust(t, s, bob(s), freeTier, billing.TrustLevelDefault, 20_000_000).FirstPaidAt
		requireGraduated(t, s, bob(s), "paid-not-long", false)
		advanceTo(s.a.world, first.Add(7*24*time.Hour))
		requireTrust(t, s, alice(s), freeTier, billing.TrustLevelDefault, 10_000_000)
		requireTrust(t, s, bob(s), "veteran", billing.TrustLevelRule, 20_000_000)
		sets := viewSets(t, s, nil, alice(s), bob(s))
		requireSet(t, sets[0], freeTier, billing.TrustLevelDefault, 5_000_000)
		requireSet(t, sets[1], "veteran", billing.TrustLevelRule, 50_000_000)
	})

	t.Run("OR-J23-S46/a-chargeback-or-refund-lowers-the-net-amount-paid-and-the-tier-drops", func(t *testing.T) {
		s := tieredSales(t, usdTier("builder", 15_000_000, 0))
		charged := s.buy(t, "drop-alice-1")
		s.buy(t, "drop-alice-2")
		refunded := s.buyAs(t, s.a.bobToken, "drop-bob-1")
		s.buyAs(t, s.a.bobToken, "drop-bob-2")
		requireTrust(t, s, alice(s), "builder", billing.TrustLevelRule, 20_000_000)
		requireTrust(t, s, bob(s), "builder", billing.TrustLevelRule, 20_000_000)
		requireGraduated(t, s, alice(s), "before-chargeback", true)

		s.chargeback(t, charged.ID, "dp_trust_drop")
		paid := requireTrust(t, s, alice(s), freeTier, billing.TrustLevelDefault, 10_000_000)
		require.NotNil(t, paid.FirstPaidAt, "the first payment was made, whatever became of it")
		requireGraduated(t, s, alice(s), "after-chargeback", false)

		_, err := s.a.alpha.RefundPayment(t.Context(), "trust-refund", refunded.ID, billing.RefundPaymentParams{Full: true, RefundReason: billing.RefundRequestedByCustomer})
		require.NoError(t, err)
		requireTrust(t, s, bob(s), freeTier, billing.TrustLevelDefault, 10_000_000)
	})

	t.Run("OR-J23-S47/an-explicit-trust-level-beats-the-rules-and-a-requests-beats-both", func(t *testing.T) {
		s := tieredSales(t, usdTier("builder", 15_000_000, 0))
		s.buy(t, "explicit-1")
		s.buy(t, "explicit-2")
		requireTrust(t, s, alice(s), "builder", billing.TrustLevelRule, 20_000_000)

		_, err := s.a.alpha.UpdateCustomer(t.Context(), alice(s), billing.UpdateCustomerParams{TrustLevels: []billing.TrustLevel{{Currency: "USD", TrustLevel: freeTier}}})
		require.NoError(t, err)
		requireTrust(t, s, alice(s), freeTier, billing.TrustLevelExplicit, 20_000_000)
		requireGraduated(t, s, alice(s), "held-back", false)
		asked := reservation(alice(s), "asked", 20_000_000)
		asked.TrustLevel = "builder"
		d := admitOne(t, s.a.alpha, asked)
		require.True(t, d.Allowed(), "a request's own trust level beats the customer's")
		d.release(t)

		_, err = s.a.alpha.UpdateCustomer(t.Context(), alice(s), billing.UpdateCustomerParams{TrustLevels: []billing.TrustLevel{{Currency: "USD"}}})
		require.NoError(t, err)
		requireTrust(t, s, alice(s), "builder", billing.TrustLevelRule, 20_000_000)
		requireGraduated(t, s, alice(s), "released", true)
	})

	t.Run("OR-J23-S48/a-gates-view-carries-the-computed-tier-and-its-policy", func(t *testing.T) {
		s := tieredSales(t, usdTier("builder", 15_000_000, 0))
		s.buy(t, "view-1")
		s.buy(t, "view-2")
		sets := viewSets(t, s, nil, alice(s), bob(s))
		requireSet(t, sets[0], "builder", billing.TrustLevelRule, 50_000_000)
		requireSet(t, sets[1], freeTier, billing.TrustLevelDefault, 5_000_000)
		requireSet(t, viewSets(t, s, ptr(freeTier), alice(s))[0], freeTier, billing.TrustLevelExplicit, 5_000_000)
		// The gate decides on the set the view carries.
		requireGraduated(t, s, alice(s), "on-the-view", true)
		requireGraduated(t, s, bob(s), "on-the-view", false)
	})

	t.Run("OR-J23-S50/the-credit-line-and-delinquency-grace-follow-the-computed-tiers-policy", func(t *testing.T) {
		// The default tier's policy lends $10 with the merchant's grace (none);
		// a customer who paid $5 is a builder, lent $25 with 30 days' grace.
		a, _, customer := usageBillingWorld(t, true, func(w *world) {
			m := cfgMerchant(w.cfg, "alpha")
			m.BillingPolicies = []billing.BillingPolicy{{Name: "starter", OutstandingCapAmount: 10_000_000}, {Name: "builder", OutstandingCapAmount: 25_000_000, DelinquencyGraceDays: ptr(30)}}
			m.BillingPolicyBindings = []billing.BillingPolicyBinding{{PolicyName: "starter"}, {PolicyName: "builder", Tier: "builder"}}
			m.TrustTiers = []billing.TrustTier{usdTier("builder", 5_000_000, 0)}
			setCfgMerchant(&w.cfg, "alpha", m)
		})
		view := func() *billing.LimitSet {
			t.Helper()
			return syncLease(t, a.alpha, billing.SyncLeasesParams{LeaseID: newLease(), Views: []billing.ViewParams{computeView(customer)}}).LimitSets[0]
		}
		set := view()
		require.Equal(t, freeTier, set.TrustLevel)
		require.EqualValues(t, 10_000_000, set.CreditLine, "the default tier's cap")

		product, err := a.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "starter-pack", DisplayName: "Starter pack"})
		require.NoError(t, err)
		price, err := a.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: "starter-pack", UnitAmount: 5_000_000, Currency: "USD"})
		require.NoError(t, err)
		invoice, err := a.alpha.CreateInvoice(t.Context(), "graduating-sale", billing.CreateInvoiceParams{CustomerID: customer, Lines: []billing.InvoiceLineParams{{PriceID: price.ID}}})
		require.NoError(t, err)
		_, err = a.alpha.RecordExternalPayment(t.Context(), billing.RecordExternalPaymentParams{InvoiceID: invoice.ID, Amount: 5_000_000, TransactionID: "bank-receipt-graduates"})
		require.NoError(t, err)
		set = view()
		require.Equal(t, "builder", set.TrustLevel, "money the merchant recorded counts as paid")
		require.Equal(t, billing.TrustLevelRule, set.TrustLevelSource)
		require.EqualValues(t, 25_000_000, set.CreditLine, "the graduated tier's cap")

		usage := func() billing.Invoice {
			t.Helper()
			rows, err := a.alpha.ListInvoices(t.Context(), billing.InvoiceListParams{CustomerID: customer})
			require.NoError(t, err)
			for _, row := range rows.Items {
				if row.Usage != nil {
					return row
				}
			}
			require.FailNow(t, "no usage invoice")
			return billing.Invoice{}
		}
		oneUsage(t, a.alpha, usageInput(customer, "owed-by-a-builder", 3))
		a.clock.Advance(time.Second)
		fleet := quietFleet(t, a.world)
		runUsageDue(t, fleet)
		due := usage().Usage.DueAt
		advanceTo(a.world, nextHour(due))
		require.False(t, runPass(t, fleet))
		require.False(t, usage().Usage.Delinquent, "the graduated tier's 30 days' grace, not the merchant's none")
		advanceTo(a.world, nextHour(due.Add(30*24*time.Hour)))
		require.False(t, runPass(t, fleet))
		require.True(t, usage().Usage.Delinquent, "delinquent once the tier's grace ends")
	})

	t.Run("OR-J23-S49/trust-tiers-that-cannot-be-met-in-order-are-refused", func(t *testing.T) {
		bound := func(tiers ...billing.TrustTier) billing.MerchantDeclaration {
			m := billing.MerchantDeclaration{MerchantDocument: billing.MerchantDocument{DisplayName: "Tiered", MerchantSettings: billing.MerchantSettings{
				BillingPolicies:       []billing.BillingPolicy{{Name: "capped", SpendWindows: []billing.BudgetWindow{{Key: "month", WindowSeconds: 30 * 24 * 3600, Limit: 50_000_000, Currency: "USD"}}}},
				BillingPolicyBindings: []billing.BillingPolicyBinding{{PolicyName: "capped", Tier: "builder"}, {PolicyName: "capped", Tier: "veteran"}},
				TrustTiers:            tiers,
			}}}
			return m
		}
		refused := map[string]struct {
			m    billing.MerchantDeclaration
			want string
		}{
			"twice":    {bound(usdTier("builder", 5_000_000, 0), usdTier("builder", 15_000_000, 0)), `trust_tiers names "builder" twice`},
			"default":  {bound(usdTier(freeTier, 5_000_000, 0)), `trust_tiers[0].name "free" is the default tier`},
			"unbound":  {bound(usdTier("ghost", 5_000_000, 0)), `trust tier "ghost" has no billing_policy_bindings entry`},
			"lower":    {bound(usdTier("builder", 15_000_000, 0), usdTier("veteran", 5_000_000, 7)), `trust_tiers[1] (veteran) asks no more than builder in USD`},
			"same":     {bound(usdTier("builder", 15_000_000, 7), usdTier("veteran", 15_000_000, 7)), `trust_tiers[1] (veteran) asks no more than builder in USD`},
			"scale":    {bound(usdTier("builder", 50, 0)), `trust_tiers[0].requirements[0].net_paid_amount 50 is not whole minor units of USD`},
			"nothing":  {bound(usdTier("builder", 0, 0)), `trust_tiers[0].requirements[0] asks nothing`},
			"negative": {bound(usdTier("builder", -10_000, 0)), `trust_tiers[0].requirements[0]: net_paid_amount must be >= 0`},
			"twice-in": {bound(billing.TrustTier{Name: "builder", Requirements: []billing.TrustTierRequirement{{Currency: "USD", NetPaidAmount: 10_000}, {Currency: "usd", DaysSinceFirstPayment: 1}}}), `trust_tiers[0] (builder) names USD twice`},
			"empty":    {bound(billing.TrustTier{Name: "builder"}), `trust_tiers[0] (builder) has no requirements`},
		}
		w := newWorld(t, func(w *world) {
			setCfgMerchant(&w.cfg, "alpha", bound(usdTier("builder", 5_000_000, 0), usdTier("veteran", 5_000_000, 7)))
			for slug, c := range refused {
				setCfgMerchant(&w.cfg, slug, c.m)
			}
		})
		state := configurationState(t, w.engine)
		for slug, c := range refused {
			require.ErrorContains(t, state, "merchants."+slug+".merchant: settings: "+c.want, slug)
		}
		require.NotContains(t, state.Error(), "merchants.alpha.", "tiers asking more of days alone serve")
		served, err := w.alpha.GetMerchantConfiguration(t.Context())
		require.NoError(t, err)
		require.Len(t, served.TrustTiers, 2)

		a, _ := newPSPWorld(t)
		valid := bound(usdTier("builder", 5_000_000, 0)).MerchantSettings
		edited, err := a.alphaRemote.UpdateMerchantConfiguration(t.Context(), billing.UpdateMerchantConfigurationParams{MerchantSettings: &valid})
		require.NoError(t, err)
		require.Equal(t, []billing.TrustTier{usdTier("builder", 5_000_000, 0)}, edited.TrustTiers)
		for slug, c := range refused {
			_, err := a.alphaRemote.UpdateMerchantConfiguration(t.Context(), billing.UpdateMerchantConfigurationParams{MerchantSettings: &c.m.MerchantSettings})
			refusal := requireCode(t, err, billing.CodeInvalidParam)
			require.Contains(t, refusal.Message, c.want, slug)
		}
		after, err := a.alphaRemote.GetMerchantConfiguration(t.Context())
		require.NoError(t, err)
		require.Equal(t, edited.Revision, after.Revision, "a refused edit writes nothing")
	})
}
