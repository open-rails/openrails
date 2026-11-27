//go:build integration

package critical_test

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit/authtest"
	"net/http"
	"net/textproto"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/nmimock"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"
)

func TestFindingAlerts(t *testing.T) {
	t.Run("OR-J06-S66/a-refund-the-ledger-cannot-book-alerts-the-operator", func(t *testing.T) {
		mail := &alertMailbox{}
		a := ccbillWebhookWorld(t, func(w *world) {
			alertEmail(w)
			w.deps.Email = mail
		})
		price := ccbillMemberPrice(t, a.alpha)
		now := a.clock.Now().UTC().Truncate(time.Second)
		const ref, sale = "556677889900114455", "0812233445566779911"
		importLegacyCCBill(t, a.alpha, now)(legacyCCBillMember(customerOf(a.alice), price, ref, sale, now.Add(5*24*time.Hour)))
		fleet := hostFleet(t, a.world)
		fleet.PeriodicJobs().Clear()
		require.NoError(t, a.engine.Start(t.Context(), openrails.WithRiverClient(fleet)))
		require.NoError(t, fleet.Start(t.Context()))

		// CCBill refunds more than the sale: nothing is booked, a person repairs it.
		status, body := ccbillNotice(t, a, "Refund", map[string]any{"subscriptionId": ref, "transactionId": sale,
			"amount": "15.00", "currencyCode": "840", "reason": "customer request", "timestamp": now.Add(-time.Hour).Format(time.DateTime)})
		require.Equal(t, http.StatusOK, status, body)
		unbooked, err := a.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: "consistency.ledger.unbooked"})
		require.NoError(t, err)
		require.Len(t, unbooked.Items, 1)
		require.Eventually(t, func() bool { return mail.count("consistency.ledger.unbooked") == 1 }, 20*time.Second, 20*time.Millisecond,
			"the critical finding alerts its operator")
		alertsSettled(t, fleet)
		require.Equal(t, 1, mail.count("consistency.ledger.unbooked"), "once")
	})

	t.Run("OR-J06-S71/distinct-repairs-of-one-transaction-alert-separately", func(t *testing.T) {
		mail := &alertMailbox{}
		a := ccbillWebhookWorld(t, func(w *world) {
			alertEmail(w)
			w.deps.Email = mail
		})
		price := ccbillMemberPrice(t, a.alpha)
		now := a.clock.Now().UTC().Truncate(time.Second)
		const ref, sale = "556677889900114466", "0812233445566779922"
		importLegacyCCBill(t, a.alpha, now)(legacyCCBillMember(customerOf(a.alice), price, ref, sale, now.Add(5*24*time.Hour)))
		fleet := hostFleet(t, a.world)
		fleet.PeriodicJobs().Clear()
		require.NoError(t, a.engine.Start(t.Context(), openrails.WithRiverClient(fleet)))
		require.NoError(t, fleet.Start(t.Context()))

		// Two refunds of the sale with no time to tell them apart: each is its
		// own repair, though both name the sale.
		for _, amount := range []string{"2.00", "3.00"} {
			status, body := ccbillNotice(t, a, "Refund", map[string]any{"subscriptionId": ref, "transactionId": sale,
				"amount": amount, "currencyCode": "840", "reason": "customer request", "timestamp": ""})
			require.Equal(t, http.StatusOK, status, body)
		}
		unbooked, err := a.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: "consistency.ledger.unbooked"})
		require.NoError(t, err)
		require.Len(t, unbooked.Items, 2)
		require.Eventually(t, func() bool { return mail.count("consistency.ledger.unbooked") == 2 }, 20*time.Second, 20*time.Millisecond,
			"each repair alerts its operator")
		alertsSettled(t, fleet)
		require.Equal(t, 2, mail.count("consistency.ledger.unbooked"))
	})

	t.Run("OR-J06-S67/a-low-cranker-alerts-the-operator", func(t *testing.T) {
		mail := &alertMailbox{}
		s := newSolanaWorld(t, true, solanaFixtureOptions{alerts: mail})
		_, sub := s.subscribe(t)
		due := *sub.CurrentPeriodEndsAt
		s.node.SetSOLBalance(s.cranker.PublicKey(), 1_000)
		fleet := startFleet(t, s.world)
		settleOperations(t, fleet)
		s.clock.Advance(due.Add(pullMargin).Sub(s.clock.Now()))
		wakeOperations(t, fleet)
		require.Eventually(t, func() bool {
			low, err := s.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: "life.solana.cranker_low_gas"})
			return err == nil && len(low.Items) == 1
		}, 20*time.Second, 20*time.Millisecond, "a pull signed with a low cranker raises its finding")
		require.Eventually(t, func() bool { return mail.count("life.solana.cranker_low_gas") == 1 }, 20*time.Second, 20*time.Millisecond,
			"the high finding alerts its operator")
		settleOperations(t, fleet)
		alertsSettled(t, fleet)
		require.Equal(t, 1, mail.count("life.solana.cranker_low_gas"), "once")
	})

	t.Run("OR-J06-S68/a-refused-message-sender-alerts-the-operator-once", func(t *testing.T) {
		// The mail server refuses the sender of customer mail; the operator's
		// alert address still receives.
		mail := &senderRefusingMailbox{}
		a, _ := newPaymentWorld(t, func(w *world) {
			alertEmail(w)
			w.deps.Email = mail
		})
		mail.refuse = a.alice.Email
		alice := customerOf(a.alice)
		paidExternally(t, a, alice, messageSale(t, a), "refused-sender-alert")
		fleet := startFleet(t, a.world)
		require.Eventually(t, func() bool { return mail.count("life.customer.messages_sender_refused") == 1 }, 30*time.Second, 20*time.Millisecond,
			"the high finding alerts its operator")
		refused, err := a.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: "life.customer.messages_sender_refused"})
		require.NoError(t, err)
		require.Len(t, refused.Items, 1)
		alertsSettled(t, fleet)
		require.Equal(t, 1, mail.count("life.customer.messages_sender_refused"), "once, however often the message is retried")
	})

	t.Run("OR-J06-S69/a-rate-limited-customer-alerts-the-operator-once", func(t *testing.T) {
		mail := &alertMailbox{}
		a, _ := newPaymentWorld(t, func(w *world) {
			alertEmail(w)
			w.deps.Email = mail
		})
		alice := customerOf(a.alice)
		lines := messageSale(t, a)
		for i := range 12 {
			paidExternally(t, a, alice, lines, fmt.Sprintf("alert-capped-%02d", i))
		}
		fleet := startFleet(t, a.world)
		settledMessages(t, a.world, alice, 12)
		limited, err := a.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: "life.customer.messages_rate_limited"})
		require.NoError(t, err)
		require.Len(t, limited.Items, 1)
		finding := limited.Items[0].ID.UUID()
		alertsSettled(t, fleet)
		jobs, err := fleet.JobList(t.Context(), river.NewJobListParams().Kinds("openrails.operator_alert").First(100))
		require.NoError(t, err)
		alerts := 0
		for _, job := range jobs.Jobs {
			var args struct {
				Finding *struct {
					ID uuid.UUID `json:"id"`
				} `json:"finding"`
				Channel json.RawMessage `json:"channel"`
			}
			require.NoError(t, json.Unmarshal(job.EncodedArgs, &args))
			if args.Finding != nil && args.Finding.ID == finding && args.Channel == nil {
				alerts++
			}
		}
		require.Equal(t, 1, alerts, "two messages past the cap alert once")
		require.Zero(t, mail.count("life.customer.messages_rate_limited"), "a medium finding goes to alert webhooks, not email")
	})

	t.Run("OR-J06-S83/customer-cancels-spend-no-budget-and-card-removals-past-the-cap-alert-staff-once", func(t *testing.T) {
		a, journal := newNMIWorld(t, alertEmail)
		product, err := a.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "membership", DisplayName: "Membership"})
		require.NoError(t, err)
		price, err := a.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: "membership-day", UnitAmount: 10_000_000, Currency: "USD", BillingIntervalHours: ptr(24), AccessDurationHours: ptr(24)})
		require.NoError(t, err)
		psps, err := a.alpha.ListPSPs(t.Context(), billing.PSPListParams{})
		require.NoError(t, err)
		require.Len(t, psps.Items, 1)
		tokens := []string{a.aliceToken, a.bobToken}
		for range 5 {
			tokens = append(tokens, signIn(t, a.auth, authtest.NewUser(t, a.auth)).AccessToken)
		}
		var subs []*billing.Subscription
		for i, token := range tokens {
			input := billing.CreateMyInvoiceParams{Lines: []billing.InvoiceLineParams{{PriceID: price.ID}}, ExpectedTotal: ptr(int64(10_000_000)), Reusable: ptr(true),
				Payment: &billing.InvoicePaymentParams{Token: journal.core.Tokenize(nmimock.Card{Brand: "visa", Last4: "4242", Exp: "1235"}), BillingDetails: billedTo("US", "10001")}}
			status, raw := a.request(t, http.MethodPost, "/v1/me/invoices", token, input, map[string]string{"Idempotency-Key": fmt.Sprintf("wave-%d", i)})
			require.Equal(t, http.StatusCreated, status, string(raw))
			var invoice billing.Invoice
			require.NoError(t, json.Unmarshal(raw, &invoice))
			require.Equal(t, billing.InvoiceComplete, invoice.Status)
			require.NotNil(t, invoice.Lines[0].SubscriptionID)
			sub, err := a.alpha.GetSubscription(t.Context(), *invoice.Lines[0].SubscriptionID)
			require.NoError(t, err)
			require.Equal(t, "engine", sub.CollectionPolicy)
			subs = append(subs, sub)
		}
		cancel := func(i int) {
			t.Helper()
			status, raw := a.request(t, http.MethodPost, "/v1/me/subscriptions/"+subs[i].ID.String()+"/cancel", tokens[i], map[string]any{"reason": "Stop renewing"})
			require.Equal(t, http.StatusOK, status, string(raw))
			canceled, err := a.alpha.GetSubscription(t.Context(), subs[i].ID)
			require.NoError(t, err)
			require.Equal(t, billing.SubscriptionCanceled, canceled.Status)
		}
		for i := range 6 {
			cancel(i)
		}
		page, err := a.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: "life.customer_action_rate.exceeded"})
		require.NoError(t, err)
		require.Empty(t, page.Items, "customer cancellations raise no wave finding")

		methods := make([]*billing.PaymentMethod, 6)
		for i := range methods {
			status, raw := a.request(t, http.MethodPost, "/v1/me/payment-methods", tokens[i], billing.CreatePaymentMethodParams{PSPID: psps.Items[0].ID,
				Token: journal.core.Tokenize(nmimock.Card{Brand: "visa", Last4: "4242", Exp: "1235"}), BillingDetails: billedTo("US", "10001")})
			require.Equal(t, http.StatusCreated, status, string(raw))
			require.NoError(t, json.Unmarshal(raw, &methods[i]))
		}
		remove := func(i int) (int, []byte) {
			return a.request(t, http.MethodDelete, "/v1/me/payment-methods/"+methods[i].ID.String(), tokens[i], nil)
		}
		for i := range 5 {
			status, raw := remove(i)
			require.Equal(t, http.StatusNoContent, status, string(raw))
		}
		status, raw := remove(5)
		require.Equal(t, http.StatusTooManyRequests, status, string(raw))
		require.Contains(t, string(raw), `"code":"rate_limit_exceeded"`)
		require.Contains(t, string(raw), "try again in")
		kept, err := a.alpha.ListPaymentMethods(t.Context(), methods[5].CustomerID, billing.PaymentMethodListParams{})
		require.NoError(t, err)
		index := slices.IndexFunc(kept.Items, func(method billing.PaymentMethod) bool { return method.ID == methods[5].ID })
		require.NotEqual(t, -1, index, "a refused removal preserves the card")
		require.Equal(t, billing.PaymentMethodActive, kept.Items[index].Status)

		// A new customer can still cancel after card removals exhaust the budget.
		cancel(6)
		cancel(6)
		page, err = a.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: "life.customer_action_rate.exceeded"})
		require.NoError(t, err)
		require.Len(t, page.Items, 1)
		wave := page.Items[0]
		require.EqualValues(t, 5, wave.Evidence["card_removals"], "the finding names the wave: %v", wave.Evidence)
		require.NotEmpty(t, wave.Evidence["wave_started_at"])
		var alerts int
		require.NoError(t, a.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+pgx.Identifier{a.schema + "_river", "river_job"}.Sanitize()+
			" WHERE kind = 'openrails.operator_alert' AND args->'finding'->>'id' = $1", wave.ID.UUID().String()).Scan(&alerts))
		require.Equal(t, 1, alerts, "staff hear of the wave once")
		status, raw = remove(5)
		require.Equal(t, http.StatusTooManyRequests, status, string(raw))
		require.NoError(t, a.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+pgx.Identifier{a.schema + "_river", "river_job"}.Sanitize()+
			" WHERE kind = 'openrails.operator_alert' AND args->'finding'->>'id' = $1", wave.ID.UUID().String()).Scan(&alerts))
		require.Equal(t, 1, alerts, "a second refusal of the same wave alerts no one again")
	})

	t.Run("OR-J06-S70/a-held-terminal-outcome-alerts-the-operator-once", func(t *testing.T) {
		// The switch is off, as it ships: a declined charge that spends an NMI
		// schedule's dunning keeps the member's access, and tells the operator.
		mail := &alertMailbox{}
		b := newNMIBook(t, func(w *world) {
			alertEmail(w)
			w.deps.Email = mail
		})
		fleet := hostFleet(t, b.a.world)
		fleet.PeriodicJobs().Clear()
		require.NoError(t, b.a.engine.Start(t.Context(), openrails.WithRiverClient(fleet)))
		require.NoError(t, fleet.Start(t.Context()))
		asOf := b.started.Add(10 * 24 * time.Hour)
		member := billing.CustomerID(uuid.New())
		book := b.dunnedBook(asOf, member, "alerted-dunned")
		b.land(b.a.alpha, book)
		require.NotEqual(t, billing.SubscriptionCanceled, b.status(member))
		require.Eventually(t, func() bool { return mail.count("life.terminal_outcome.held") == 1 }, 20*time.Second, 20*time.Millisecond,
			"the held outcome alerts its operator")
		b.land(b.a.alpha, book)
		alertsSettled(t, fleet)
		require.Equal(t, 1, mail.count("life.terminal_outcome.held"), "once per case")
	})
}

// alertEmail gives alpha an operator alert address.
func alertEmail(w *world) {
	m := cfgMerchant(w.cfg, "alpha")
	m.AlertEmail = ptr("ops@alpha.example")
	setCfgMerchant(&w.cfg, "alpha", m)
}

// senderRefusingMailbox is an alertMailbox whose mail server refuses the
// sender of anything sent to refuse.
type senderRefusingMailbox struct {
	alertMailbox
	mu     sync.Mutex
	refuse string
}

func (m *senderRefusingMailbox) Send(ctx context.Context, email openrails.Email) error {
	m.mu.Lock()
	refused := m.refuse != "" && email.To == m.refuse
	m.mu.Unlock()
	if refused {
		return fmt.Errorf("smtp: MAIL FROM: %w", &textproto.Error{Code: 553, Msg: "5.7.1 sender address rejected"})
	}
	return m.alertMailbox.Send(ctx, email)
}
