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
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/reconcile/converge"
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
		// A second paid agreement at the same boundary: the invoice's sale, twice.
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

		// The invoice's sale alone renews the first period.
		_, err = a.pool.Exec(t.Context(), "DELETE FROM "+intents+" WHERE merchant_id = $1 AND idempotency_key = 'critical:second-agreement'", a.alpha.MerchantID().UUID())
		require.NoError(t, err)
		// The next pass takes it again; the period still starts at the boundary.
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

		// Seats added mid-period change the invoice's agreement, not replace it.
		w, provider := newPaymentWorld(t, func(w *world) { w.clock.Advance(-30 * 24 * time.Hour) })
		team, err := w.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "team", DisplayName: "Team", Entitlements: []string{"team:member"}})
		require.NoError(t, err)
		seat, err := w.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: team.ID, Key: "team-day", UnitAmount: 10_000_000, Currency: "USD", BillingIntervalHours: ptr(24), AccessDurationHours: ptr(24), Quantity: &catalog.Quantity{Min: 1, Max: 5}})
		require.NoError(t, err)
		invoice := billing.CreateMyInvoiceParams{Lines: []billing.InvoiceLineParams{{PriceID: seat.ID, Quantity: ptr(1)}}, ExpectedTotal: ptr(int64(10_000_000)), Reusable: ptr(true),
			Payment: &billing.InvoicePaymentParams{Token: provider.core.NewPaymentMethod(stripemock.Card{Brand: "visa", Last4: "4242"})}}
		status, raw = w.request(t, http.MethodPost, "/v1/me/invoices", w.aliceToken, invoice, map[string]string{"Idempotency-Key": "team-purchase"})
		require.Equal(t, http.StatusCreated, status, string(raw))
		var bought billing.Invoice
		require.NoError(t, json.Unmarshal(raw, &bought))
		require.Equal(t, billing.InvoiceComplete, bought.Status, string(raw))
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

func TestSubscriptionGrantRevocations(t *testing.T) {
	for _, tc := range []struct {
		name    string
		upgrade bool
	}{
		{"OR-J18-S08/upgrade-leaves-ended-paid-periods-unrevoked", true},
		{"OR-J17-S16/admin-revoke-leaves-ended-paid-periods-unrevoked", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _, sub := purchasedMembership(t)
			fleet := startFleet(t, a.world)
			for range 2 {
				sub = renewedAt(t, a, fleet, sub.ID, *sub.CurrentPeriodEndsAt, *sub.CurrentPeriodEndsAt)
			}
			require.NoError(t, fleet.Stop(t.Context()))
			if tc.upgrade {
				a.clock.Advance(12 * time.Hour)
				price := upgradePrice(t, a, sub)
				status, raw := a.request(t, http.MethodPost, "/v1/me/subscriptions/"+sub.ID.String()+"/change", a.aliceToken, map[string]any{"price_id": price.ID}, map[string]string{"Idempotency-Key": "past-period-upgrade"})
				require.Equal(t, http.StatusOK, status, string(raw))
				var changed billing.SubscriptionChange
				require.NoError(t, json.Unmarshal(raw, &changed))
				require.Equal(t, "succeeded", changed.Status, changed.Message)
			} else {
				// The last ended period ends exactly at the revoke instant.
				canceled, err := a.alpha.CancelSubscription(t.Context(), sub.ID, billing.CancelSubscriptionParams{Reason: "End membership access immediately", RevokeAccess: true})
				require.NoError(t, err)
				require.Equal(t, billing.SubscriptionCanceled, canceled.Status)
			}
			database, err := producerDB(a.pool, a.schema)
			require.NoError(t, err)
			ctx := merchant.WithID(t.Context(), a.alpha.MerchantID())
			q := database.Gen(ctx)
			unretracted, err := q.ListUnretractedTerminations(ctx, gen.ListUnretractedTerminationsParams{MerchantID: a.alpha.MerchantID().UUID(), RowLimit: 100})
			require.NoError(t, err)
			require.Empty(t, unretracted, "ended paid windows need no retraction")
			grants, err := q.ListGrantsByCustomer(ctx, gen.ListGrantsByCustomerParams{MerchantID: a.alpha.MerchantID().UUID(), CustomerID: sub.CustomerID.UUID()})
			require.NoError(t, err)
			ended, revoked := 0, 0
			for _, g := range grants {
				if g.SourceType != "subscription" || g.SourceID == nil || *g.SourceID != sub.ID.UUID().String() {
					continue
				}
				require.NotNil(t, g.EndsAt)
				terminated, err := q.IsGrantTerminated(ctx, gen.IsGrantTerminatedParams{MerchantID: a.alpha.MerchantID().UUID(), GrantID: g.ID})
				require.NoError(t, err)
				window, err := q.GetProductAccessByGrant(ctx, gen.GetProductAccessByGrantParams{MerchantID: a.alpha.MerchantID().UUID(), GrantID: g.ID})
				require.NoError(t, err)
				if !g.EndsAt.After(a.clock.Now()) {
					ended++
					require.False(t, terminated)
					require.Nil(t, window.RevokedAt)
				} else {
					revoked++
					require.True(t, terminated)
					require.NotNil(t, window.RevokedAt)
					require.True(t, a.clock.Now().Equal(*window.RevokedAt))
				}
			}
			require.Equal(t, 2, ended)
			require.Equal(t, 1, revoked)
			access, err := a.alpha.CheckEntitlements(t.Context(), billing.CheckEntitlementsParams{CustomerID: sub.CustomerID, Entitlements: []string{"membership:paid"}})
			require.NoError(t, err)
			require.False(t, access.Entitlements["membership:paid"])
			_, err = converge.NewConvergeEngine(database, a.clock).Converge(ctx, converge.Scope{Merchant: a.alpha.MerchantID()})
			require.NoError(t, err)
			findings, err := a.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: "derive.grant_effect.excess"})
			require.NoError(t, err)
			require.Empty(t, findings.Items)
		})
	}
}
