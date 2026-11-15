//go:build integration

package critical_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/stripemock"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"
)

// renewedAt runs one due pass at at and waits for sub's period to start at due.
func renewedAt(t *testing.T, a *authorityWorld, fleet *river.Client[pgx.Tx], sub billing.SubscriptionID, at, due time.Time) *billing.Subscription {
	t.Helper()
	a.clock.Advance(at.Sub(a.clock.Now()))
	runRegistered(t, fleet, "openrails.billing_pass", 1)
	require.Eventually(t, func() bool {
		current, err := a.alpha.GetSubscription(t.Context(), sub)
		return err == nil && current.CurrentPeriodStartsAt != nil && current.CurrentPeriodStartsAt.Equal(due)
	}, 20*time.Second, 20*time.Millisecond, "the renewal at %s settles", due)
	current, err := a.alpha.GetSubscription(t.Context(), sub)
	require.NoError(t, err)
	return current
}

func lastCharge(t *testing.T, p *providerLedger, count int) int64 {
	t.Helper()
	effects := p.effects(t)
	require.Len(t, effects, count)
	require.Equal(t, "charge", effects[count-1].Kind)
	return effects[count-1].Amount
}

func TestRenewalAgreements(t *testing.T) {
	t.Run("OR-J14-S06/each-paid-agreement-renews-once-and-two-refuse", func(t *testing.T) {
		a, p, sub := purchasedMembership(t)
		due := *sub.CurrentPeriodEndsAt
		intents := pgx.Identifier{a.schema, "provider_intents"}.Sanitize()
		// A second paid agreement at the same boundary: the order's sale, twice.
		tag, err := a.pool.Exec(t.Context(), "INSERT INTO "+intents+` (merchant_id, rail, intent_type, subscription_id, payment_id, price_id, payload, idempotency_key, status,
			attempts, next_attempt_at, origin, origin_reason, actor, result_evidence, executed_at, psp_id, custodian_id, subject, credential)
			SELECT merchant_id, rail, intent_type, subscription_id, payment_id, price_id, payload, 'critical:second-agreement', status,
			attempts, next_attempt_at, origin, origin_reason, actor, result_evidence, executed_at, psp_id, custodian_id, subject, credential
			FROM `+intents+" WHERE merchant_id = $1 AND intent_type = 'nmi_sale' AND status = 'succeeded'", a.alpha.MerchantID().UUID())
		require.NoError(t, err)
		require.EqualValues(t, 1, tag.RowsAffected())
		a.clock.Advance(due.Sub(a.clock.Now()))
		fleet := startFleet(t, a.world)
		runRegistered(t, fleet, "openrails.billing_pass", 1)
		refused := func() []billing.Finding {
			findings, err := a.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: "life.due_pass.refused"})
			require.NoError(t, err)
			return findings.Items
		}
		require.Len(t, refused(), 1, "an ambiguous agreement authorizes no charge")
		require.Equal(t, sub.ID.UUID().String(), refused()[0].SubjectKey)
		require.Contains(t, refused()[0].Evidence["error"], "engine paid agreement is missing or ambiguous")
		current, err := a.alpha.GetSubscription(t.Context(), sub.ID)
		require.NoError(t, err)
		require.Equal(t, due, *current.CurrentPeriodEndsAt)
		require.Len(t, p.effects(t), 1)

		// The order's sale alone renews the first period.
		_, err = a.pool.Exec(t.Context(), "DELETE FROM "+intents+" WHERE merchant_id = $1 AND idempotency_key = 'critical:second-agreement'", a.alpha.MerchantID().UUID())
		require.NoError(t, err)
		// Its recheck is a minute later; the period still starts at the boundary.
		current = renewedAt(t, a, fleet, sub.ID, due.Add(2*time.Minute), due)
		require.EqualValues(t, 1000, lastCharge(t, p, 2))
		require.Empty(t, refused(), "the refusal closes once the renewal is admitted")
		// That renewal's collection is the next period's agreement.
		renewedAt(t, a, fleet, sub.ID, *current.CurrentPeriodEndsAt, *current.CurrentPeriodEndsAt)
		require.EqualValues(t, 1000, lastCharge(t, p, 3))

		// An upgrade's initial membership is its subscription's agreement.
		price := upgradePrice(t, a, sub)
		a.clock.Advance(12 * time.Hour)
		status, raw := a.request(t, http.MethodPost, "/v1/me/subscriptions/"+sub.ID.String()+"/change", a.aliceToken, map[string]any{"price_id": price.ID}, map[string]string{"Idempotency-Key": "agreement-upgrade"})
		require.Equal(t, http.StatusOK, status, string(raw))
		var changed billing.SubscriptionChange
		require.NoError(t, json.Unmarshal(raw, &changed))
		require.Equal(t, "succeeded", changed.Status, string(raw))
		require.NotNil(t, changed.SubscriptionID)
		require.NotEqual(t, sub.ID, *changed.SubscriptionID)
		lastCharge(t, p, 4)
		upgraded, err := a.alpha.GetSubscription(t.Context(), *changed.SubscriptionID)
		require.NoError(t, err)
		upgraded = renewedAt(t, a, fleet, upgraded.ID, *upgraded.CurrentPeriodEndsAt, *upgraded.CurrentPeriodEndsAt)
		require.Equal(t, price.ID, upgraded.PriceID)
		require.EqualValues(t, 2000, lastCharge(t, p, 5))
		cycles, err := a.alpha.ListRenewals(t.Context(), billing.RenewalListParams{SubscriptionID: upgraded.ID})
		require.NoError(t, err)
		require.Len(t, cycles.Items, 1)
		require.Equal(t, "collected", cycles.Items[0].Outcome)
		require.EqualValues(t, 20_000_000, cycles.Items[0].Amount)

		// Seats added mid-period change the order's agreement, not replace it.
		w, provider := newPaymentWorld(t, func(w *world) { w.clock.Advance(-30 * 24 * time.Hour) })
		team, err := w.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "team", DisplayName: "Team", Entitlements: []string{"team:member"}})
		require.NoError(t, err)
		seat, err := w.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: team.ID, Key: "team-day", UnitAmount: 10_000_000, Currency: "USD", BillingIntervalHours: ptr(24), AccessDurationHours: ptr(24), Quantity: &catalog.Quantity{Min: 1, Max: 5}})
		require.NoError(t, err)
		order := billing.CreateMyOrderParams{Lines: []billing.OrderLineParams{{PriceID: seat.ID, Quantity: ptr(1)}}, ExpectedTotal: ptr(int64(10_000_000)), Reusable: ptr(true),
			Payment: &billing.OrderPaymentParams{Token: provider.core.NewPaymentMethod(stripemock.Card{Brand: "visa", Last4: "4242"})}}
		status, raw = w.request(t, http.MethodPost, "/v1/me/orders", w.aliceToken, order, map[string]string{"Idempotency-Key": "team-purchase"})
		require.Equal(t, http.StatusCreated, status, string(raw))
		var bought billing.Order
		require.NoError(t, json.Unmarshal(raw, &bought))
		require.Equal(t, billing.OrderComplete, bought.Status, string(raw))
		member, err := w.alpha.GetSubscription(t.Context(), *bought.Lines[0].SubscriptionID)
		require.NoError(t, err)
		w.clock.Advance(12 * time.Hour)
		status, raw = w.request(t, http.MethodPost, "/v1/me/subscriptions/"+member.ID.String()+"/change", w.aliceToken, map[string]any{"quantity": 2}, map[string]string{"Idempotency-Key": "more-seats"})
		require.Equal(t, http.StatusOK, status, string(raw))
		var added billing.SubscriptionChange
		require.NoError(t, json.Unmarshal(raw, &added))
		require.Equal(t, "succeeded", added.Status, string(raw))
		lastCharge(t, provider, 2)
		w.clock.Advance(member.CurrentPeriodEndsAt.Sub(w.clock.Now()))
		seats := startFleet(t, w.world)
		renewed := renewedAt(t, w, seats, member.ID, *member.CurrentPeriodEndsAt, *member.CurrentPeriodEndsAt)
		require.Equal(t, 2, *renewed.Quantity)
		require.EqualValues(t, 2000, lastCharge(t, provider, 3))
	})
}
