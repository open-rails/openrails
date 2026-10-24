//go:build integration

package critical_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/nmimock"
	"github.com/open-rails/openrails/internal/stripemock"
	"github.com/stretchr/testify/require"
)

func saveMethod(t *testing.T, a *authorityWorld, p *providerLedger, bearer, last4 string, decline ...string) *billing.PaymentMethod {
	t.Helper()
	psps, err := a.alpha.ListPSPs(t.Context(), billing.PSPListParams{})
	require.NoError(t, err)
	require.Len(t, psps.Items, 1)
	card := stripemock.Card{Brand: "visa", Last4: last4}
	if len(decline) > 0 {
		card.Decline = decline[0]
	}
	token := p.core.NewPaymentMethod(card)
	status, raw := a.request(t, http.MethodPost, "/v1/me/payment-methods", bearer, billing.CreatePaymentMethodParams{PSPID: psps.Items[0].ID, Token: token})
	require.Equal(t, http.StatusCreated, status, string(raw))
	var method billing.PaymentMethod
	require.NoError(t, json.Unmarshal(raw, &method))
	return &method
}

func TestPaymentMethodContracts(t *testing.T) {
	t.Run("OR-J12-S04/provider-detach-after-acceptance-preserves-payment-and-replay", func(t *testing.T) {
		a, p := newPaymentWorld(t)
		saved := saveMethod(t, a, p, a.aliceToken, "4242")
		setup := p.core.Submitted("/v1/setup_intents")
		require.Len(t, setup, 1)
		input := saleInput(t, a, p)
		input.Payment, input.Reusable = &billing.OrderPaymentParams{PaymentMethodID: &saved.ID}, nil
		gate := &wireGate{arrived: make(chan struct{}), release: make(chan struct{})}
		p.mu.Lock()
		p.responseGate = gate
		p.mu.Unlock()
		defer gate.unblock()
		type response struct {
			status int
			raw    []byte
			err    error
		}
		finished := make(chan response, 1)
		go func() {
			status, raw, err := a.do(t.Context(), http.MethodPost, "/v1/me/orders", a.aliceToken, input, map[string]string{"Idempotency-Key": "owned-card-sale"})
			finished <- response{status, raw, err}
		}()
		select {
		case <-gate.arrived:
		case <-time.After(10 * time.Second):
			t.Fatal("order did not reach the provider acceptance barrier")
		}
		require.Len(t, p.effects(t), 1)
		status, raw := a.request(t, http.MethodDelete, "/v1/me/payment-methods/"+saved.ID.String(), a.aliceToken, nil)
		require.Equal(t, http.StatusBadRequest, status, string(raw))
		require.Contains(t, string(raw), `"code":"payment_method_delete_unsupported"`)
		// Stripe owns removal for this rail. The provider detaches its card
		// after accepting the debit, before OpenRails receives that evidence.
		p.core.Detach(setup[0].Form.Get("payment_method"))
		gate.unblock()
		answer := <-finished
		require.NoError(t, answer.err)
		require.Equal(t, http.StatusCreated, answer.status, string(answer.raw))
		var accepted billing.Order
		require.NoError(t, json.Unmarshal(answer.raw, &accepted))
		require.Equal(t, billing.OrderComplete, accepted.Status)
		status, raw = a.request(t, http.MethodPost, "/v1/me/orders", a.aliceToken, input, map[string]string{"Idempotency-Key": "owned-card-sale"})
		require.Equal(t, http.StatusCreated, status, string(raw))
		var replay billing.Order
		require.NoError(t, json.Unmarshal(raw, &replay))
		require.Equal(t, accepted.ID, replay.ID)
		require.Equal(t, accepted.Payment.PaymentID, replay.Payment.PaymentID)
		payment, err := a.alpha.GetPayment(t.Context(), *accepted.Payment.PaymentID)
		require.NoError(t, err)
		require.Equal(t, billing.PaymentSucceeded, payment.Status)
		require.EqualValues(t, 30_000_000, payment.Amount)
		require.Len(t, p.effects(t), 1)
		require.EqualValues(t, 3000, p.effects(t)[0].Amount)
	})
	t.Run("OR-J12-S02/foreign-reused-and-removed-card-references-cannot-pay", func(t *testing.T) {
		a, p := newPaymentWorld(t)
		saved := saveMethod(t, a, p, a.aliceToken, "4242")
		calls := p.core.Submitted("/v1/setup_intents")
		require.Len(t, calls, 1)
		token := calls[0].Form.Get("payment_method")
		require.NotEmpty(t, token)
		request := billing.CreatePaymentMethodParams{PSPID: *saved.PSPID, Token: token}
		status, raw := a.request(t, http.MethodPost, "/v1/me/payment-methods", a.aliceToken, request)
		require.Equal(t, http.StatusCreated, status, string(raw))
		var replay billing.PaymentMethod
		require.NoError(t, json.Unmarshal(raw, &replay))
		require.Equal(t, saved.ID, replay.ID)
		require.Len(t, p.core.Submitted("/v1/setup_intents"), 1)
		status, raw = a.request(t, http.MethodPost, "/v1/me/payment-methods", a.bobToken, request)
		require.Equal(t, http.StatusPaymentRequired, status, string(raw))
		require.Contains(t, string(raw), `"code":"payment_method_stale"`)
		status, raw = a.request(t, http.MethodPost, "/v1/me/payment-methods/"+saved.ID.String()+"/confirm", a.bobToken, billing.ConfirmPaymentMethodParams{})
		require.Equal(t, http.StatusNotFound, status, string(raw))
		status, raw = a.request(t, http.MethodDelete, "/v1/me/payment-methods/"+saved.ID.String(), a.bobToken, nil)
		require.Equal(t, http.StatusNotFound, status, string(raw))
		status, raw = a.request(t, http.MethodDelete, "/v1/me/payment-methods/"+saved.ID.String(), a.aliceToken, nil)
		require.Equal(t, http.StatusBadRequest, status, string(raw))
		require.Contains(t, string(raw), `"code":"payment_method_delete_unsupported"`)
		status, raw = a.request(t, http.MethodGet, "/v1/me/payment-methods", a.bobToken, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		var methods billing.ListPage[billing.PaymentMethod]
		require.NoError(t, json.Unmarshal(raw, &methods))
		require.Empty(t, methods.Items)
		require.Empty(t, p.effects(t))
		facts, err := a.alpha.ListEvents(t.Context(), billing.EventQuery{Types: []string{"payment.settled", "order.completed"}, Limit: 100})
		require.NoError(t, err)
		require.Empty(t, facts.Events)

		// NMI owns the public removal contract; a truly removed stored method
		// must be unusable afterward, even with its formerly valid public ID.
		nmi, journal := newNMIWorld(t)
		enableLifecycleMaintenance(t, nmi)
		psps, err := nmi.alpha.ListPSPs(t.Context(), billing.PSPListParams{})
		require.NoError(t, err)
		require.Len(t, psps.Items, 1)
		status, raw = nmi.request(t, http.MethodPost, "/v1/me/payment-methods", nmi.aliceToken, billing.CreatePaymentMethodParams{PSPID: psps.Items[0].ID, Token: journal.core.Tokenize(nmimock.Card{Last4: "4242", Exp: "1235"})})
		require.Equal(t, http.StatusCreated, status, string(raw))
		var old billing.PaymentMethod
		require.NoError(t, json.Unmarshal(raw, &old))
		status, raw = nmi.request(t, http.MethodDelete, "/v1/me/payment-methods/"+old.ID.String(), nmi.aliceToken, nil)
		require.Equal(t, http.StatusNoContent, status, string(raw))
		product, err := nmi.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "removed-card", DisplayName: "Removed card purchase"})
		require.NoError(t, err)
		price, err := nmi.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, UnitAmount: 30_000_000, Currency: "USD"})
		require.NoError(t, err)
		input := billing.CreateMyOrderParams{Lines: []billing.OrderLineParams{{PriceID: price.ID}}, ExpectedTotal: ptr(int64(30_000_000)), Payment: &billing.OrderPaymentParams{PaymentMethodID: &old.ID}}
		status, raw = nmi.request(t, http.MethodPost, "/v1/me/orders", nmi.aliceToken, input, map[string]string{"Idempotency-Key": "removed-card-purchase"})
		require.Equal(t, http.StatusPaymentRequired, status, string(raw))
		require.Contains(t, string(raw), `"code":"payment_method_stale"`)
		journal.money(t, 0, 0)
		facts, err = nmi.alpha.ListEvents(t.Context(), billing.EventQuery{Types: []string{"payment.settled", "order.completed"}, Limit: 100})
		require.NoError(t, err)
		require.Empty(t, facts.Events)
	})
	t.Run("OR-J12-S03/default-and-attached-card-change-preserve-accepted-instrument", func(t *testing.T) {
		a, p, sub := purchasedMembership(t)
		replacement := saveMethod(t, a, p, a.aliceToken, "5556")
		require.Equal(t, billing.PaymentMethodActive, replacement.Status)
		due := *sub.CurrentPeriodEndsAt
		gate := &wireGate{arrived: make(chan struct{}), release: make(chan struct{})}
		p.mu.Lock()
		p.responseGate = gate
		p.mu.Unlock()
		defer gate.unblock()
		a.clock.Advance(due.Sub(a.clock.Now()))
		fleet := startFleet(t, a.world)
		runRegistered(t, fleet, "openrails.dunning", 1)
		select {
		case <-gate.arrived:
		case <-time.After(15 * time.Second):
			t.Fatal("accepted renewal never reached provider")
		}
		status, raw := a.request(t, http.MethodPut, "/v1/me/default-payment-methods/USD", a.aliceToken, billing.SetDefaultPaymentMethodParams{PaymentMethodID: replacement.ID})
		require.Equal(t, http.StatusOK, status, string(raw))
		_, err := a.alpha.SetSubscriptionPaymentMethod(t.Context(), sub.ID, billing.SetSubscriptionPaymentMethodParams{PaymentMethodID: &replacement.ID})
		require.Error(t, err)
		var refusal *billing.StatusError
		require.ErrorAs(t, err, &refusal)
		require.Equal(t, "rebill_terms_committed", refusal.Code)
		gate.unblock()
		renewalSettled(t, a, p, sub, due)
		current, err := a.alpha.GetSubscription(t.Context(), sub.ID)
		require.NoError(t, err)
		require.Equal(t, sub.PaymentMethodID, current.PaymentMethodID)
		payments, err := a.alpha.ListPayments(t.Context(), billing.PaymentListParams{CustomerID: sub.CustomerID})
		require.NoError(t, err)
		require.Len(t, payments.Items, 2)
		for _, payment := range payments.Items {
			require.EqualValues(t, 10_000_000, payment.Amount)
		}
		calls := p.core.Submitted("/v1/payment_intents")
		require.Len(t, calls, 2)
		require.Equal(t, calls[0].Form.Get("payment_method"), calls[1].Form.Get("payment_method"))
	})
	t.Run("OR-J12-S01/owned-card-setup-confirms-once-without-a-charge", func(t *testing.T) {
		a, p := newPaymentWorld(t)
		saved := saveMethod(t, a, p, a.aliceToken, "4242", "auth")
		require.Equal(t, billing.PaymentMethodRequiresAction, saved.Status)
		require.NotNil(t, saved.NextAction)
		setup, _, ok := strings.Cut(saved.NextAction.Payload["client_secret"], "_secret_")
		require.True(t, ok)
		require.True(t, p.core.AuthenticateSetup(setup), "only the external bank completes authentication")
		for range 2 {
			status, raw := a.request(t, http.MethodPost, "/v1/me/payment-methods/"+saved.ID.String()+"/confirm", a.aliceToken, billing.ConfirmPaymentMethodParams{})
			require.Equal(t, http.StatusOK, status, string(raw))
			var confirmed billing.PaymentMethod
			require.NoError(t, json.Unmarshal(raw, &confirmed))
			require.Equal(t, saved.ID, confirmed.ID)
			require.Equal(t, billing.PaymentMethodActive, confirmed.Status)
			require.Equal(t, saved.CustomerID, confirmed.CustomerID)
			require.Nil(t, confirmed.NextAction)
			require.Equal(t, "4242", *confirmed.Card.Last4)
		}
		status, raw := a.request(t, http.MethodGet, "/v1/me/payment-methods", a.aliceToken, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		var methods billing.ListPage[billing.PaymentMethod]
		require.NoError(t, json.Unmarshal(raw, &methods))
		require.Len(t, methods.Items, 1)
		require.Equal(t, saved.ID, methods.Items[0].ID)
		require.Len(t, p.core.Submitted("/v1/setup_intents"), 1)
		require.Empty(t, p.effects(t), "saving and confirming a card never collects money")
	})
}
