//go:build integration

package critical_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/textproto"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
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
