//go:build integration

package critical_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	embedded "github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/nmimock"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/require"
)

func purchasedSale(t *testing.T, configure ...func(*world)) (*authorityWorld, *providerLedger, billing.Invoice) {
	t.Helper()
	a, p := newPaymentWorld(t, configure...)
	input := saleInput(t, a, p)
	status, raw := a.request(t, http.MethodPost, "/v1/me/invoices", a.aliceToken, input, map[string]string{"Idempotency-Key": "sale-for-refund"})
	require.Equal(t, http.StatusCreated, status, string(raw))
	var invoice billing.Invoice
	require.NoError(t, json.Unmarshal(raw, &invoice))
	require.Equal(t, billing.InvoiceComplete, invoice.Status)
	require.NotNil(t, invoice.Payment.PaymentID)
	return a, p, invoice
}

func purchasedNMISale(t *testing.T) (*authorityWorld, *nmiJournal, billing.PaymentID) {
	t.Helper()
	a, p := newNMIWorld(t)
	invoice := nmiCheckoutInvoice(t, a, "refund-sale", false, 48*time.Hour)
	invoice = payNMICheckout(t, a, invoice, p.core.Tokenize(nmimock.Card{Brand: "visa", Last4: "4242"}), true)
	require.Equal(t, billing.InvoiceComplete, invoice.Status)
	require.NotNil(t, invoice.Payment.PaymentID)
	return a, p, *invoice.Payment.PaymentID
}

func nmiRefundRequest(r *http.Request) bool {
	return r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/refund")
}

func nmiRefundOperation(t *testing.T, a *authorityWorld, payment billing.PaymentID, key string, verify bool) gen.BillingProviderIntent {
	t.Helper()
	rt := embedded.Graph(a.engine).Runtime
	var row gen.BillingProviderIntent
	require.NoError(t, rt.DB.RunInMerchantConn(merchant.WithID(t.Context(), a.alpha.MerchantID()), func(ctx context.Context) error {
		var err error
		row, err = intents.NewStore(rt.DB).GetByIdempotencyKey(ctx, intents.RefundIdempotencyKey(payment.UUID(), key))
		if err == nil && verify {
			row, err = rt.IntentRunner().VerifyByID(ctx, row.ID)
		}
		return err
	}))
	return row
}

func TestRefundRecovery(t *testing.T) {
	t.Run("OR-J20-S02/overlapping-refunds-conserve-original-charge", func(t *testing.T) {
		a, p, invoice := purchasedSale(t)
		payment := *invoice.Payment.PaymentID
		type answer struct {
			refund *billing.Payment
			err    error
		}
		answers := make(chan answer, 2)
		start := make(chan struct{})
		for _, key := range []string{"overlap-first", "overlap-second"} {
			go func() {
				<-start
				refund, err := a.alpha.RefundPayment(t.Context(), key, payment, billing.RefundPaymentParams{Amount: 20_000_000, Reason: "Overlapping request"})
				answers <- answer{refund, err}
			}()
		}
		close(start)
		succeeded := 0
		for range 2 {
			result := <-answers
			if result.err == nil {
				succeeded++
				require.NotNil(t, result.refund)
				require.Equal(t, billing.PaymentSucceeded, result.refund.Status)
				require.EqualValues(t, -20_000_000, result.refund.Amount)
			}
		}
		require.Equal(t, 1, succeeded, "only one overlapping refund fits the settled charge")
		read, err := a.alpha.GetPayment(t.Context(), payment)
		require.NoError(t, err)
		require.EqualValues(t, 20_000_000, read.AmountRefunded)
		require.Len(t, read.Refunds, 1)
		final, err := a.alpha.RefundPayment(t.Context(), "overlap-remainder", payment, billing.RefundPaymentParams{Full: true, Reason: "Return remainder"})
		require.NoError(t, err)
		require.EqualValues(t, -10_000_000, final.Amount)
		var debited, refunded int64
		for _, effect := range p.effects(t) {
			if effect.Kind == "charge" {
				debited += effect.Amount
			} else if effect.Kind == "refund" {
				refunded += effect.Amount
			} else {
				t.Fatalf("unknown effect %s", effect.Kind)
			}
		}
		require.EqualValues(t, 3000, debited)
		require.EqualValues(t, 3000, refunded)
		read, err = a.alpha.GetPayment(t.Context(), payment)
		require.NoError(t, err)
		require.Equal(t, billing.PaymentRefunded, read.Status)
		require.EqualValues(t, 30_000_000, read.AmountRefunded)
	})
	t.Run("OR-J20-S03/lost-refund-response-resolves-original-provider-refund", func(t *testing.T) {
		// Begun an hour ago: the refund settles when the retry finds it,
		// twenty minutes on, and a payment is never dated in the future.
		a, p, invoice := purchasedSale(t, func(w *world) { w.clock.Advance(-time.Hour) })
		payment := *invoice.Payment.PaymentID
		p.mu.Lock()
		p.lostRefundResponse = true
		p.mu.Unlock()
		params := billing.RefundPaymentParams{Amount: 10_000_000, Reason: "Refund with response lost"}
		_, _ = a.alpha.RefundPayment(t.Context(), "refund-response-lost", payment, params)
		effects := p.effects(t)
		require.Len(t, effects, 2, "provider must actually have returned money before the loss")
		var refundID string
		for _, effect := range effects {
			if effect.Kind == "refund" {
				refundID = effect.ID
				require.EqualValues(t, 1000, effect.Amount)
			}
		}
		require.NotEmpty(t, refundID)
		before, err := a.alpha.GetPayment(t.Context(), payment)
		require.NoError(t, err)
		require.Zero(t, before.AmountRefunded, "the fault must leave local refund receipt unresolved")
		p.mu.Lock()
		p.lostRefundResponse = false
		p.mu.Unlock()
		a.clock.Advance(20 * time.Minute)
		fleet := startFleet(t, a.world)
		jobs, err := fleet.JobList(t.Context(), river.NewJobListParams().Kinds("openrails.provider_operation"))
		require.NoError(t, err)
		for _, job := range jobs.Jobs {
			if job.State != rivertype.JobStateCompleted && job.State != rivertype.JobStateRunning {
				_, err = fleet.JobRetry(t.Context(), job.ID)
				require.NoError(t, err)
			}
		}
		require.Eventually(t, func() bool {
			read, err := a.alpha.GetPayment(t.Context(), payment)
			return err == nil && read.AmountRefunded == 10_000_000
		}, 20*time.Second, 20*time.Millisecond)
		replay, err := a.alpha.RefundPayment(t.Context(), "refund-response-lost", payment, params)
		require.NoError(t, err)
		require.Equal(t, billing.PaymentSucceeded, replay.Status)
		require.EqualValues(t, -10_000_000, replay.Amount)
		read, err := a.alpha.GetPayment(t.Context(), payment)
		require.NoError(t, err)
		require.Len(t, read.Refunds, 1)
		require.Len(t, p.effects(t), 2, "recovery and caller replay cannot refund twice")
	})
	t.Run("OR-J20-S07/an-nmi-refund-nmi-refuses-is-released-not-held", func(t *testing.T) {
		for _, profile := range []string{"already-refunded", "gateway-refusal", "duplicate"} {
			t.Run(profile, func(t *testing.T) {
				a, p, payment := purchasedNMISale(t)
				if profile == "already-refunded" {
					p.core.Refund(p.core.LastSale().TransactionID, 500)
				} else {
					text := "Transaction was rejected by gateway"
					if profile == "duplicate" {
						text = "Duplicate transaction"
					}
					p.core.Intercept(nmiRefundRequest, func(r *http.Request, _ func() *http.Response) (*http.Response, error) {
						return answer(r, http.StatusOK, `{"response":"3","response_code":"300","response_text":"`+text+`"}`), nil
					})
				}
				refund, err := a.alpha.RefundPayment(t.Context(), "refused", payment, billing.RefundPaymentParams{Full: true, Reason: "Customer requested a refund"})
				row := nmiRefundOperation(t, a, payment, "refused", false)
				require.NotNil(t, row.LastFailureReason)
				reason := *row.LastFailureReason
				read, readErr := a.alpha.GetPayment(t.Context(), payment)
				require.NoError(t, readErr)
				require.Zero(t, read.AmountRefunded)
				if profile == "duplicate" {
					require.NoError(t, err)
					require.Equal(t, billing.PaymentPending, refund.Status)
					require.Equal(t, intents.StatusUnknownNeedsVerify, row.Status)
					a.clock.Advance(time.Hour)
					row = nmiRefundOperation(t, a, payment, "refused", true)
					require.Equal(t, intents.StatusUnknownNeedsVerify, row.Status)
					require.Equal(t, reason, *row.LastFailureReason)
					return
				}
				require.Error(t, err)
				code, ok := billing.ErrorCodeFrom(err)
				require.True(t, ok)
				require.Equal(t, billing.CodeRefundFailed, code)
				require.Equal(t, intents.StatusFailedTerminal, row.Status)
				require.Contains(t, reason, "nmi refund refused")
				var evidence struct {
					ResponseCode int  `json:"response_code"`
					StatusCode   int  `json:"status_code"`
					NoEffect     bool `json:"destructive_no_effect"`
				}
				require.NoError(t, json.Unmarshal(row.ResultEvidence, &evidence))
				require.True(t, evidence.NoEffect)
				if profile == "already-refunded" {
					require.Equal(t, http.StatusBadRequest, evidence.StatusCode)
				} else {
					require.Equal(t, 300, evidence.ResponseCode)
				}
				payload, err := intents.DecodeRefundPayload(row)
				require.NoError(t, err)
				reservation, err := a.alpha.GetPayment(t.Context(), billing.PaymentID(payload.ReservationID))
				require.NoError(t, err)
				require.Equal(t, billing.PaymentFailed, reservation.Status)
				for _, reversal := range read.Refunds {
					require.NotEqual(t, billing.PaymentPending, reversal.Status)
				}
				p.core.ClearIntercepts()
				refund, err = a.alpha.RefundPayment(t.Context(), "remainder", payment, billing.RefundPaymentParams{Full: true, Reason: "Return the remainder"})
				if profile == "already-refunded" {
					code, ok := billing.ErrorCodeFrom(err)
					require.True(t, ok)
					require.Equal(t, billing.CodeRefundFailed, code, "NMI's real balance decides; the failed attempt holds no amount")
				} else {
					require.NoError(t, err)
					require.EqualValues(t, -5_000_000, refund.Amount)
					require.Equal(t, billing.PaymentSucceeded, refund.Status)
				}
				row = nmiRefundOperation(t, a, payment, "refused", false)
				require.Equal(t, reason, *row.LastFailureReason)
			})
		}
	})
	t.Run("OR-J20-S08/an-unresolved-nmi-refund-is-named-and-closed-by-the-operator", func(t *testing.T) {
		for _, profile := range []string{"lost-response", "lost-request"} {
			t.Run(profile, func(t *testing.T) {
				a, p, payment := purchasedNMISale(t)
				sale := p.core.LastSale()
				other := p.core.AddSale(nmimock.Sale{Vault: sale.Vault, Currency: "USD", Amount: "5.00", At: a.clock.Now()})
				foreignRefund := p.core.Refund(other.TransactionID, 500)
				if profile == "lost-request" {
					_, err := a.alpha.RefundPayment(t.Context(), "recorded", payment, billing.RefundPaymentParams{Amount: 1_000_000, Reason: "First partial refund"})
					require.NoError(t, err)
				}
				p.core.Intercept(nmiRefundRequest, func(r *http.Request, serve func() *http.Response) (*http.Response, error) {
					if profile == "lost-request" {
						return nil, errors.New("connection lost before NMI received the request")
					}
					response := serve()
					require.NoError(t, response.Body.Close())
					return answer(r, http.StatusServiceUnavailable, ""), nil
				})
				refund, err := a.alpha.RefundPayment(t.Context(), "unknown", payment, billing.RefundPaymentParams{Full: true, Reason: "Customer requested a refund"})
				require.NoError(t, err)
				require.Equal(t, billing.PaymentPending, refund.Status)
				p.core.ClearIntercepts()
				row := nmiRefundOperation(t, a, payment, "unknown", false)
				require.Equal(t, intents.StatusUnknownNeedsVerify, row.Status)
				require.NotNil(t, row.LastFailureReason)
				reason := *row.LastFailureReason
				a.clock.Advance(time.Hour)
				row = nmiRefundOperation(t, a, payment, "unknown", true)
				require.Equal(t, intents.StatusUnknownNeedsVerify, row.Status)
				require.Equal(t, reason, *row.LastFailureReason)
				findings, err := a.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: intents.RefundUnresolvedFinding, Status: "requires_review"})
				require.NoError(t, err)
				require.Len(t, findings.Items, 1)
				finding := findings.Items[0]
				require.Equal(t, row.ID.String(), finding.SubjectKey)
				require.Equal(t, "high", finding.Severity)
				require.Contains(t, *finding.RecommendedAction, "openrails intents resolve")
				if profile == "lost-response" {
					require.Contains(t, *finding.RecommendedAction, "5.00 USD")
				} else {
					require.Contains(t, *finding.RecommendedAction, "4.00 USD")
				}
				require.Equal(t, payment.String(), finding.Evidence["payment_id"])
				require.Equal(t, reason, finding.Evidence["reason"])
				rt := embedded.Graph(a.engine).Runtime
				resolve := func(resolution intents.Resolution) error {
					return rt.DB.RunInMerchantConn(merchant.WithID(t.Context(), a.alpha.MerchantID()), func(ctx context.Context) error {
						resolved, err := rt.IntentRunner().Resolve(ctx, row.ID, resolution)
						if err == nil {
							row = resolved
						}
						return err
					})
				}
				release := intents.Resolution{NotExecuted: true, Actor: "operator", Reason: "Checked NMI's refunds and attest release"}
				p.core.QueryUnavailable(true)
				require.ErrorIs(t, resolve(release), intents.ErrResolutionRejected)
				p.core.QueryUnavailable(false)
				if profile == "lost-response" {
					var realRefund string
					for _, recordedSale := range p.core.Sales() {
						if recordedSale.TransactionID == sale.TransactionID {
							realRefund = recordedSale.RefundIDs[0]
						}
					}
					require.NotEmpty(t, realRefund)
					err := resolve(release)
					require.ErrorIs(t, err, intents.ErrResolutionRejected)
					require.ErrorContains(t, err, realRefund)
					err = resolve(intents.Resolution{ProviderReference: foreignRefund, Actor: "operator", Reason: "Check a different sale on the same vault"})
					require.ErrorIs(t, err, intents.ErrResolutionRejected)
					require.ErrorContains(t, err, "is not a refund of transaction")
					require.NoError(t, resolve(intents.Resolution{ProviderReference: realRefund, Actor: "operator", Reason: "Found the exact refund at NMI"}))
					require.Equal(t, intents.StatusSucceeded, row.Status)
					read, err := a.alpha.GetPayment(t.Context(), payment)
					require.NoError(t, err)
					require.EqualValues(t, 5_000_000, read.AmountRefunded)
				} else {
					require.NoError(t, resolve(release))
					require.Equal(t, intents.StatusFailedTerminal, row.Status)
					require.Equal(t, "operator attested release; NMI shows no unrecorded refund of the sale", *row.LastFailureReason)
					var evidence struct {
						OperatorResolution struct {
							Actor string `json:"actor"`
						} `json:"operator_resolution"`
						NoEffect bool `json:"destructive_no_effect"`
					}
					require.NoError(t, json.Unmarshal(row.ResultEvidence, &evidence))
					require.Equal(t, "operator", evidence.OperatorResolution.Actor)
					require.True(t, evidence.NoEffect)
					reservation, err := a.alpha.GetPayment(t.Context(), refund.ID)
					require.NoError(t, err)
					require.Equal(t, billing.PaymentFailed, reservation.Status)
					remaining, err := a.alpha.RefundPayment(t.Context(), "released-remainder", payment, billing.RefundPaymentParams{Full: true, Reason: "Refund the released remainder"})
					require.NoError(t, err)
					require.EqualValues(t, -4_000_000, remaining.Amount)
				}
				findings, err = a.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: intents.RefundUnresolvedFinding, Status: "fixed"})
				require.NoError(t, err)
				require.Len(t, findings.Items, 1)
				require.Equal(t, finding.ID, findings.Items[0].ID)
				active, err := a.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: intents.RefundUnresolvedFinding, Status: "requires_review"})
				require.NoError(t, err)
				require.Empty(t, active.Items)
			})
		}
	})

	t.Run("OR-J20-S09/a-full-dashboard-refund-ends-the-nmi-engine-membership", func(t *testing.T) {
		for _, full := range []bool{true, false} {
			t.Run(map[bool]string{true: "full-refund", false: "partial-refund"}[full], func(t *testing.T) {
				a, p, sub := nmiEngineMembership(t)
				due := *sub.CurrentPeriodEndsAt
				access := func(want bool) {
					t.Helper()
					check, err := a.alpha.CheckEntitlements(t.Context(), billing.CheckEntitlementsParams{CustomerID: sub.CustomerID, Entitlements: []string{"nmi:engine"}})
					require.NoError(t, err)
					require.Equal(t, want, check.Entitlements["nmi:engine"])
				}
				access(true)
				paid, err := a.alpha.ListPayments(t.Context(), billing.PaymentListParams{SubscriptionID: sub.ID})
				require.NoError(t, err)
				require.Len(t, paid.Items, 1)
				sale := p.core.LastSale()
				cents, amount := int64(1000), "10.00"
				if !full {
					cents, amount = 400, "4.00"
				}
				a.clock.Advance(time.Hour)
				refund := p.core.Refund(sale.TransactionID, cents)
				event := map[string]any{
					"event_id": "dashboard-refund", "event_type": "transaction.refund.success",
					"event_body": map[string]any{
						"transaction_id": refund, "amount": amount, "currency": "USD",
						"merchant":    map[string]string{"id": "critical-gateway"},
						"transaction": map[string]string{"transaction_id": sale.TransactionID},
					},
				}
				status, raw := nmiSignedDelivery(t, a, event, "dashboard-refund")
				require.Equal(t, http.StatusOK, status, raw)
				current, err := a.alpha.GetSubscription(t.Context(), sub.ID)
				require.NoError(t, err)
				access(!full)
				if !full {
					require.Equal(t, billing.SubscriptionActive, current.Status)
					require.Nil(t, current.EndedAt)
					require.Equal(t, sub.CurrentPeriodEndsAt, current.CurrentPeriodEndsAt, "renewal stays scheduled")
					return
				}
				require.Equal(t, billing.SubscriptionStatus("canceled"), current.Status)
				require.NotNil(t, current.EndedAt)
				require.False(t, current.EndedAt.After(a.clock.Now()))
				notifications := func() int {
					t.Helper()
					var count int
					require.NoError(t, a.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+pgx.Identifier{a.schema, "notification_events"}.Sanitize()+" WHERE merchant_id=$1 AND customer_id=$2", a.alpha.MerchantID().UUID(), sub.CustomerID.UUID()).Scan(&count))
					return count
				}
				before := notifications()
				a.clock.Advance(time.Minute)
				event["event_id"] = "dashboard-refund-redelivered"
				status, raw = nmiSignedDelivery(t, a, event, "dashboard-refund-redelivered")
				require.Equal(t, http.StatusOK, status, raw)
				replayed, err := a.alpha.GetSubscription(t.Context(), sub.ID)
				require.NoError(t, err)
				require.Equal(t, current, replayed)
				require.Equal(t, before, notifications())
				payment, err := a.alpha.GetPayment(t.Context(), paid.Items[0].ID)
				require.NoError(t, err)
				require.Len(t, payment.Refunds, 1)
				ledger := p.core.Ledger("")
				a.clock.Advance(due.Add(time.Hour).Sub(a.clock.Now()))
				runRegistered(t, startFleet(t, a.world), "openrails.billing_pass", 1)
				require.Equal(t, ledger, p.core.Ledger(""), "the refunded membership must never charge again")
				access(false)
			})
		}
	})

	t.Run("OR-J20-S70/a-dashboard-refund-lands-while-the-roster-cannot-be-read", func(t *testing.T) {
		a, p, invoice := purchasedSale(t)
		payment := *invoice.Payment.PaymentID
		var charge string
		for _, effect := range p.effects(t) {
			if effect.Kind == "charge" {
				charge = effect.ID
			}
		}
		require.NotEmpty(t, charge)
		// A book too large to read in one pass: the full read never gets past
		// the subscription roster.
		p.core.Intercept(func(r *http.Request) bool { return r.Method == http.MethodGet && r.URL.Path == "/v1/subscriptions" },
			func(r *http.Request, _ func() *http.Response) (*http.Response, error) {
				return answer(r, http.StatusServiceUnavailable, `{"error":{"type":"api_error","message":"roster unavailable"}}`), nil
			})
		_, err := p.core.Refund(charge, 1000)
		require.NoError(t, err)
		a.clock.Advance(10 * time.Minute)
		fleet := quietFleet(t, a.world)
		// The full read cannot finish, so the refresh fails incomplete and
		// River would retry it.
		attempt := refreshAttempt(t, fleet, a.alpha.MerchantID().UUID())
		require.Equal(t, river.EventKindJobFailed, attempt.Kind)
		require.NotEmpty(t, attempt.Job.Errors)
		require.Contains(t, attempt.Job.Errors[len(attempt.Job.Errors)-1].Error, "incomplete")
		_, err = fleet.JobCancel(t.Context(), attempt.Job.ID)
		require.NoError(t, err)
		read, err := a.alpha.GetPayment(t.Context(), payment)
		require.NoError(t, err)
		require.EqualValues(t, 10_000_000, read.AmountRefunded, "the reversals read records a refund the full read never reaches")
		require.Len(t, read.Refunds, 1)
		// The roster reads again: the full read finds the refund already recorded.
		p.core.ClearIntercepts()
		a.clock.Advance(10 * time.Minute)
		refreshMerchant(t, fleet, a.alpha.MerchantID().UUID())
		read, err = a.alpha.GetPayment(t.Context(), payment)
		require.NoError(t, err)
		require.EqualValues(t, 10_000_000, read.AmountRefunded, "both reads record the refund once")
		require.Len(t, read.Refunds, 1)
	})
}
