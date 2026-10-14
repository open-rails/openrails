//go:build e2e && integration

package subscriptions_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/nmimock"
)

// A nil hour count means either an accepted provider period or explicitly
// indefinite access. Provider changes must freeze and retain that choice,
// including the adjustment bill, its future schedule, and a later replay.
func TestProviderTierChangeRetainsAcceptedAccessChoice(t *testing.T) {
	rows := []struct {
		name     string
		from, to catalog.Field[int]
	}{
		{"default to indefinite", catalog.Field[int]{}, catalog.Null[int]()},
		{"indefinite to default", catalog.Null[int](), catalog.Field[int]{}},
		{"default to hours", catalog.Field[int]{}, catalog.Value(48)},
		{"hours to default", catalog.Value(1440), catalog.Field[int]{}},
	}
	for _, rail := range []string{"nmi", "stripe"} {
		for _, action := range []string{"upgrade", "downgrade"} {
			for i, row := range rows {
				t.Run(rail+"/"+action+"/"+row.name, func(t *testing.T) {
					w := newWorld(t)
					w.riverWorkers = 1
					w.armDestructive()
					tp := []topology{embedded, remote}[i%2]
					group := "provider-access-" + uuid.NewString()[:8]
					oldRank, targetRank, oldCents, targetCents := 1, 2, int64(999), int64(1999)
					if action == "downgrade" {
						oldRank, targetRank, oldCents, targetCents = 2, 1, 1999, 999
					}
					old := w.providerAccessTier(rail, group, oldRank, oldCents, row.from)
					target := w.providerAccessTier(rail, group, targetRank, targetCents, row.to)
					var member *legacy
					if rail == "nmi" {
						member = w.legacyOnTier(tp, old, oldCents, monthHours, 10*day)
					} else {
						member = w.stripeOnAccessTier(tp, old, oldCents)
					}
					w.refreshProviders()
					before := w.subscription(tp, member.sub)
					boundary, changedAt := *before.CurrentPeriodEndsAt, w.clock.Now()
					params := billing.ChangeSubscriptionParams{PriceID: priceRef(target.ID), IdempotencyKey: "access-" + uuid.NewString()}
					done, err := member.c.change(member.sub, params)
					require.NoError(t, err)
					require.Equal(t, "succeeded", done.Status)
					w.settle()
					var accepted string
					require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT payload::text FROM billing.provider_intents WHERE subscription_id=$1 AND intent_type IN ('nmi_upgrade','stripe_tier_change') ORDER BY created_at DESC LIMIT 1`), member.sub.UUID()).Scan(&accepted))
					var frozen map[string]json.RawMessage
					require.NoError(t, json.Unmarshal([]byte(accepted), &frozen))
					if row.to.Set {
						require.NotContains(t, frozen, "provider_access_default", "false stays absent in historical accepted JSON")
					} else {
						require.JSONEq(t, `true`, string(frozen["provider_access_default"]))
					}
					if action == "downgrade" {
						require.Equal(t, old.ID, w.subscription(tp, member.sub).PriceID)
						w.advanceTo(boundary.Add(time.Hour))
						require.Equal(t, http.StatusOK, w.deliver(rail, member.providerRenewal(true)))
						w.settle()
						changedAt = boundary
					}
					current := w.subscription(other(tp), member.sub)
					require.Equal(t, target.ID, current.PriceID)
					var hours *int
					var defaultAccess bool
					require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT access_duration_hours_snapshot,provider_access_default_snapshot FROM billing.subscriptions WHERE id=$1`), member.sub.UUID()).Scan(&hours, &defaultAccess))
					require.Equal(t, !row.to.Set, defaultAccess)
					var wantEnd *time.Time
					if defaultAccess {
						require.Nil(t, hours)
						wantEnd = current.CurrentPeriodEndsAt
					} else if !row.to.Null {
						require.Equal(t, &row.to.Value, hours)
						wantEnd = new(changedAt.Add(time.Duration(row.to.Value) * time.Hour))
					} else {
						require.Nil(t, hours)
					}
					var gotEnd *time.Time
					require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT ends_at FROM billing.grants WHERE source_type='subscription' AND source_id=$1 AND product_id=$2 AND event='grant' AND supersedes_id IS NULL ORDER BY starts_at DESC,created_at DESC LIMIT 1`), member.sub.String(), target.ProductID.UUID()).Scan(&gotEnd))
					if wantEnd == nil {
						require.Nil(t, gotEnd, "explicit null remains indefinite")
					} else {
						require.NotNil(t, gotEnd)
						require.True(t, wantEnd.Equal(*gotEnd), "accepted access end: want %s, got %s", wantEnd, gotEnd)
					}
					if rail == "nmi" {
						var billDefault bool
						require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT provider_access_default FROM billing.order_lines WHERE subscription_id=$1 AND price_id=$2 ORDER BY created_at LIMIT 1`), member.sub.UUID(), target.ID.UUID()).Scan(&billDefault))
						require.Equal(t, defaultAccess, billDefault, "accepted adjustment/schedule retains its access choice")
					}
					grants := w.countRows(`SELECT count(*) FROM billing.grants WHERE source_id=$1`, member.sub.String())
					paid := len(completed(w.payments(tp, member.c.id)))
					again, err := member.c.change(member.sub, params)
					require.NoError(t, err)
					require.Equal(t, "succeeded", again.Status)
					w.settle()
					require.Equal(t, grants, w.countRows(`SELECT count(*) FROM billing.grants WHERE source_id=$1`, member.sub.String()), "replay adds no access grant")
					require.Len(t, completed(w.payments(tp, member.c.id)), paid, "replay adds no payment")
					var replayed string
					require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT payload::text FROM billing.provider_intents WHERE subscription_id=$1 AND intent_type IN ('nmi_upgrade','stripe_tier_change') ORDER BY created_at DESC LIMIT 1`), member.sub.UUID()).Scan(&replayed))
					require.Equal(t, accepted, replayed, "completion/replay preserves accepted JSON")
				})
			}
		}
	}
}

func (w *world) providerAccessTier(rail, group string, rank int, cents int64, access catalog.Field[int]) tier {
	w.t.Helper()
	key := "access-tier-" + uuid.NewString()[:8]
	product, err := w.client[embedded].CreateProduct(w.t.Context(), billing.CreateProductParams{Key: key, DisplayName: key, TierGroup: &group, TierRank: rank, Entitlements: []string{key}})
	require.NoError(w.t, err)
	params := billing.CreateProviderPriceParams{ProductID: product.ID, Key: "monthly", PSP: rail, AccessDurationHours: access}
	out := tier{ent: key, plan: "access-plan-" + uuid.NewString()[:8]}
	if rail == "nmi" {
		w.nmi.AddPlan(nmimock.Plan{ID: out.plan, Name: key, Amount: decimalCents(cents), Days: 30})
		params.PlanID = out.plan
	} else {
		out.plan = "price_legacy_access_" + uuid.NewString()[:8]
		w.stripe.SetLegacyPrice(out.plan, cents)
		params.PriceID = out.plan
	}
	out.Price, err = w.client[embedded].CreateProviderPrice(w.t.Context(), params)
	require.NoError(w.t, err)
	return out
}

func (w *world) stripeOnAccessTier(tp topology, price tier, cents int64) *legacy {
	w.t.Helper()
	member := &legacy{w: w, rail: "stripe", tp: tp, c: w.newCustomer(), price: price.Price, ent: price.ent, railCust: "cus_access_" + uuid.NewString()[:8]}
	start := w.clock.Now().Add(-10 * day)
	end := start.Add(monthHours * time.Hour)
	method := "pm_access_" + uuid.NewString()[:8]
	member.railSub = w.stripe.AddSubscription(member.railCust, method, price.plan, cents, start, end)
	book := billing.DeclaredBilling{AsOf: w.clock.Now(), DefaultPSP: billing.PSPRef{Key: "stripe"}, Customers: []billing.DeclaredCustomer{{Customer: member.c.cid()}},
		PaymentMethods: []billing.DeclaredPaymentMethod{{Customer: member.c.cid(), Rail: "stripe", RailCustomerRef: member.railCust, RailMethodRef: method, Card: declaredCard(visa)}},
		Subscriptions:  []billing.DeclaredSubscription{{SourceID: member.railSub, Customer: member.c.cid(), Price: price.ID, Rail: "stripe", RailSubscriptionID: member.railSub, StartedAt: start, PaidThrough: &end, PaymentMethod: &billing.PaymentMethodRef{Rail: "stripe", RailCustomerRef: member.railCust, RailMethodRef: method}}},
		Transactions:   []billing.DeclaredTransaction{{RailSubscriptionID: member.railSub, TransactionID: w.stripe.LatestCharge(member.railSub), Success: true, Amount: cents * 10_000, Currency: "USD", OccurredAt: start}}}
	attachDeclaredStripeInvoice(w.t, w, &book)
	result, err := w.client[tp].ImportBilling(w.t.Context(), book)
	require.NoError(w.t, err)
	require.Len(w.t, result.Imported, 1)
	w.settle()
	list, err := w.client[tp].ListSubscriptions(w.t.Context(), billing.SubscriptionListParams{CustomerID: member.c.cid()})
	require.NoError(w.t, err)
	require.Len(w.t, list.Items, 1)
	member.sub = list.Items[0].ID
	return member
}
