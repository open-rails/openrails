package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	billingidentity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/http/middleware"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	solanamodule "github.com/open-rails/openrails/internal/modules/solana"
	solanatokens "github.com/open-rails/openrails/internal/modules/solana/tokens"
	"github.com/open-rails/openrails/internal/railresolve"
	billingservice "github.com/open-rails/openrails/internal/service"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/open-rails/openrails/pkg/pricing"
)

// Priced checkout refuses operation selectors and malformed saved methods
// before any service is reached.
func TestPricedCheckoutRejectsBeforeEngine(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{`{"mode":"one_off"}`, "dedicated setup or subscription action"},
		{`{"subscription_id":"x"}`, "dedicated setup or subscription action"},
		{`{"new_price_id":"x"}`, "dedicated setup or subscription action"},
		{`{"payment":{"payment_method_id":"550e8400-e29b-41d4-a716-446655440000"}}`, "invalid payment_method_id"},
		{`{"payment":{"payment_method_id":"price_550e8400-e29b-41d4-a716-446655440000"}}`, "invalid payment_method_id"},
		{`{"payment":{"payment_method_id":"pm_00000000-0000-0000-0000-000000000000"}}`, "invalid payment_method_id"},
	} {
		r, rec := newTestRequest(http.MethodPost, "/v1/merchant/checkout-sessions", strings.NewReader(tc.body), nil)
		ServiceCreateCheckoutSession(r)
		require.Equal(t, http.StatusBadRequest, rec.Code, tc.body)
		require.Contains(t, rec.Body.String(), tc.want)
	}
}

// Only the customer's own interactive session may take a payment action; the
// checkout principal copies verified facts without promoting the class.
func TestCustomerActionRequiresInteractiveSession(t *testing.T) {
	classes := []billingauth.CredentialClass{billingauth.CredentialClassUnknown, billingauth.CredentialClassAutomation, billingauth.CredentialClassUserSession}
	for _, class := range classes {
		for _, invoker := range []string{"", "automation-agent"} {
			wire := httptest.NewRequest(http.MethodPost, "/v1/me/invoices/id/pay-now", strings.NewReader(`{"credential_class":"user_session"}`))
			wire.Header.Set("Credential-Class", "user_session")
			rec := httptest.NewRecorder()
			r := httprequest.NewHTTP(rec, wire, nil)
			payer, mid := uuid.NewString(), merchant.ID(uuid.New())
			r.SetUserContext(billingauth.UserContext{UserID: payer})
			r.Set(middleware.PrincipalContextKey, &middleware.Principal{CredentialType: middleware.CredentialHostDelegatedUser, CredentialClass: class, Invoker: invoker, MerchantID: mid, Subject: payer})

			_, ok := customerActionPayer(r)
			require.Equal(t, class == billingauth.CredentialClassUserSession && invoker == "", ok, "%s/%q", class, invoker)
			if !ok {
				require.Equal(t, http.StatusForbidden, rec.Code)
				require.Contains(t, rec.Body.String(), "customer_action_required")
			}
			require.Equal(t, billingauth.DelegatedPrincipal{CredentialClass: class, Invoker: invoker, SubjectID: payer, MerchantID: mid.String()}, checkoutVerifiedPrincipal(r))
		}
	}
	r, rec := newTestRequest(http.MethodPost, "/", nil, nil)
	_, ok := customerActionPayer(r)
	require.False(t, ok, "no principal is no customer action")
	require.Equal(t, http.StatusForbidden, rec.Code)
}

// The payment Idempotency-Key is caller text that may be logged: it is
// bounded and scanned for card numbers before it is used.
func TestPaymentActionKeyRefusesCardData(t *testing.T) {
	for key, ok := range map[string]bool{
		"archive-key-1461":           true,
		" padded ":                   true,
		"":                           false,
		strings.Repeat("k", 256):     false,
		"4111111111111111":           false,
		"opaque-4111 1111 1111 1111": false,
	} {
		wire := httptest.NewRequest(http.MethodPost, "/", nil)
		wire.Header.Set("Idempotency-Key", key)
		rec := httptest.NewRecorder()
		got, accepted := paymentActionKey(httprequest.NewHTTP(rec, wire, nil))
		require.Equal(t, ok, accepted, "%q", key)
		if ok {
			require.Equal(t, strings.TrimSpace(key), got)
		} else {
			require.Equal(t, http.StatusBadRequest, rec.Code)
			require.Contains(t, rec.Body.String(), "invalid_param")
			require.NotContains(t, rec.Body.String(), "4111")
		}
	}
}

// Card data never reaches OpenRails raw, and tokenized billing details round
// trip into the vault request and back out without the provider payload.
func TestPaymentMethodRequestMapping(t *testing.T) {
	for _, field := range []string{"card_number", "number", "pan", "primary_account_number", "cvv", "cvc", "cvn", "security_code", "verification_value"} {
		var create createPaymentMethodRequest
		require.NoError(t, json.Unmarshal([]byte(`{"payment_token":"tok","`+field+`":"4111111111111111"}`), &create))
		require.ErrorContains(t, create.rejectRawCardFields(), field)
		var update updatePaymentMethodRequest
		require.NoError(t, json.Unmarshal([]byte(`{"payment_token":"tok","`+field+`":"123"}`), &update))
		require.ErrorContains(t, update.rejectRawCardFields(), field)
	}
	var clean createPaymentMethodRequest
	require.NoError(t, json.Unmarshal([]byte(`{"payment_token":"tok"}`), &clean))
	require.NoError(t, clean.rejectRawCardFields())

	for _, tc := range []struct{ country, postal, zip, want string }{
		{"US", "", "80202", "80202"},
		{"DE", "10115", "", "10115"},
		{"JP", "100-0001", "", "100-0001"},
		{"BR", "01001-000", "", "01001-000"},
	} {
		got := toCreatePaymentMethodRequest(&createPaymentMethodRequest{
			PaymentToken: "opaque-provider-token", Provider: "mobius", NameOnCard: "Ada Lovelace",
			BillingCountry: tc.country, PostalCode: tc.postal, Zip: tc.zip, Phone: "+49 30 123456", LastFour: "card-4242",
		}, "ada@example.com")
		require.Equal(t, []any{"opaque-provider-token", "mobius", tc.country, tc.want, "4242", "ada@example.com", "", ""},
			[]any{got.PaymentToken, got.Provider, got.Country, got.Zip, got.LastFour, got.Email, got.FirstName, got.LastName})
		for key, want := range map[string]any{"name_on_card": "Ada Lovelace", "billing_country": tc.country, "postal_code": tc.want, "billing_email": "ada@example.com", "billing_phone": "+49 30 123456"} {
			require.Equal(t, want, got.Metadata[key], key)
		}
		for _, key := range []string{"payment_token", "raw_tokenization_payload", "pan", "cvv"} {
			require.NotContains(t, got.Metadata, key)
		}
	}
	legacy := toCreatePaymentMethodRequest(&createPaymentMethodRequest{
		PaymentToken: "tok", FirstName: "Ada", LastName: "Lovelace", Address1: "1 Main", Address2: "Unit 2", City: "Denver", State: "CO", Zip: "80202", Country: "US",
	}, "")
	require.Equal(t, []any{"Ada", "Lovelace", "US", "80202", "1 Main", "Unit 2", "Denver", "CO"},
		[]any{legacy.FirstName, legacy.LastName, legacy.Country, legacy.Zip, legacy.Metadata["billing_address1"], legacy.Metadata["billing_address2"], legacy.Metadata["billing_city"], legacy.Metadata["billing_state"]})

	last4, brand := "4242", "Visa"
	out := paymentMethodToAPI(&models.PaymentMethod{Rail: "mobius", LastFour: &last4, CardType: &brand, Metadata: map[string]any{
		"name_on_card": "Ada Lovelace", "billing_email": "ada@example.com", "billing_country": "JP", "postal_code": "100-0001", "billing_address1": "1 Chiyoda",
	}}, nil)
	require.NotNil(t, out.BillingDetails)
	require.NotNil(t, out.BillingDetails.Address)
	require.Equal(t, []any{"Ada Lovelace", "ada@example.com", "JP", "100-0001", "1 Chiyoda"},
		[]any{*out.BillingDetails.Name, *out.BillingDetails.Email, *out.BillingDetails.Address.Country, *out.BillingDetails.Address.PostalCode, *out.BillingDetails.Address.Line1})
}

// #589: a method is active unless its card expired or its last charge failed;
// a card is valid through the end of its expiry month.
func TestPaymentMethodHealth(t *testing.T) {
	ptr := func(s string) *string { return &s }
	thisMonth := time.Now().UTC().Format("01/06")
	require.Equal(t, "", cardExpiryStatus(nil))
	require.Equal(t, "", cardExpiryStatus(ptr("not-a-date")))
	require.Equal(t, "expired", cardExpiryStatus(ptr("01/20")))
	require.Equal(t, "valid", cardExpiryStatus(ptr("12/99")))
	require.Equal(t, "expiring_soon", cardExpiryStatus(&thisMonth))

	at := time.Date(2026, 6, 24, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		expiry, charge, outcome string
		active                  bool
	}{
		{"12/99", "", "", true},
		{"12/99", "completed", "success", true},
		{"12/99", "refunded", "refunded", true},
		{"12/99", "pending", "pending", true},
		{"12/99", "failed", "failed", false},
		{"01/20", "completed", "success", false},
	} {
		var charge *models.PaymentMethodCharge
		if tc.charge != "" {
			charge = &models.PaymentMethodCharge{LastChargedAt: at, Status: tc.charge}
		}
		h := paymentMethodHealthFrom(&models.PaymentMethod{ExpiryDate: ptr(tc.expiry)}, charge)
		require.Equal(t, []any{tc.active, tc.outcome}, []any{h.Active, h.LastChargeOutcome}, "%+v", tc)
		require.Equal(t, charge != nil, h.LastChargedAt != nil)
	}
}

func TestUsageMeterAndRateCardInput(t *testing.T) {
	spec, err := usageMeterSpec(" Storage.GB ", adminUsageMeterRequest{
		EventType: " storage.used ", ValueProperty: " bytes ", Aggregation: " SUM ", Unit: " GB ",
		GroupBy: map[string]string{" region ": " metadata.region "},
	})
	require.NoError(t, err)
	require.Equal(t, billingservice.UsageMeterSpec{
		Key: "storage-gb", EventType: "storage.used", ValueProperty: "bytes", Aggregation: pricing.AggregationSum,
		Unit: "GB", GroupBy: map[string]string{"region": "metadata.region"},
	}, spec)
	_, err = usageMeterSpec("requests", adminUsageMeterRequest{Aggregation: pricing.AggregationMax})
	require.EqualError(t, err, "usage meter aggregation must be sum or count")

	product := uuid.New()
	price := func(currency string) pricing.RatePrice {
		return pricing.RatePrice{Model: pricing.ModelPerUnit, Currency: currency, PerUnit: &pricing.PerUnitPrice{UnitAmount: 100}}
	}
	meter := billingservice.UsageMeterDTO{Key: "storage-gb", GroupBy: map[string]string{"region": "metadata.region"}}
	input, err := defaultUsageRateCardInput(meter, adminDefaultUsageRateCardRequest{
		ProductID: openrails.ProductID(product).String(), Filter: map[string][]string{" region ": {" eu ", "eu"}}, Price: price("usd"),
	})
	require.NoError(t, err)
	require.Equal(t, product, *input.ProductID)
	require.Equal(t, "storage-gb", input.MeterKey)
	require.Equal(t, "USD", input.Price.Currency)
	require.Equal(t, map[string][]string{"region": {"eu"}}, input.Filter)

	_, err = defaultUsageRateCardInput(meter, adminDefaultUsageRateCardRequest{ProductID: openrails.ProductID(product).String(), Price: price("GBP")})
	require.EqualError(t, err, `money: unknown currency "GBP"`)
	_, err = defaultUsageRateCardInput(meter, adminDefaultUsageRateCardRequest{ProductID: "nope", Price: price("USD")})
	require.EqualError(t, err, "product_id required")
}

// Metering catalog writes follow the catalog policy, never the secret backend.
func TestAdminCatalogOwnership(t *testing.T) {
	for _, backend := range []string{config.SecretBackendSnapshot, config.SecretBackendDB, config.SecretBackendVault} {
		for _, allow := range []bool{false, true} {
			r, _ := newTestRequest(http.MethodGet, "/", nil, &app.Runtime{Config: &config.Config{SecretBackend: backend, AllowCatalogUpdates: allow}})
			meter := adminUsageMeterDTO(r, billingservice.UsageMeterDTO{Key: "requests"})
			require.Equal(t, []any{"database", allow}, []any{meter.ConfigurationSource, meter.WritesAllowed}, backend)
		}
	}
	r, _ := newTestRequest(http.MethodGet, "/", nil, nil)
	source, allow := adminCatalogOwnership(r)
	require.Equal(t, []any{"database", false}, []any{source, allow})
}

func TestSelfUsageWindow(t *testing.T) {
	now := time.Date(2040, 5, 15, 12, 30, 0, 0, time.UTC)
	clock := clockwork.NewFakeClockAt(now)
	rt := &app.Runtime{Clock: clock}
	r, _ := newTestRequest(http.MethodGet, "/usage", nil, rt)
	from, to, ok := selfUsageWindow(r)
	require.True(t, ok)
	require.Equal(t, []time.Time{now.AddDate(0, -1, 0), now}, []time.Time{from, to}, "defaults come from the runtime clock")
	clock.Advance(24 * time.Hour)
	_, to, _ = selfUsageWindow(r)
	require.Equal(t, now.Add(24*time.Hour), to)

	r, _ = newTestRequest(http.MethodGet, "/usage?from=2030-01-01&to=2030-02-01", nil, rt)
	from, to, ok = selfUsageWindow(r)
	require.True(t, ok)
	require.Equal(t, []any{2030, time.January, time.February}, []any{from.Year(), from.Month(), to.Month()})

	for _, q := range []string{"from=junk", "to=junk", "from=2030-02-01&to=2030-01-01", "from=2030-01-01&to=2030-01-01"} {
		r, rec := newTestRequest(http.MethodGet, "/usage?"+q, nil, rt)
		_, _, ok := selfUsageWindow(r)
		require.False(t, ok, q)
		require.Equal(t, http.StatusBadRequest, rec.Code, q)
	}
}

// #335: every batch item gets its own verdict; one item's bad input, scope
// denial, deny or backend error never fails the others, and the cause of a
// backend error reaches the operator log without reaching the wire.
func TestServiceAdmitBatchIsolatesItems(t *testing.T) {
	var logs bytes.Buffer
	prevOut, prevLevel := log.StandardLogger().Out, log.GetLevel()
	log.SetOutput(&logs)
	log.SetLevel(log.ErrorLevel)
	t.Cleanup(func() { log.SetOutput(prevOut); log.SetLevel(prevLevel) })

	payer := func() openrails.CustomerID { return openrails.CustomerID(uuid.New()) }
	allowed, broke, abusive, failing, scopedOut, holdless, reused := payer(), payer(), payer(), payer(), payer(), payer(), payer()
	deadline := time.Now().Add(time.Hour)
	items := []serviceAdmitRequest{
		{CustomerID: allowed.String(), Invoker: "user:a", TrustLevel: " trusted ", EstimatedAmount: 100, ExpiresAt: &deadline, RequestID: "r1"},
		{CustomerID: broke.String(), Invoker: "user:b", EstimatedAmount: 100, ExpiresAt: &deadline, RequestID: "r2"},
		{Invoker: "user:c", RequestID: "r3"},
		{CustomerID: abusive.String(), Invoker: "user:d", RequestID: "r4"},
		{CustomerID: failing.String(), Invoker: "user:e", RequestID: "r5", Source: "host-four"},
		{CustomerID: scopedOut.String(), Invoker: "user:f", RequestID: "r6"},
		{CustomerID: allowed.String(), Invoker: "user:g", EstimatedAmount: -1, RequestID: "r7"},
		{CustomerID: holdless.String(), Invoker: "user:h", EstimatedAmount: 100, RequestID: "r8"},
		{CustomerID: reused.String(), Invoker: "user:i", RequestID: "r9"},
	}
	cause := errors.New(`ERROR: relation "openrails.billing_policy_bindings" does not exist (SQLSTATE 42P01)`)
	var seenTrust string
	admit := func(_ context.Context, in billingservice.AdmitInput) (*billingservice.AdmitResult, error) {
		switch openrails.CustomerID(in.CustomerID) {
		case allowed:
			seenTrust = in.TrustLevel
			return &billingservice.AdmitResult{Allowed: true}, nil
		case broke:
			return &billingservice.AdmitResult{BlockedBy: "money", DenyCode: "insufficient_balance"}, nil
		case abusive:
			return &billingservice.AdmitResult{BlockedBy: "abuse", RetryAfterSeconds: 7}, nil
		case holdless:
			return nil, fmt.Errorf("admit: %w", billingservice.ErrHoldDeadlineRequired)
		case reused:
			return nil, fmt.Errorf("admit: %w", billingservice.ErrIdempotencyKeyReused)
		default:
			return nil, cause
		}
	}
	allows := func(id billingidentity.CustomerID) bool { return openrails.CustomerID(id) != scopedOut }

	out := serviceAdmitBatchVerdicts(context.Background(), items, allows, admit)
	require.Len(t, out, len(items))
	status := make([]int, len(out))
	for i, v := range out {
		status[i] = v.Status
	}
	require.Equal(t, []int{200, 402, 400, 429, 500, 403, 400, 400, 409}, status)
	require.Equal(t, "trusted", seenTrust)
	require.Equal(t, "insufficient_balance", out[1].Result.DenyCode)
	require.Nil(t, out[2].Result)
	require.Equal(t, int64(7), out[3].Result.RetryAfterSeconds)
	require.Equal(t, "admission check failed", out[4].Error.Message)
	require.Equal(t, "service_credential_customer_scope_denied", out[5].Error.Message)
	require.Equal(t, "expires_at", *out[7].Error.Param)

	wire, err := json.Marshal(out)
	require.NoError(t, err)
	require.NotContains(t, string(wire), "billing_policy_bindings")
	for _, want := range []string{"admission check failed", "billing_policy_bindings", "42P01", failing.String()} {
		require.Contains(t, logs.String(), want)
	}
}

type fakeMintReader struct{ decimals uint8 }

func (r fakeMintReader) GetAccountData(context.Context, solanago.PublicKey) ([]byte, error) {
	blob := make([]byte, solanaint.MintAccountSize)
	blob[44] = r.decimals
	blob[45] = 1
	return blob, nil
}

// #352: the browser config never carries an RPC URL; #817: decimals come
// from the mint on chain, not configuration.
func TestSolanaConfigIsBrowserSafe(t *testing.T) {
	const mint = "5CVTPbcqPuzQd9bMCViire6zQVSr7TUTWTjM21aE4TZ"
	rt := &app.Runtime{
		Config:             &config.Config{},
		SolanaMintDecimals: solanamodule.NewMintDecimals(fakeMintReader{decimals: 6}),
		RailConfigs: railresolve.FixedSet{"solana": {Rail: models.RailSolana, Solana: &config.SolanaRailConfig{
			Network: "devnet", Tokens: map[string]config.TokenConfig{"USDC": {Name: "Dev USDC", Mint: mint}},
		}}},
	}
	r, rec := newTestRequest(http.MethodGet, "/v1/solana/config", nil, rt)
	GetSolanaConfig(r)
	require.Equal(t, http.StatusOK, rec.Code)
	var got SolanaRuntimeConfigResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Equal(t, []any{"devnet", "solana:devnet", "", "devnet", solanatokens.PreferredStablecoin},
		[]any{got.Network, got.Chain, got.RPCURL, got.ExplorerCluster, got.PreferredToken})
	require.True(t, got.Features.SolanaPay && got.Features.RecurringSubscriptions && got.Features.SolanaPayRecurringSubscriptions)
	require.Len(t, got.Tokens, 1)
	require.Equal(t, []any{"USDC", mint, 6, true, true},
		[]any{got.Tokens[0].Symbol, got.Tokens[0].Mint, got.Tokens[0].Decimals, got.Tokens[0].Preferred, got.Tokens[0].RecurringEligible})
}
