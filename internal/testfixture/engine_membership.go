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
	"strings"
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
// customer confirmation entry, executes one initial charge against
// a loopback NMI, HyperSwitch or Stripe provider, qualifies its receipt and
// commits the shared writer.
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
	if method.Rail == "stripe" {
		return stripeEngineMembership(t, ctx, d, terms, method)
	}
	var account gen.OpenrailsCustodian
	if method.CustodianID != nil {
		account, err = d.Gen(ctx).GetCustodian(ctx, gen.GetCustodianParams{MerchantID: mid.UUID(), ID: *method.CustodianID})
		require.NoError(t, err)
	}
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
		case r.Method == http.MethodGet && r.URL.Path == "/customers/"+method.RailCustomerRef:
			fmt.Fprintf(w, `{"object":"customer","id":%q,"billing":[{"id":%q,"priority":1}]}`, method.RailCustomerRef, method.RailMethodRef)
		case r.URL.Path == "/transact" && r.Method == http.MethodPost:
			require.NoError(t, r.ParseForm())
			require.Equal(t, "customer", r.Form.Get("initiated_by"))
			require.Equal(t, "recurring", r.Form.Get("billing_method"))
			require.Empty(t, r.Form.Get("recurring"))
			require.Equal(t, amount, r.Form.Get("amount"))
			require.Equal(t, method.RailCustomerRef, r.Form.Get("customer_vault_id"))
			require.Equal(t, method.RailMethodRef, r.Form.Get("billing_id"))
			posts.Add(1)
			order.Store(r.Form.Get("orderid"))
			fmt.Fprintf(w, "response=1&response_code=100&transactionid=%s", transaction)
		case r.URL.Path == "/payments/"+transaction:
			fmt.Fprintf(w, `{"object":"transaction","id":%q,"amount":%q,"currency":%q,"customer_vault_id":%q,"billing_id":%q,"response":"1","actions":[{"id":%q,"type":"sale","amount":%q,"success":true,"response":"1"}]}`, transaction, amount, terms.Currency, method.RailCustomerRef, method.RailMethodRef, transaction+"-a", amount)
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
	cfg := &config.Config{TestMode: config.CredentialPostureSandbox, ProviderWriteMode: config.ProviderWriteModeFull}
	var secrets merchants.MerchantSecretReader
	if method.CustodianID != nil {
		cfg.HyperSwitch = &config.HyperSwitchConfig{APIBaseURL: server.URL}
		name, err := merchants.CustodianSecretName("hyperswitch", account.Environment, account.AccountID, "api_key")
		require.NoError(t, err)
		secrets = initialSecret{mid, name}
	}
	return confirmEngineMembership(t, ctx, d, terms, cfg, initialReader{client}, secrets, &posts)
}

func confirmEngineMembership(t testing.TB, ctx context.Context, d *db.DB, terms subscriptions.InitialMembershipTerms, cfg *config.Config, resolver intents.NMIClientResolver, secrets merchants.MerchantSecretReader, posts *atomic.Int64) gen.OpenrailsRailIntent {
	t.Helper()
	mid, err := merchant.Require(ctx)
	require.NoError(t, err)
	clock := clockwork.NewFakeClockAt(terms.AcceptedAt)
	service := &checkout.CheckoutService{SubscriptionService: subscriptions.NewSubscriptionService(d, nil, nil, nil, clock), Lifecycle: subscriptions.NewSubscriptionLifecycleService(d, nil, nil, nil, nil, payments.NewPaymentService(d, clock), clock), Config: cfg, MerchantSecrets: secrets}
	service.SetClock(clock)
	runner := &intents.Runner{Store: intents.NewStore(d), Registry: intents.NewRegistry(checkout.NewInitialMembershipIntentHandler(service, resolver)), Config: cfg, Clock: clock}
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

type stripeInitialReader struct{ service *subscriptions.StripeService }

func (r stripeInitialReader) ResolveNMIClient(context.Context, uuid.UUID, *uuid.UUID) (*nmi.NMIClient, bool, error) {
	return nil, false, fmt.Errorf("Stripe fixture cannot use NMI")
}
func (r stripeInitialReader) ResolveStripeEngineService(_ context.Context, mid uuid.UUID, psp *uuid.UUID) (*subscriptions.StripeService, bool, error) {
	owner, account := r.service.AccountIdentity()
	if owner != mid || psp == nil || *psp != account {
		return nil, false, fmt.Errorf("wrong Stripe account")
	}
	return r.service, true, nil
}

func stripeEngineMembership(t testing.TB, ctx context.Context, d *db.DB, terms subscriptions.InitialMembershipTerms, method gen.OpenrailsPaymentMethod) gen.OpenrailsRailIntent {
	t.Helper()
	minor, err := moneyutil.NativeToRailMinorExact(terms.Currency, terms.Amount)
	require.NoError(t, err)
	var pi map[string]any
	var posts atomic.Int64
	id := "pi_initial" + strings.ReplaceAll(terms.SubscriptionID.String(), "-", "")
	charge := "ch_initial" + strings.ReplaceAll(terms.SubscriptionID.String(), "-", "")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/payment_intents":
			require.NoError(t, r.ParseForm())
			require.Equal(t, "false", r.Form.Get("off_session"))
			require.Equal(t, "off_session", r.Form.Get("setup_future_usage"))
			require.Equal(t, fmt.Sprint(minor), r.Form.Get("amount"))
			metadata := map[string]string{}
			for key, values := range r.Form {
				if strings.HasPrefix(key, "metadata[") {
					metadata[strings.TrimSuffix(strings.TrimPrefix(key, "metadata["), "]")] = values[0]
				}
			}
			pi = map[string]any{"id": id, "status": "succeeded", "customer": method.RailCustomerRef, "payment_method": method.RailMethodRef, "amount": minor, "amount_received": minor, "currency": "usd", "setup_future_usage": "off_session", "capture_method": "automatic", "confirmation_method": "automatic", "latest_charge": charge, "metadata": metadata, "livemode": false}
			posts.Add(1)
			json.NewEncoder(w).Encode(pi)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/payment_intents/"+id:
			json.NewEncoder(w).Encode(pi)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/payment_intents":
			json.NewEncoder(w).Encode(map[string]any{"data": []any{pi}, "has_more": false})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/charges/"+charge:
			json.NewEncoder(w).Encode(map[string]any{"id": charge, "amount": minor, "amount_captured": minor, "currency": "usd", "customer": method.RailCustomerRef, "payment_method": method.RailMethodRef, "payment_intent": id, "paid": true, "captured": true, "status": "succeeded"})
		default:
			t.Errorf("unexpected initial Stripe request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", 500)
		}
	}))
	defer server.Close()
	mid, err := merchant.Require(ctx)
	require.NoError(t, err)
	cfg := &config.Config{TestMode: config.CredentialPostureSandbox, ProviderWriteMode: config.ProviderWriteModeFull}
	service := subscriptions.NewAccountStripeService(cfg, mid.UUID(), terms.PSPID, "acct_fixture_initial", "sk_test_fixture_initial")
	service.SetBaseURLForTest(server.URL)
	return confirmEngineMembership(t, ctx, d, terms, cfg, stripeInitialReader{service}, nil, &posts)
}
