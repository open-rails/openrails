//go:build greenfield && integration

package greenfield_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/stretchr/testify/require"
)

// greenfieldNMIGateway is a deliberately small NMI wire. It accepts the
// read-only test-mode probe, vault creation, and classic sale used by the
// public NMI checkout path. No provider credentials or NMI package internals
// are used by the test.
type greenfieldNMIGateway struct {
	mu       sync.Mutex
	paths    []string
	vaults   atomic.Int32
	sales    atomic.Int32
	sequence atomic.Int32
}

func (g *greenfieldNMIGateway) RoundTrip(req *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	g.mu.Lock()
	g.paths = append(g.paths, req.URL.Path)
	g.mu.Unlock()

	write := func(status int, contentType, value string) (*http.Response, error) {
		return &http.Response{
			StatusCode: status,
			Header:     http.Header{"Content-Type": {contentType}},
			Body:       io.NopCloser(strings.NewReader(value)),
			Request:    req,
		}, nil
	}

	if strings.HasSuffix(req.URL.Path, "/query.php") {
		form, parseErr := url.ParseQuery(string(body))
		if parseErr != nil {
			return nil, parseErr
		}
		if form.Get("report_type") == "test_mode_status" {
			return write(http.StatusOK, "application/xml", `<nm_response><test_mode_enabled>true</test_mode_enabled></nm_response>`)
		}
		return write(http.StatusOK, "application/xml", `<nm_response></nm_response>`)
	}

	if req.Method == http.MethodPost && (strings.HasSuffix(req.URL.Path, "/customers") || strings.HasSuffix(req.URL.Path, "/customers/")) {
		id := fmt.Sprintf("vault_greenfield_%d", g.vaults.Add(1))
		return write(http.StatusOK, "application/json", fmt.Sprintf(`{"object":"customer","id":%q,"billing":[{"id":"bill_greenfield","priority":1}]}`, id))
	}

	if req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/transact.php") {
		form, parseErr := url.ParseQuery(string(body))
		if parseErr != nil {
			return nil, parseErr
		}
		if form.Get("type") != "sale" {
			return write(http.StatusOK, "application/x-www-form-urlencoded", "response=1&response_code=100")
		}
		id := fmt.Sprintf("txn_greenfield_%d", g.sales.Add(1))
		return write(http.StatusOK, "application/x-www-form-urlencoded", "response=1&response_code=100&responsetext=SUCCESS&authcode=OK&transactionid="+id)
	}

	if req.Method == http.MethodPost && strings.Contains(req.URL.Path, "/payments") {
		if strings.HasSuffix(req.URL.Path, "/payments/auth") {
			return write(http.StatusOK, "application/json", `{"object":"transaction","id":"txn_greenfield_probe","response":"1","response_code":"100","amount":"1.01","currency":"USD","actions":[{"id":"probe_action","type":"auth","success":true,"amount":"1.01"}]}`)
		}
		id := fmt.Sprintf("txn_greenfield_v5_%d", g.sequence.Add(1))
		return write(http.StatusOK, "application/json", fmt.Sprintf(`{"object":"transaction","id":%q,"response":"1","response_code":"100","amount":"10.00","currency":"USD","actions":[{"id":%q,"type":"sale","success":true,"amount":"10.00"}]}`, id, id+"_action"))
	}

	return nil, fmt.Errorf("unexpected fake NMI request: %s %s", req.Method, req.URL.Path)
}

func newNMITestRuntime(t *testing.T, f *fixture, slug string, gateway *greenfieldNMIGateway) (*embed.Runtime, *openrails.Client) {
	t.Helper()
	runtime, err := embed.New(t.Context(), embed.Options{
		Config: &config.Config{
			TestMode:            config.CredentialPostureSandbox,
			AllowCatalogUpdates: true,
			ProviderWriteMode:   config.ProviderWriteModeFull,
			DB:                  &config.DBConfig{URL: f.dsn(t), Schema: f.schema},
		},
		Merchant: &embed.MerchantDeclaration{Slug: slug, Config: embed.MerchantConfig{
			DisplayName: slug,
			PSPs: map[string]embed.PSPConfig{"nmi": {"nmi": {
				AccountID: "acct_greenfield_nmi",
				Secrets: map[string]string{
					"security_key":           "sk_test_greenfield_nmi",
					"webhook_signing_secret": "whsec_greenfield_nmi",
				},
				Settings: map[string]any{
					"tokenization_key": "pk_test_greenfield_nmi",
					"tokenization_url": "https://tokenize.greenfield.test",
				},
			}}},
		}},
		PGXPool:      f.pool,
		River:        embed.RiverManagedByOpenRails(f.schema),
		NMITransport: gateway,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, runtime.Close(context.Background())) })
	client, err := runtime.Client()
	require.NoError(t, err)
	return runtime, client
}

func TestNMIProviderOwnedSubscriptionPreservesProviderDunning(t *testing.T) {
	f := newFixture(t)
	gateway := &greenfieldNMIGateway{}
	_, client := newNMITestRuntime(t, f, "nmi-provider-owned-"+uuid.NewString()[:8], gateway)

	product, err := client.Products.Create(t.Context(), &openrails.ProductCreateParams{
		Key:              "provider-membership-" + uuid.NewString()[:8],
		DisplayName:      "Provider membership",
		EntitlementsSpec: map[string]*int{"membership:provider": nil},
	})
	require.NoError(t, err)
	hours := 720
	price, err := client.Prices.Create(t.Context(), &openrails.PriceCreateParams{
		ProductID:           product.ID,
		Key:                 "provider-membership-usd-" + uuid.NewString()[:8],
		UnitAmount:          5_000_000,
		Currency:            "USD",
		AutoRenew:           true,
		AccessDurationHours: &hours,
	})
	require.NoError(t, err)

	customer := openrails.CustomerID(uuid.New())
	asOf := time.Now().UTC()
	lastRetry := time.Now().UTC().Add(-2 * time.Hour)
	book := openrails.DeclaredBilling{
		AsOf:      asOf,
		Customers: []openrails.DeclaredCustomer{{Customer: customer}},
		Subscriptions: []openrails.DeclaredSubscription{{
			CollectionPolicy:   "provider_dunning",
			SourceID:           "mobius-subscription-1",
			Customer:           customer,
			Price:              mustPriceID(t, price.ID),
			PSP:                openrails.PSPRef{Key: "nmi"},
			Rail:               "nmi",
			RailSubscriptionID: "nmi_sub_provider_1",
			StartedAt:          time.Now().UTC().Add(-24 * time.Hour),
			PaidThrough:        ptrTime(time.Now().UTC().Add(6 * 24 * time.Hour)),
			Dunning:            &openrails.DunningEvidence{Retries: 2, LastRetryAt: &lastRetry, ScheduleLive: true},
			PaymentMethod:      &openrails.PaymentMethodRef{Rail: "nmi", RailCustomerRef: "vault_provider_1", RailMethodRef: "bill_provider_1"},
		}},
		PaymentMethods: []openrails.DeclaredPaymentMethod{{
			Customer: customer, Rail: "nmi", PSP: openrails.PSPRef{Key: "nmi"}, RailCustomerRef: "vault_provider_1", RailMethodRef: "bill_provider_1", LastFour: "1111", CardType: "visa", ExpiryDate: "12/30",
		}},
		Transactions: []openrails.DeclaredTransaction{{
			RailSubscriptionID: "nmi_sub_provider_1", TransactionID: "nmi_txn_provider_1", Success: true, AmountCents: 500, Currency: "USD", OccurredAt: bookTime(asOf),
		}},
	}
	result, err := client.ImportBilling(t.Context(), book)
	require.NoError(t, err)
	require.Contains(t, result.Imported, "mobius-subscription-1")

	page, err := client.ListSubscriptions(t.Context(), openrails.SubscriptionFilter{CustomerID: customer.String()})
	require.NoError(t, err)
	require.Len(t, page.Data, 1)
	sub := page.Data[0]
	require.Equal(t, "provider_dunning", sub.CollectionPolicy)
	require.Equal(t, "nmi_sub_provider_1", sub.RailSubscriptionID)
	require.Equal(t, 2, *sub.RetryAttempts)
	require.NotNil(t, sub.GraceEndsAt)
	require.NotNil(t, sub.PaymentMethodID)

	replay, err := client.ImportBilling(t.Context(), book)
	require.NoError(t, err)
	require.Contains(t, replay.Skipped, "mobius-subscription-1")
	require.EqualValues(t, 0, gateway.sales.Load(), "provider-owned import must not submit a new charge")
}

func TestNMIEngineOwnedSubscriptionAdmissionUsesOpenRailsState(t *testing.T) {
	f := newFixture(t)
	gateway := &greenfieldNMIGateway{}
	slug := "nmi-engine-owned-" + uuid.NewString()[:8]
	customer := openrails.CustomerID(uuid.New())
	runtime, err := embed.New(t.Context(), embed.Options{
		Config: &config.Config{
			TestMode:            config.CredentialPostureSandbox,
			AllowCatalogUpdates: true,
			ProviderWriteMode:   config.ProviderWriteModeFull,
			DB:                  &config.DBConfig{URL: f.dsn(t), Schema: f.schema},
		},
		Merchant: &embed.MerchantDeclaration{Slug: slug, Config: embed.MerchantConfig{
			DisplayName: slug,
			PSPs: map[string]embed.PSPConfig{"nmi": {"nmi": {
				AccountID: "acct_greenfield_nmi",
				Secrets:   map[string]string{"security_key": "sk_test_greenfield_nmi", "webhook_signing_secret": "whsec_greenfield_nmi"},
				Settings:  map[string]any{"tokenization_key": "pk_test_greenfield_nmi", "tokenization_url": "https://tokenize.greenfield.test"},
			}}},
		}},
		PGXPool: f.pool, River: embed.RiverManagedByOpenRails(f.schema), NMITransport: gateway,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, runtime.Close(context.Background())) })
	client, err := runtime.Client()
	require.NoError(t, err)

	product, err := client.Products.Create(t.Context(), &openrails.ProductCreateParams{
		Key:              "engine-membership-" + uuid.NewString()[:8],
		DisplayName:      "Engine membership",
		EntitlementsSpec: map[string]*int{"membership:engine": nil},
	})
	require.NoError(t, err)
	hours := 720
	price, err := client.Prices.Create(t.Context(), &openrails.PriceCreateParams{
		ProductID: product.ID, Key: "engine-membership-usd-" + uuid.NewString()[:8],
		UnitAmount: 5_000_000, Currency: "USD", AutoRenew: true, AccessDurationHours: &hours,
	})
	require.NoError(t, err)

	checkoutConfig, err := client.GetCheckoutConfig(t.Context())
	require.NoError(t, err)
	var nmiPSPID string
	for _, psp := range checkoutConfig.PSPs {
		if psp.Key == "nmi" {
			nmiPSPID = psp.PSPID
			break
		}
	}
	require.NotEmpty(t, nmiPSPID)
	_, err = client.ImportBilling(t.Context(), openrails.DeclaredBilling{
		AsOf: time.Now().UTC(), Customers: []openrails.DeclaredCustomer{{Customer: customer}},
		PaymentMethods: []openrails.DeclaredPaymentMethod{{
			Customer: customer, Rail: "nmi", PSP: openrails.PSPRef{Key: "nmi"},
			RailCustomerRef: "vault_engine_1", RailMethodRef: "bill_engine_1", InitialTransactionID: "nmi_initial_engine_1",
			LastFour: "4242", CardType: "visa", ExpiryDate: "12/30",
		}},
	})
	require.NoError(t, err)
	methods, err := client.ListPaymentMethods(t.Context(), customer.String(), openrails.PageOptions{})
	require.NoError(t, err)
	require.Len(t, methods.Data, 1)
	methodID, err := openrails.ParsePaymentMethodID(methods.Data[0].ID)
	require.NoError(t, err)

	// The public merchant Client admits the recurring checkout, but the NMI
	// initial charge is customer-confirmed. This proves engine ownership before
	// confirmation: no NMI recurring schedule is created and no sale is sent.
	session, err := client.CreateCheckoutSession(t.Context(), openrails.CreateCheckoutSessionRequest{
		Customer: openrails.CheckoutCustomerIdentity{ID: customer.String(), VerifiedEmail: "engine@example.test"},
		PriceID:  price.ID, Entitlement: "membership:engine", OfferKind: openrails.OfferRecurring,
		PaymentOptions: openrails.CheckoutPaymentOptions{PSPID: nmiPSPID, Rail: "nmi", PaymentMethodID: methodID.String()},
		IdempotencyKey: "nmi-engine-" + uuid.NewString(),
	})
	require.NoError(t, err)
	require.Equal(t, "subscription", session.Mode)
	require.Equal(t, "requires_action", session.Status)
	require.Nil(t, session.SubscriptionID)
	require.Zero(t, gateway.sales.Load(), "engine-owned admission must not charge before customer confirmation")
	require.NotContains(t, session.RailData, "plan_id", "engine-owned NMI must not create a provider schedule")
}

func ptrTime(t time.Time) *time.Time { return &t }

func bookTime(t time.Time) time.Time { return t.Add(-time.Hour) }

func mustPriceID(t *testing.T, raw string) openrails.PriceID {
	t.Helper()
	id, err := openrails.ParsePriceID(raw)
	require.NoError(t, err)
	return id
}
