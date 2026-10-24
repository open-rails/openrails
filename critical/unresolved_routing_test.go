//go:build integration

package critical_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/stripemock"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/require"
)

type accountWire struct {
	primary, alternate http.RoundTripper
}

func (w accountWire) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Header.Get("Authorization") == "Bearer sk_test_alternate" {
		return w.alternate.RoundTrip(r)
	}
	return w.primary.RoundTrip(r)
}

func TestUnresolvedRouting(t *testing.T) {
	t.Run("OR-J11-S04/unknown-debit-forbids-replacement-and-alternate-account", func(t *testing.T) {
		var alternate *providerLedger
		a, p := newPaymentWorld(t, func(w *world) {
			alternate = newProviderLedger(t, w)
			alternate.secret = "sk_test_alternate"
			target, err := url.Parse(alternate.server.URL)
			require.NoError(t, err)
			wire := http.DefaultTransport.(*http.Transport).Clone()
			t.Cleanup(wire.CloseIdleConnections)
			w.deps.StripeTransport = accountWire{primary: w.deps.StripeTransport, alternate: loopbackWire{target: target, wire: wire}}
			declaration := w.cfg.Merchants["alpha"]
			declaration.CardVault = ""
			declaration.PSPRouting = &[]billing.PSPRoutingRule{{Prefer: []string{"stripe", "alternate"}}}
			declaration.PSPs["alternate"] = openrails.StripePSP{AccountID: "acct_other", Environment: openrails.EnvironmentTest, SecretKey: "sk_test_alternate", WebhookSigningSecret: "whsec_alternate", PublishableKey: "pk_test_alternate"}.PSPConfig()
			w.cfg.Merchants["alpha"] = declaration
		})
		psps, err := a.alpha.ListPSPs(t.Context(), billing.PSPListParams{})
		require.NoError(t, err)
		require.Len(t, psps.Items, 2)
		var primaryID, alternateID billing.PSPID
		for _, psp := range psps.Items {
			if psp.Key == "stripe" {
				primaryID = psp.ID
			} else {
				alternateID = psp.ID
			}
		}
		input := saleInput(t, a, p)
		routing, err := a.alpha.PreviewPSPRouting(t.Context(), billing.PreviewPSPRoutingParams{PriceID: input.Lines[0].PriceID})
		require.NoError(t, err)
		t.Logf("declared account routing: %+v", routing.Candidates)
		// Positive control: the second account really is reachable and armed.
		status, raw := a.request(t, http.MethodPost, "/v1/me/payment-methods", a.aliceToken, billing.CreatePaymentMethodParams{PSPID: alternateID, Token: alternate.core.NewPaymentMethod(stripemock.Card{Brand: "visa", Last4: "5556"})})
		require.Equal(t, http.StatusCreated, status, string(raw))
		var saved billing.PaymentMethod
		require.NoError(t, json.Unmarshal(raw, &saved))
		control := input
		control.Payment = &billing.OrderPaymentParams{PaymentMethodID: &saved.ID}
		control.Reusable = nil
		status, raw = a.request(t, http.MethodPost, "/v1/me/orders", a.aliceToken, control, map[string]string{"Idempotency-Key": "alternate-positive-control"})
		require.Equal(t, http.StatusCreated, status, string(raw))
		require.Len(t, alternate.effects(t), 1)
		require.EqualValues(t, 3000, alternate.effects(t)[0].Amount)
		input.Payment.PSPID = &primaryID
		p.interruptPaymentResponses()
		p.unavailableEvidence(true)
		_, _ = a.request(t, http.MethodPost, "/v1/me/orders", a.aliceToken, input, map[string]string{"Idempotency-Key": "unresolved-account"})
		orders, err := a.alpha.ListOrders(t.Context(), billing.OrderListParams{CustomerID: billing.CustomerID(uuid.MustParse(a.alice.ID))})
		require.NoError(t, err)
		require.Len(t, orders.Items, 2)
		var order billing.Order
		for _, item := range orders.Items {
			if item.Payment.Status == billing.OrderPaymentProcessing {
				order = item
			}
		}
		require.Equal(t, billing.OrderPaymentProcessing, order.Payment.Status)
		for _, change := range []struct {
			key    string
			method billing.OrderPaymentParams
		}{
			{"replacement", billing.OrderPaymentParams{PSPID: &primaryID, Token: p.core.NewPaymentMethod(stripemock.Card{Brand: "visa", Last4: "4242"})}},
			{"alternate", *control.Payment},
		} {
			pay := billing.PayOrderParams{ExpectedTotal: 30_000_000, Payment: change.method}
			if change.method.Token != "" {
				pay.Reusable = ptr(true)
			}
			status, raw = a.request(t, http.MethodPost, "/v1/me/orders/"+order.ID.String()+"/pay", a.aliceToken, pay, map[string]string{"Idempotency-Key": change.key})
			require.Equal(t, http.StatusConflict, status, string(raw))
		}
		require.Len(t, p.effects(t), 1)
		require.Len(t, alternate.effects(t), 1, "only the independent positive control may debit the alternate account")
		retained, err := a.alpha.GetOrder(t.Context(), order.ID)
		require.NoError(t, err)
		require.Equal(t, billing.OrderPaymentProcessing, retained.Payment.Status)
		p.restorePaymentResponses()
		p.unavailableEvidence(false)
		a.clock.Advance(20 * time.Minute)
		fleet := startFleet(t, a.world)
		jobs, err := fleet.JobList(t.Context(), river.NewJobListParams().Kinds("openrails.provider_operation"))
		require.NoError(t, err)
		for _, job := range jobs.Jobs {
			if job.State == rivertype.JobStateScheduled || job.State == rivertype.JobStateRetryable {
				_, err = fleet.JobRetry(t.Context(), job.ID)
				require.NoError(t, err)
			}
		}
		require.Eventually(t, func() bool {
			out, err := a.alpha.GetOrder(t.Context(), order.ID)
			return err == nil && out.Status == billing.OrderComplete
		}, 20*time.Second, 20*time.Millisecond)
		retained, err = a.alpha.GetOrder(t.Context(), order.ID)
		require.NoError(t, err)
		payment, err := a.alpha.GetPayment(t.Context(), *retained.Payment.PaymentID)
		require.NoError(t, err)
		require.EqualValues(t, 30_000_000, payment.Amount)
		require.Equal(t, primaryID, *payment.PSPID)
		require.Len(t, p.effects(t), 1)
		require.Len(t, alternate.effects(t), 1)
		encoded, err := json.Marshal(payment)
		require.NoError(t, err)
		require.NotContains(t, string(encoded), "sk_test_")
	})
}
