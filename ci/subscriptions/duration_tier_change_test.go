//go:build e2e && integration

package subscriptions_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
)

func TestDurationDowngradePreservesLongerPaidBenefit(t *testing.T) {
	for _, tp := range []topology{embedded, remote} {
		t.Run(string(tp), func(t *testing.T) {
			w := newWorld(t)
			w.armDestructive()
			group, plan := "duration-"+uuid.NewString(), "duration_plan_"+uuid.NewString()[:8]
			product, err := w.client[tp].CreateProduct(t.Context(), billing.CreateProductParams{
				Key: "long-access", DisplayName: "Long paid access", TierGroup: &group, TierRank: 2,
				EntitlementsSpec: map[string]*int{"content:long-paid": nil},
			})
			require.NoError(t, err)
			price, err := w.client[tp].CreatePrice(t.Context(), billing.CreatePriceParams{
				ProductID: product.ID, Key: "monthly", UnitAmount: 19_990_000, Currency: "USD",
				BillingIntervalHours: new(720), AccessDurationHours: new(1440),
				PSPLinks: map[string]map[string]string{"nmi": {"plan_id": plan}},
			})
			require.NoError(t, err)
			old := tier{Price: price, ent: "content:long-paid", plan: plan}
			lower := w.tierPrice(group, 1, 499, monthHours, true)
			member := w.legacyOnTier(tp, old, 1999, monthHours, 10*day)
			boundary := member.periodEnd()
			done, err := w.client[tp].ChangeTier(t.Context(), member.sub, billing.ChangeTierParams{PriceID: lower.ID, IdempotencyKey: uuid.NewString()})
			require.NoError(t, err)
			w.settle()
			require.Equal(t, "succeeded", done.Status)
			w.advanceTo(boundary.Add(time.Hour))
			require.Equal(t, http.StatusOK, w.deliver("nmi", member.providerRenewal(true)))
			require.Equal(t, lower.ID, w.subscription(tp, member.sub).PriceID)
			require.True(t, member.c.entitled(lower.ent))
			require.True(t, member.c.entitled(old.ent), "downgrading cannot revoke the longer access already purchased")
			w.converge()
			require.True(t, member.c.entitled(old.ent), "reconciliation preserves the original paid window")
			w.advanceTo(boundary.Add(721 * time.Hour))
			w.converge()
			require.False(t, member.c.entitled(old.ent), "the removed benefit ends at its original access boundary")
		})
	}
}
