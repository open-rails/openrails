//go:build integration

// Package testfixture contains shared production-workflow fixtures, not alternate
// writers. Provider responses are loopback observations; ledger and local effects
// are authored exclusively by the ordinary accepted-operation handlers.
package testfixture

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/checkout"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

type initialReader struct{ client *nmi.NMIClient }

func (r initialReader) ResolveNMIClient(context.Context, uuid.UUID, *uuid.UUID) (*nmi.NMIClient, bool, error) {
	return r.client, true, nil
}

type initialSecret struct {
	owner merchant.ID
	name  string
}

func (s initialSecret) Get(_ context.Context, owner merchant.ID, name string) (merchants.Secret, error) {
	if owner != s.owner || name != s.name {
		return merchants.Secret{}, merchants.ErrSecretNotFound
	}
	return merchants.Secret{Name: name, Value: "fixture-initial-key", Version: 1}, nil
}

// EngineMembership accepts explicit USD commercial terms through the verified
// customer confirmation entry, executes one initial HyperSwitch charge against
// a loopback provider, qualifies its receipt and commits the shared writer.
// It never infers the customer's agreement from mutable catalog values.
func EngineMembership(t testing.TB, ctx context.Context, d *db.DB, terms subscriptions.InitialMembershipTerms) gen.OpenrailsRailIntent {
	t.Helper()
	require.Equal(t, "USD", terms.Currency)
	if terms.Entitlements == nil {
		terms.Entitlements = map[string]*int{}
	}
	require.NoError(t, terms.Validate())
	mid, err := merchant.Require(ctx)
	require.NoError(t, err)
	method, err := d.Gen(ctx).GetPaymentMethodByID(ctx, gen.GetPaymentMethodByIDParams{MerchantID: mid.UUID(), ID: terms.PaymentMethodID})
	require.NoError(t, err)
	require.NotNil(t, method.CustodianID)
	account, err := d.Gen(ctx).GetCustodian(ctx, gen.GetCustodianParams{MerchantID: mid.UUID(), ID: *method.CustodianID})
	require.NoError(t, err)
	minor, err := moneyutil.NativeToRailMinorExact(terms.Currency, terms.Amount)
	require.NoError(t, err)
	amount := fmt.Sprintf("%d.%02d", minor/100, minor%100)
	transaction := "initial-" + terms.SubscriptionID.String()
	var order atomic.Value
	order.Store("")
	var posts atomic.Int64
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v2/payment-methods/"+method.RailMethodRef:
			json.NewEncoder(w).Encode(map[string]any{"id": method.RailMethodRef, "merchant_id": account.AccountID, "customer_id": method.RailCustomerRef, "storage_type": "persistent", "payment_method_data": map[string]any{"card": map[string]any{"last4_digits": "1111", "expiry_month": "12", "expiry_year": "2030"}}})
		case r.URL.Path == "/v2/proxy" && r.Method == http.MethodGet:
			fmt.Fprintf(w, `{"contract":"openrails-nmi-form-v2","strict":true,"max_response_bytes":65536,"routes":[{"destination_url":%q,"method":"POST","response_profile":"nmi_classic"}]}`, server.URL+"/transact")
		case r.URL.Path == "/v2/proxy":
			var input struct {
				Form map[string]string `json:"request_body"`
			}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&input))
			require.Equal(t, "customer", input.Form["initiated_by"])
			require.Equal(t, "recurring", input.Form["billing_method"])
			require.Empty(t, input.Form["recurring"])
			require.Equal(t, amount, input.Form["amount"])
			posts.Add(1)
			order.Store(input.Form["orderid"])
			fmt.Fprintf(w, `{"response":{"response":"1","response_code":"100","responsetext":"Approved","transactionid":%q},"status_code":200,"response_headers":{}}`, transaction)
		case r.URL.Path == "/payments/"+transaction:
			json.NewEncoder(w).Encode(map[string]any{"object": "transaction", "id": transaction, "amount": amount, "currency": terms.Currency, "response": "1", "actions": []map[string]any{{"id": transaction + "-a", "type": "sale", "amount": amount, "success": true, "response": "1"}}})
		case r.URL.Path == "/query":
			require.NoError(t, r.ParseForm())
			require.Equal(t, order.Load(), r.Form.Get("order_id"))
			require.NotEmpty(t, order.Load())
			fmt.Fprintf(w, `<nm_response><transaction><transaction_id>%s</transaction_id><order_id>%s</order_id><action><action_type>sale</action_type><success>1</success></action></transaction></nm_response>`, transaction, order.Load())
		default:
			t.Errorf("unexpected initial provider request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", 500)
		}
	}))
	defer server.Close()
	client, err := nmi.NewAccountClient(mid.UUID(), terms.PSPID, "initial-fixture", &config.NMIProviderSettings{SecurityKey: "fixture-initial-key", WebhookSecret: "fixture-initial-webhook"}, true)
	require.NoError(t, err)
	client.DirectPostURL, client.QueryURL, client.V5BaseURL = server.URL+"/transact", server.URL+"/query", server.URL
	clock := clockwork.NewFakeClockAt(terms.AcceptedAt)
	cfg := &config.Config{TestMode: config.CredentialPostureSandbox, ProviderWriteMode: config.ProviderWriteModeFull, HyperSwitch: &config.HyperSwitchConfig{APIBaseURL: server.URL}}
	name, err := merchants.CustodianSecretName("hyperswitch", account.Environment, account.AccountID, "api_key")
	require.NoError(t, err)
	service := &checkout.CheckoutService{SubscriptionService: subscriptions.NewSubscriptionService(d, nil, nil, nil, clock), Lifecycle: subscriptions.NewSubscriptionLifecycleService(d, nil, nil, nil, nil, payments.NewPaymentService(d, clock), clock), Config: cfg, MerchantSecrets: initialSecret{mid, name}}
	service.SetClock(clock)
	runner := &intents.Runner{Store: intents.NewStore(d), Registry: intents.NewRegistry(checkout.NewInitialMembershipIntentHandler(service, initialReader{client})), Config: cfg, Clock: clock}
	service.Intents = runner
	key := "engine-fixture-" + terms.SubscriptionID.String()
	_, err = service.ConfirmInitialMembership(ctx, terms, key, billingauth.DelegatedPrincipal{CredentialClass: billingauth.CredentialClassUserSession, MerchantID: mid.String(), SubjectID: terms.CustomerID.String()})
	require.NoError(t, err)
	op, err := intents.NewStore(d).GetByIdempotencyKey(ctx, checkout.InitialMembershipIdempotencyKey(key))
	require.NoError(t, err)
	require.Equal(t, intents.StatusSucceeded, op.Status, string(op.ResultEvidence))
	require.NoError(t, intents.ValidateInitialMembershipTerminal(op))
	require.EqualValues(t, 1, posts.Load())
	return op
}
