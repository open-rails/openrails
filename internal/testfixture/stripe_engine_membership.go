//go:build integration

package testfixture

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/checkout"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

type stripeInitialReader struct{ service *subscriptions.StripeService }

func (r stripeInitialReader) ResolveNMIClient(context.Context, uuid.UUID, *uuid.UUID) (*nmi.NMIClient, bool, error) {
	return nil, false, errors.New("Stripe initial fixture cannot use NMI")
}
func (r stripeInitialReader) ResolveStripeEngineService(_ context.Context, mid uuid.UUID, psp *uuid.UUID) (*subscriptions.StripeService, bool, error) {
	owner, account := r.service.AccountIdentity()
	return r.service, psp != nil && owner == mid && account == *psp, nil
}

// StripeEngineMembership authors the accepted initial operation, qualified
// captured receipt, payment and membership through the ordinary user confirmation
// and execution path. Renewal tests inherit this persisted commercial agreement.
func StripeEngineMembership(t testing.TB, ctx context.Context, d *db.DB, terms subscriptions.InitialMembershipTerms) gen.OpenrailsRailIntent {
	t.Helper()
	require.NoError(t, terms.Validate())
	mid, err := merchant.Require(ctx)
	require.NoError(t, err)
	method, err := d.Gen(ctx).GetPaymentMethodByID(ctx, gen.GetPaymentMethodByIDParams{MerchantID: mid.UUID(), ID: terms.PaymentMethodID})
	require.NoError(t, err)
	minor, err := moneyutil.NativeToRailMinorExact(terms.Currency, terms.Amount)
	require.NoError(t, err)
	var pi map[string]any
	var mu sync.Mutex
	var posts atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		var out any
		switch {
		case r.Method == "POST" && r.URL.Path == "/v1/payment_intents":
			posts.Add(1)
			require.NoError(t, r.ParseForm())
			require.Equal(t, "false", r.Form.Get("off_session"))
			require.Equal(t, "off_session", r.Form.Get("setup_future_usage"))
			metadata := map[string]string{}
			for key, values := range r.Form {
				if strings.HasPrefix(key, "metadata[") {
					metadata[strings.TrimSuffix(strings.TrimPrefix(key, "metadata["), "]")] = values[0]
				}
			}
			pi = map[string]any{"id": "pi_initial", "status": "succeeded", "customer": method.RailCustomerRef, "payment_method": method.RailMethodRef, "amount": minor, "amount_received": minor, "currency": strings.ToLower(terms.Currency), "setup_future_usage": "off_session", "capture_method": "automatic", "confirmation_method": "automatic", "latest_charge": "ch_initial", "metadata": metadata, "livemode": false}
			out = pi
		case r.Method == "GET" && r.URL.Path == "/v1/payment_intents/pi_initial":
			out = pi
		case r.Method == "GET" && r.URL.Path == "/v1/charges/ch_initial":
			out = map[string]any{"id": "ch_initial", "payment_intent": "pi_initial", "customer": method.RailCustomerRef, "payment_method": method.RailMethodRef, "amount": minor, "amount_captured": minor, "currency": strings.ToLower(terms.Currency), "status": "succeeded", "paid": true, "captured": true}
		default:
			t.Errorf("unexpected initial Stripe request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", 500)
			return
		}
		require.NoError(t, json.NewEncoder(w).Encode(out))
	}))
	defer server.Close()
	clock := clockwork.NewFakeClockAt(terms.AcceptedAt)
	cfg := &config.Config{TestMode: config.CredentialPostureSandbox, ProviderWriteMode: config.ProviderWriteModeFull}
	stripe := subscriptions.NewAccountStripeService(cfg, mid.UUID(), terms.PSPID, "acct_fixture", "sk_test_fixture")
	stripe.SetBaseURLForTest(server.URL)
	service := &checkout.CheckoutService{SubscriptionService: subscriptions.NewSubscriptionService(d, nil, nil, nil, clock), Lifecycle: subscriptions.NewSubscriptionLifecycleService(d, nil, nil, nil, nil, payments.NewPaymentService(d, clock), clock), Config: cfg}
	service.SetClock(clock)
	runner := &intents.Runner{Store: intents.NewStore(d), Registry: intents.NewRegistry(checkout.NewInitialMembershipIntentHandler(service, stripeInitialReader{stripe})), Config: cfg, Clock: clock}
	service.Intents = runner
	key := "stripe-engine-fixture-" + terms.SubscriptionID.String()
	_, err = service.ConfirmInitialMembership(ctx, terms, key, billingauth.DelegatedPrincipal{CredentialClass: billingauth.CredentialClassUserSession, MerchantID: mid.String(), SubjectID: terms.CustomerID.String()})
	require.NoError(t, err)
	op, err := intents.NewStore(d).GetByIdempotencyKey(ctx, checkout.InitialMembershipIdempotencyKey(key))
	require.NoError(t, err)
	require.Equal(t, intents.StatusSucceeded, op.Status)
	require.NoError(t, intents.ValidateInitialMembershipTerminal(op))
	require.EqualValues(t, 1, posts.Load())
	return op
}
