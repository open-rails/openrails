//go:build integration

package critical_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/nmimock"
	"github.com/open-rails/openrails/internal/stripemock"
	"github.com/stretchr/testify/require"
)

func TestEnrollmentContracts(t *testing.T) {
	t.Run("OR-J13-S02/concurrent-signup-key-creates-one-membership-and-charge", func(t *testing.T) {
		a, p := newPaymentWorld(t, func(w *world) { w.clock.Advance(-30 * 24 * time.Hour) })
		product, err := a.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "concurrent-member", DisplayName: "Concurrent member", Entitlements: []string{"concurrent:member"}})
		require.NoError(t, err)
		price, err := a.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, UnitAmount: 10_000_000, Currency: "USD", BillingIntervalHours: ptr(24), AccessDurationHours: ptr(24)})
		require.NoError(t, err)
		token := p.core.NewPaymentMethod(stripemock.Card{Brand: "visa", Last4: "4242"})
		input := billing.CreateMyOrderParams{Lines: []billing.OrderLineParams{{PriceID: price.ID}}, ExpectedTotal: ptr(int64(10_000_000)), Reusable: ptr(true), Payment: &billing.OrderPaymentParams{Token: token}}
		type response struct {
			status int
			raw    []byte
			err    error
		}
		begin := make(chan struct{})
		answers := make(chan response, 2)
		for range 2 {
			go func() {
				<-begin
				status, raw, err := a.do(t.Context(), http.MethodPost, "/v1/me/orders", a.aliceToken, input, map[string]string{"Idempotency-Key": "one-membership"})
				answers <- response{status, raw, err}
			}()
		}
		close(begin)
		ids := []billing.OrderID{}
		for range 2 {
			r := <-answers
			require.NoError(t, r.err)
			if r.status == http.StatusConflict {
				require.Contains(t, string(r.raw), "idempotency_key_in_use")
				continue
			}
			require.Equal(t, http.StatusCreated, r.status, string(r.raw))
			var order billing.Order
			require.NoError(t, json.Unmarshal(r.raw, &order))
			require.Equal(t, billing.OrderComplete, order.Status)
			ids = append(ids, order.ID)
		}
		require.NotEmpty(t, ids)
		status, raw := a.request(t, http.MethodPost, "/v1/me/orders", a.aliceToken, input, map[string]string{"Idempotency-Key": "one-membership"})
		require.Equal(t, http.StatusCreated, status, string(raw))
		var replay billing.Order
		require.NoError(t, json.Unmarshal(raw, &replay))
		for _, id := range ids {
			require.Equal(t, id, replay.ID)
		}
		require.Equal(t, billing.OrderComplete, replay.Status)
		subscriptions, err := a.alpha.ListSubscriptions(t.Context(), billing.SubscriptionListParams{CustomerID: billing.CustomerID(uuid.MustParse(a.alice.ID))})
		require.NoError(t, err)
		require.Len(t, subscriptions.Items, 1)
		require.Equal(t, billing.SubscriptionActive, subscriptions.Items[0].Status)
		require.Len(t, p.effects(t), 1)
		require.EqualValues(t, 1000, p.effects(t)[0].Amount)
		events, err := a.alpha.ListEvents(t.Context(), billing.EventQuery{Types: []string{"payment.settled", "order.completed"}, Limit: 100})
		require.NoError(t, err)
		require.Len(t, events.Events, 2)
	})
	t.Run("OR-J13-S03/imported-paid-introduction-retains-first-and-recurring-terms", func(t *testing.T) {
		for _, introDays := range []int{2, 14} {
			t.Run(fmt.Sprintf("intro-%d-days-to-seven", introDays), func(t *testing.T) {
				a, p := newNMIWorld(t)
				p.core.AddPlan(nmimock.Plan{ID: "intro-weekly", Name: "Weekly after introduction", Amount: "10.00", Days: max(7, introDays)})
				product, err := a.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "imported-intro", DisplayName: "Imported introduction", Entitlements: []string{"intro:member"}})
				require.NoError(t, err)
				price, err := a.alpha.CreateProviderPrice(t.Context(), billing.CreateProviderPriceParams{ProductID: product.ID, Key: "intro-weekly", PSP: "nmi", PlanID: "intro-weekly", Currency: "USD"})
				require.NoError(t, err)
				// A legacy introduction may be longer than the accepted recurring
				// interval. Catalog metadata is no authority to rewrite that agreement.
				p.core.AddPlan(nmimock.Plan{ID: "intro-weekly", Name: "Weekly after introduction", Amount: "10.00", Days: 7})
				start, due := a.clock.Now(), a.clock.Now().Add(time.Duration(introDays)*24*time.Hour)
				vaultID := p.core.AddVault(nmimock.Card{Brand: "visa", Last4: "4242"})
				vault := p.core.Vault(vaultID)
				schedule := p.core.AddSchedule(nmimock.Schedule{Vault: vaultID, Plan: "intro-weekly", Amount: "2.00", Days: 7, Created: start, NextBilling: due, Order: "legacy-paid-introduction"})
				initial := p.core.AddScheduleSale(schedule, start)
				p.core.EditSchedule(schedule, func(s *nmimock.Schedule) { s.Amount = "10.00" })
				require.NoError(t, p.persist(t.Context()))
				customer := billing.CustomerID(uuid.MustParse(a.alice.ID))
				book := billing.DeclaredBilling{AsOf: start, DefaultPSP: billing.PSPRef{Key: "nmi"}, Customers: []billing.DeclaredCustomer{{Customer: customer}}, PaymentMethods: []billing.DeclaredPaymentMethod{{Customer: customer, Rail: "nmi", RailCustomerRef: vaultID, RailMethodRef: vault.BillingID, RecurringTransactionID: initial.TransactionID, CreatedAt: start}}, Subscriptions: []billing.DeclaredSubscription{{SourceID: "intro-membership", Customer: customer, Price: price.ID, Rail: "nmi", RailSubscriptionID: schedule, StartedAt: start, PaidThrough: &due, PaymentMethod: &billing.PaymentMethodRef{Rail: "nmi", RailCustomerRef: vaultID, RailMethodRef: vault.BillingID}, AcceptedSchedule: &billing.DeclaredProviderSchedule{SourceReference: schedule, Amount: 10_000_000, Currency: "USD", IntervalHours: 168, FirstDueAt: due}}}, Transactions: []billing.DeclaredTransaction{{SourceID: "intro-receipt", RailSubscriptionID: schedule, TransactionID: initial.TransactionID, Type: "sale", Success: true, Amount: 2_000_000, Currency: "USD", OccurredAt: start, NextBillingAt: &due}}}
				result, err := a.alpha.ImportBilling(t.Context(), book)
				require.NoError(t, err)
				require.Empty(t, result.Blocked, "%+v", result.Reasons)
				subs, err := a.alpha.ListSubscriptions(t.Context(), billing.SubscriptionListParams{CustomerID: customer})
				require.NoError(t, err)
				require.Len(t, subs.Items, 1)
				sub, err := a.alpha.GetSubscription(t.Context(), subs.Items[0].ID)
				require.NoError(t, err)
				require.Equal(t, start, *sub.CurrentPeriodStartsAt)
				require.Equal(t, due, *sub.CurrentPeriodEndsAt)
				payments, err := a.alpha.ListPayments(t.Context(), billing.PaymentListParams{CustomerID: customer})
				require.NoError(t, err)
				require.Len(t, payments.Items, 1)
				require.EqualValues(t, 2_000_000, payments.Items[0].Amount)
				p.money(t, 1, 200)
				p.core.AddScheduleSale(schedule, due)
				p.core.EditSchedule(schedule, func(s *nmimock.Schedule) {
					s.NextBilling = due.Add(7 * 24 * time.Hour)
					if introDays > 7 {
						// The provider has deferred a later debit. That future
						// roster date is not payment for a second interval.
						s.NextBilling = due.Add(14 * 24 * time.Hour)
					}
				})
				require.NoError(t, p.persist(t.Context()))
				a.clock.Advance(due.Sub(a.clock.Now()) + 25*time.Hour)
				fleet := startFleet(t, a.world)
				runRegistered(t, fleet, "openrails.rebill_watch", 2)
				observed, err := a.alpha.GetSubscription(t.Context(), sub.ID)
				require.NoError(t, err)
				require.Equal(t, due, *observed.CurrentPeriodStartsAt)
				t.Logf("intro %d days: settled public period %s through %s; accepted recurring end %s", introDays, observed.CurrentPeriodStartsAt.Format(time.RFC3339), observed.CurrentPeriodEndsAt.Format(time.RFC3339), due.Add(7*24*time.Hour).Format(time.RFC3339))
				history, err := a.alpha.ListRenewals(t.Context(), billing.RenewalListParams{SubscriptionID: sub.ID})
				require.NoError(t, err)
				require.Len(t, history.Items, 1)
				require.EqualValues(t, 10_000_000, history.Items[0].Amount)
				require.Equal(t, "collected", history.Items[0].Outcome)
				require.Eventually(t, func() bool {
					read, err := a.alpha.GetSubscription(t.Context(), sub.ID)
					return err == nil && read.CurrentPeriodEndsAt.Equal(due.Add(7*24*time.Hour))
				}, 20*time.Second, 20*time.Millisecond)
				payments, err = a.alpha.ListPayments(t.Context(), billing.PaymentListParams{CustomerID: customer})
				require.NoError(t, err)
				require.Len(t, payments.Items, 2)
				require.ElementsMatch(t, []int64{2_000_000, 10_000_000}, []int64{payments.Items[0].Amount, payments.Items[1].Amount})
				p.money(t, 2, 1200)
			})
		}
	})
	t.Run("OR-J13-S04/trial-price-is-explicitly-unavailable-to-new-checkout", func(t *testing.T) {
		a, p := newPaymentWorld(t)
		product, err := a.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "retained-trial", DisplayName: "Retained trial"})
		require.NoError(t, err)
		for _, amount := range []int64{0, 2_000_000} {
			price, err := a.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: "trial", UnitAmount: 10_000_000, Currency: "USD", BillingIntervalHours: ptr(168), TrialUnitAmount: &amount, TrialDurationHours: ptr(48)})
			require.NoError(t, err)
			token := p.core.NewPaymentMethod(stripemock.Card{Brand: "visa", Last4: "4242"})
			input := billing.CreateMyOrderParams{Lines: []billing.OrderLineParams{{PriceID: price.ID}}, ExpectedTotal: &amount, Reusable: ptr(true), Payment: &billing.OrderPaymentParams{Token: token}}
			status, raw := a.request(t, http.MethodPost, "/v1/me/orders", a.aliceToken, input, map[string]string{"Idempotency-Key": "unsupported-trial-" + price.ID.String()})
			require.Equal(t, http.StatusUnprocessableEntity, status, string(raw))
			require.Contains(t, string(raw), "order_line_unavailable")
			require.Empty(t, p.effects(t))
		}
		customer := billing.CustomerID(uuid.MustParse(a.alice.ID))
		subs, err := a.alpha.ListSubscriptions(t.Context(), billing.SubscriptionListParams{CustomerID: customer})
		require.NoError(t, err)
		require.Empty(t, subs.Items)
		facts, err := a.alpha.ListEvents(t.Context(), billing.EventQuery{Types: []string{"payment.settled", "order.completed"}, Limit: 100})
		require.NoError(t, err)
		require.Empty(t, facts.Events)
	})
}
