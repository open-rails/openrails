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

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/http/middleware"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	solanamodule "github.com/open-rails/openrails/internal/modules/solana"
	solanatokens "github.com/open-rails/openrails/internal/modules/solana/tokens"
	"github.com/open-rails/openrails/internal/railresolve"
	billingservice "github.com/open-rails/openrails/internal/service"
)

// A checkout attempt sells a price: an operation selector is an unknown
// field, and a malformed saved method is refused before any service runs.
func TestPricedCheckoutRejectsBeforeEngine(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{`{"mode":"one_off"}`, billing.CodeUnknownField},
		{`{"subscription_id":"x"}`, billing.CodeUnknownField},
		{`{"new_price_id":"x"}`, billing.CodeUnknownField},
		{`{"payment":{"card":{"number":"4111111111111111"}}}`, billing.CodeUnknownField},
		{`{"payment":{"payment_method_id":"550e8400-e29b-41d4-a716-446655440000"}}`, "invalid payment_method_id"},
		{`{"payment":{"payment_method_id":"price_550e8400-e29b-41d4-a716-446655440000"}}`, "invalid payment_method_id"},
		{`{"payment":{"payment_method_id":"pm_00000000-0000-0000-0000-000000000000"}}`, "invalid payment_method_id"},
	} {
		r, rec := newTestRequest(http.MethodPost, "/v1/merchant/checkout-attempts", strings.NewReader(tc.body), nil)
		ServiceCreateCheckoutAttempt(r)
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
			payer, mid := uuid.NewString(), billing.MerchantID(uuid.New())
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
			// The random request id may contain "4111"; assert the card number.
			require.NotContains(t, rec.Body.String(), "4111111111111111")
			require.NotContains(t, rec.Body.String(), "4111 1111")
		}
	}
}

// A raw card number reaches OpenRails only in the card field; billing details
// round trip into the stored metadata and back out as one shape.
func TestPaymentMethodRequestMapping(t *testing.T) {
	for _, field := range []string{"card_number", "number", "pan", "cvv", "cvc", "provider", "last_four", "card_type", "expiry_date", "name_on_card", "zip"} {
		err := httprequest.DecodeStrict([]byte(`{"payment_token":"tok","`+field+`":"4111111111111111"}`), &billing.CreatePaymentMethodParams{})
		require.Equal(t, billing.CodeUnknownField, httprequest.BindError(err).Code, field)
		err = httprequest.DecodeStrict([]byte(`{"payment_token":"tok","`+field+`":"x"}`), &billing.ReplacePaymentMethodCardParams{})
		require.Equal(t, billing.CodeUnknownField, httprequest.BindError(err).Code, field)
	}
	for _, tc := range []struct{ country, zip string }{{"US", "80202"}, {"DE", "10115"}, {"JP", "100-0001"}, {"BR", "01001-000"}} {
		name, phone := "Ada Lovelace", "+49 30 123456"
		in := billingDetailsInput(&billing.BillingDetails{Name: &name, Phone: &phone, Address: &billing.BillingAddress{Country: &tc.country, PostalCode: &tc.zip}})
		metadata := in.metadata()
		for key, want := range map[string]any{"name_on_card": "Ada Lovelace", "billing_country": tc.country, "postal_code": tc.zip, "billing_phone": phone} {
			require.Equal(t, want, metadata[key], key)
		}
		out := billingDetailsFromMetadata(metadata)
		require.Equal(t, []any{name, phone, tc.country, tc.zip}, []any{*out.Name, *out.Phone, *out.Address.Country, *out.Address.PostalCode})
		require.Nil(t, out.Email)
	}
	require.Nil(t, billingDetailsFromMetadata(map[string]any{"e2e_run_id": "x"}), "no billing details is null")

	for _, alias := range []string{"first_name", "last_name"} {
		err := httprequest.DecodeStrict([]byte(`{"price_key":"p","payment":{"`+alias+`":"x"}}`), &billing.CreateCheckoutAttemptRequest{})
		require.Equal(t, billing.CodeUnknownField, httprequest.BindError(err).Code, alias)
	}
	out := PaymentMethodToAPI(&models.PaymentMethod{Rail: "nmi", Card: models.Card{Brand: "visa", Last4: "4242", ExpMonth: 12, ExpYear: 2099}}, nil, nil, time.Now())
	require.Equal(t, []any{"visa", "4242", 12, 2099}, []any{*out.Card.Brand, *out.Card.Last4, *out.Card.ExpMonth, *out.Card.ExpYear})
	require.Nil(t, out.PSPID, "a method with no PSP names none")
	require.Equal(t, []string{}, out.CollectionCurrencies)
}

// #589: a method is active unless its card expired or its last charge failed;
// a card is valid through the end of its expiry month.
func TestPaymentMethodHealth(t *testing.T) {
	now := time.Date(2026, 6, 24, 0, 0, 0, 0, time.UTC)
	status := func(c models.Card) *billing.CardExpiryStatus { return paymentMethodHealth(c, nil, now).ExpiryStatus }
	require.Nil(t, status(models.Card{}))
	require.Equal(t, billing.CardExpiryExpired, *status(models.Card{ExpMonth: 1, ExpYear: 2020}))
	require.Equal(t, billing.CardExpiryExpired, *status(models.Card{ExpMonth: 5, ExpYear: 2026}))
	require.Equal(t, billing.CardExpiryExpiringSoon, *status(models.Card{ExpMonth: 6, ExpYear: 2026}), "valid through the end of its month")
	require.Equal(t, billing.CardExpiryValid, *status(models.Card{ExpMonth: 12, ExpYear: 2099}))

	valid := models.Card{ExpMonth: 12, ExpYear: 2099}
	for _, tc := range []struct {
		card    models.Card
		charge  string
		outcome billing.ChargeOutcome
		active  bool
	}{
		{valid, "", "", true},
		{valid, "completed", billing.ChargeSucceeded, true},
		{valid, "failed", billing.ChargeFailed, false},
		{models.Card{ExpMonth: 1, ExpYear: 2020}, "completed", billing.ChargeSucceeded, false},
	} {
		var charge *models.PaymentMethodCharge
		if tc.charge != "" {
			charge = &models.PaymentMethodCharge{LastChargedAt: now, Status: tc.charge}
		}
		h := paymentMethodHealth(tc.card, charge, now)
		require.Equal(t, tc.active, h.Active, "%+v", tc)
		require.Equal(t, charge != nil, h.LastChargedAt != nil)
		if charge != nil {
			require.Equal(t, tc.outcome, *h.LastChargeOutcome)
		}
	}
}

func TestUsageWindow(t *testing.T) {
	now := time.Date(2040, 5, 15, 12, 30, 0, 0, time.UTC)
	clock := clockwork.NewFakeClockAt(now)
	rt := &app.Runtime{Clock: clock}
	r, _ := newTestRequest(http.MethodGet, "/usage", nil, rt)
	from, to, ok := usageWindow(r)
	require.True(t, ok)
	require.Equal(t, []time.Time{now.AddDate(0, -1, 0), now}, []time.Time{from, to}, "defaults come from the runtime clock")
	clock.Advance(24 * time.Hour)
	_, to, _ = usageWindow(r)
	require.Equal(t, now.Add(24*time.Hour), to)

	r, _ = newTestRequest(http.MethodGet, "/usage?from=2030-01-01&to=2030-02-01", nil, rt)
	from, to, ok = usageWindow(r)
	require.True(t, ok)
	require.Equal(t, []any{2030, time.January, time.February}, []any{from.Year(), from.Month(), to.Month()})

	for _, q := range []string{"from=junk", "to=junk", "from=2030-02-01&to=2030-01-01", "from=2030-01-01&to=2030-01-01"} {
		r, rec := newTestRequest(http.MethodGet, "/usage?"+q, nil, rt)
		_, _, ok := usageWindow(r)
		require.False(t, ok, q)
		require.Equal(t, http.StatusBadRequest, rec.Code, q)
	}
}

// #335: every batch item gets its own verdict; one item's bad input, scope
// denial, deny or backend error never fails the others, and the cause of a
// backend error reaches the operator log without reaching the wire.
func TestAdmitVerdictsIsolateItems(t *testing.T) {
	var logs bytes.Buffer
	prevOut, prevLevel := log.StandardLogger().Out, log.GetLevel()
	log.SetOutput(&logs)
	log.SetLevel(log.ErrorLevel)
	t.Cleanup(func() { log.SetOutput(prevOut); log.SetLevel(prevLevel) })

	payer := func() billing.CustomerID { return billing.CustomerID(uuid.New()) }
	allowed, broke, abusive, failing, scopedOut, holdless, reused := payer(), payer(), payer(), payer(), payer(), payer(), payer()
	deadline := time.Now().Add(time.Hour)
	items := []billing.AdmitParams{
		{CustomerID: allowed, Invoker: "user:a", TrustLevel: " trusted ", EstimatedAmount: 100, ExpiresAt: &deadline, RequestID: "r1"},
		{CustomerID: broke, Invoker: "user:b", EstimatedAmount: 100, ExpiresAt: &deadline, RequestID: "r2"},
		{Invoker: "user:c", RequestID: "r3"},
		{CustomerID: abusive, Invoker: "user:d", RequestID: "r4"},
		{CustomerID: failing, Invoker: "user:e", RequestID: "r5", Source: "host-four"},
		{CustomerID: scopedOut, Invoker: "user:f", RequestID: "r6"},
		{CustomerID: allowed, Invoker: "user:g", EstimatedAmount: -1, RequestID: "r7"},
		{CustomerID: holdless, Invoker: "user:h", EstimatedAmount: 100, RequestID: "r8"},
		{CustomerID: reused, Invoker: "user:i", RequestID: "r9"},
	}
	cause := errors.New(`ERROR: relation "billing.billing_policy_bindings" does not exist (SQLSTATE 42P01)`)
	var seenTrust string
	blocked := func(by billing.AdmissionBlock, code string, retry int64) *billing.Admission {
		out := &billing.Admission{BlockedBy: &by, DenyCode: &code}
		if retry > 0 {
			out.RetryAfterSeconds = &retry
		}
		return out
	}
	admit := func(_ context.Context, in billingservice.AdmitInput) (*billing.Admission, error) {
		switch in.CustomerID {
		case allowed:
			seenTrust = in.TrustLevel
			return &billing.Admission{Allowed: true}, nil
		case broke:
			return blocked(billing.AdmissionBlockedByMoney, "insufficient_balance", 0), nil
		case abusive:
			return blocked(billing.AdmissionBlockedByAbuse, "failure_rate_limited", 7), nil
		case holdless:
			return nil, fmt.Errorf("admit: %w", billingservice.ErrHoldDeadlineRequired)
		case reused:
			return nil, fmt.Errorf("admit: %w", billingservice.ErrIdempotencyKeyReused)
		default:
			return nil, cause
		}
	}
	allows := func(id billing.CustomerID) bool { return id != scopedOut }

	out := admitVerdicts(context.Background(), items, allows, admit)
	require.Len(t, out, len(items))
	status := make([]int, len(out))
	for i, v := range out {
		status[i] = v.Status
	}
	require.Equal(t, []int{200, 402, 400, 429, 500, 403, 400, 400, 409}, status)
	require.Equal(t, "trusted", seenTrust)
	require.Equal(t, "insufficient_balance", *out[1].Admission.DenyCode)
	require.Nil(t, out[2].Admission)
	require.Equal(t, int64(7), *out[3].Admission.RetryAfterSeconds)
	require.Equal(t, "admission check failed", out[4].Error.Message)
	require.Equal(t, "service_credential_customer_scope_denied", out[5].Error.Code)
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
	r, _ := newTestRequest(http.MethodGet, "/v1/checkout-config", nil, rt)
	got, err := solanaCheckoutConfig(r)
	require.NoError(t, err)
	require.Equal(t, []any{"devnet", "solana:devnet", solanatokens.PreferredStablecoin}, []any{got.Network, got.Chain, got.PreferredToken})
	require.Len(t, got.Tokens, 1)
	require.Equal(t, []any{"USDC", mint, 6, true, true},
		[]any{got.Tokens[0].Symbol, got.Tokens[0].Mint, got.Tokens[0].Decimals, got.Tokens[0].Preferred, got.Tokens[0].RecurringEligible})
	raw, err := json.Marshal(got)
	require.NoError(t, err)
	require.NotContains(t, strings.ToLower(string(raw)), "rpc")
}
