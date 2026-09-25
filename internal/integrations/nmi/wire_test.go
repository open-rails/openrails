package nmi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

func TestWireAmountRendersMinorUnitsAtCurrencyScale(t *testing.T) {
	for _, tc := range []struct {
		cents    moneyutil.Cents
		currency string
		want     string
	}{
		{0, "USD", "0.00"}, {1, "USD", "0.01"}, {5, "USD", "0.05"}, {99, "USD", "0.99"}, {101, "USD", "1.01"},
		{1999, "USD", "19.99"}, {123456, "EUR", "1234.56"}, {-1999, "USD", "-19.99"},
		// Zero-decimal rails keep NMI's x.xx shape: 4 yen is "4.00", never "0.04".
		{4, "JPY", "4.00"}, {0, "JPY", "0.00"}, {100, "JPY", "100.00"}, {-5, "JPY", "-5.00"},
	} {
		got, err := WireAmount(tc.cents, tc.currency)
		require.NoError(t, err)
		require.Equal(t, tc.want, got, "%d %s", tc.cents, tc.currency)
	}
	_, err := WireAmount(100, "XYZ")
	require.Error(t, err, "an unregistered currency has no scale to render")

	for _, currency := range []string{"USD", "EUR", "JPY"} {
		for _, cents := range []moneyutil.Cents{0, 1, 9, 10, 99, 100, 101, 1999, 100000, 987654321, -1, -250} {
			wire, err := WireAmount(cents, currency)
			require.NoError(t, err)
			back, ok := exactMinorAmount(wire, currency)
			require.True(t, ok, wire)
			require.EqualValues(t, cents, back, "%s round trip of %d via %q", currency, cents, wire)
			if currency != "JPY" {
				require.Equal(t, centsToDollarString(cents), wire, "enrollment and plan amounts share one rendering")
			}
		}
	}
}

func TestCentsJSONAmountRoundTrip(t *testing.T) {
	for cents, wire := range map[int64]string{0: "0.00", 1: "0.01", 99: "0.99", 100: "1.00", 1999: "19.99", 123456: "1234.56", -500: "-5.00"} {
		require.Equal(t, wire, string(centsJSONAmount(moneyutil.Cents(cents))))
		got, err := v5AmountToCents(wire)
		require.NoError(t, err)
		require.Equal(t, cents, got)
	}
	for in, want := range map[string]int64{"10.9": 1090, "5": 500, ".5": 50, " 7.25 ": 725} {
		got, err := v5AmountToCents(in)
		require.NoError(t, err, in)
		require.Equal(t, want, got, in)
	}
	for _, bad := range []string{"", "abc", "1.x"} {
		_, err := v5AmountToCents(bad)
		require.Error(t, err, bad)
	}
}

func TestExactMinorAmountRefusesInexactOrForeignShapes(t *testing.T) {
	for _, tc := range []struct {
		amount, currency string
		want             int64
		ok               bool
	}{
		{"1.00", "USD", 100, true}, {" 2.50 ", "USD", 250, true}, {"-1.50", "USD", -150, true},
		{"100.00", "JPY", 100, true}, {"100", "jpy", 100, true}, {"5.0010", "USD", 0, false},
		{"100.01", "JPY", 0, false}, {"5.001", "USD", 0, false}, {"1e2", "USD", 0, false},
		{"1.5.0", "USD", 0, false}, {"", "USD", 0, false}, {"1.00", "XYZ", 0, false},
		{"--1", "USD", 0, false}, {"99999999999999999999", "USD", 0, false},
	} {
		got, ok := exactMinorAmount(tc.amount, tc.currency)
		require.Equal(t, tc.ok, ok, "%q %s", tc.amount, tc.currency)
		require.Equal(t, tc.want, got, "%q %s", tc.amount, tc.currency)
	}
}

func TestSubscriptionAmountOverridesPlanWithoutFallback(t *testing.T) {
	plan := &V5Plan{PlanAmount: "9.99"}
	for _, tc := range []struct {
		name     string
		sub      V5Subscription
		currency string
		want     moneyutil.Cents
		ok       bool
	}{
		{"override", V5Subscription{Amount: "4.50", Plan: plan}, "USD", 450, true},
		{"plan fallback", V5Subscription{Plan: plan}, "USD", 999, true},
		{"yen", V5Subscription{Amount: "500.00"}, "JPY", 500, true},
		{"malformed override never borrows the plan", V5Subscription{Amount: "4.5x", Plan: plan}, "USD", 0, false},
		{"zero", V5Subscription{Amount: "0.00"}, "USD", 0, false},
		{"no amount", V5Subscription{}, "USD", 0, false},
		{"fractional yen", V5Subscription{Amount: "1.50"}, "JPY", 0, false},
	} {
		got, err := SubscriptionAmountMinor(tc.sub, tc.currency)
		require.Equal(t, tc.ok, err == nil, tc.name)
		require.Equal(t, tc.want, got, tc.name)
	}
	zero, err := EnrollmentEvidence{Subscription: V5Subscription{Amount: "0.00"}}.ScheduleAmountMinor("USD")
	require.NoError(t, err, "a schedule may carry a zero amount")
	require.Zero(t, zero)
	_, err = EnrollmentEvidence{Subscription: V5Subscription{Amount: "-1.00"}}.ScheduleAmountMinor("USD")
	require.ErrorIs(t, err, ErrReceiptMismatch)
}

func TestStoredCredentialPortalCombinations(t *testing.T) {
	for _, tc := range []struct {
		name string
		sc   *StoredCredential
		err  string
		form url.Values
	}{
		{"unscheduled initial CIT", &StoredCredential{InitiatedBy: InitiatedByCustomer, Indicator: IndicatorStored}, "",
			url.Values{"initiated_by": {"customer"}, "stored_credential_indicator": {"stored"}}},
		{"recurring initial CIT", &StoredCredential{InitiatedBy: InitiatedByCustomer, Indicator: IndicatorStored, Recurring: true}, "",
			url.Values{"initiated_by": {"customer"}, "stored_credential_indicator": {"stored"}, "billing_method": {"recurring"}}},
		{"unscheduled CIT reuse", &StoredCredential{InitiatedBy: InitiatedByCustomer, Indicator: IndicatorUsed, InitialTransactionID: " 42 "}, "",
			url.Values{"initiated_by": {"customer"}, "stored_credential_indicator": {"used"}, "initial_transaction_id": {"42"}}},
		{"recurring MIT", &StoredCredential{InitiatedBy: InitiatedByMerchant, Indicator: IndicatorUsed, InitialTransactionID: "42", Recurring: true}, "",
			url.Values{"initiated_by": {"merchant"}, "stored_credential_indicator": {"used"}, "initial_transaction_id": {"42"}, "billing_method": {"recurring"}}},
		{"nil", nil, "indicators are required", nil},
		{"unknown initiator", &StoredCredential{InitiatedBy: "robot", Indicator: IndicatorUsed, InitialTransactionID: "42"}, "initiated_by", nil},
		{"unknown indicator", &StoredCredential{InitiatedBy: InitiatedByCustomer, Indicator: "maybe"}, "indicator", nil},
		{"merchant cannot store", &StoredCredential{InitiatedBy: InitiatedByMerchant, Indicator: IndicatorStored}, "must be customer initiated", nil},
		{"initial carries no reference", &StoredCredential{InitiatedBy: InitiatedByCustomer, Indicator: IndicatorStored, InitialTransactionID: "stale"}, "must not carry", nil},
		{"reuse needs a reference", &StoredCredential{InitiatedBy: InitiatedByMerchant, Indicator: IndicatorUsed, InitialTransactionID: " "}, "requires initial_transaction_id", nil},
	} {
		err := tc.sc.Validate()
		if tc.err != "" {
			require.ErrorContains(t, err, tc.err, tc.name)
			continue
		}
		require.NoError(t, err, tc.name)
		form := url.Values{}
		tc.sc.ApplyToForm(form)
		require.Equal(t, tc.form, form, tc.name)
	}
	var none *StoredCredential
	form := url.Values{}
	none.ApplyToForm(form)
	require.Empty(t, form)
}

func TestParseSaleResponseTaxonomy(t *testing.T) {
	sale, err := ParseSaleResponse("response=1&response_code=100&transactionid=t1&authcode=A1&responsetext=APPROVED")
	require.NoError(t, err)
	require.Equal(t, SaleResponse{TransactionID: "t1", Authcode: "A1", ResponseText: "APPROVED"}, *sale)

	_, err = ParseSaleResponse("response=2&response_code=251&responsetext=LOST")
	var decline *CustomerVaultError
	require.ErrorAs(t, err, &decline)
	require.Equal(t, 251, decline.ResponseCode)
	require.Equal(t, "lost_card", decline.LocalizationID)
	require.False(t, RequiresVerification(err))

	for _, body := range []string{"response=3&response_code=300", "response=2&response_code=100", "", "response=1&response_code=100&transactionid=a&transactionid=b"} {
		_, err := ParseSaleResponse(body)
		require.True(t, IsTransportAmbiguous(err), "%q must require verification, got %v", body, err)
		require.False(t, errors.As(err, &decline), body)
	}
}

func TestRequiresVerificationClassifiesUncertainCodes(t *testing.T) {
	require.True(t, RequiresVerification(ambiguous(errors.New("lost"))))
	require.True(t, RequiresVerification(errors.Join(errors.New("ctx"), ambiguous(errors.New("lost")))))
	for code, want := range map[int]bool{420: true, 421: true, 430: true, 200: false, 202: false, 300: false, 410: false} {
		require.Equal(t, want, RequiresVerification(&CustomerVaultError{ResponseCode: code}), code)
	}
	require.False(t, RequiresVerification(errors.New("plain")))
	require.Nil(t, ambiguous(nil))
}

func TestCardBrandFromMaskedPAN(t *testing.T) {
	for masked, want := range map[string]string{
		"4xxxxxxxxxxx1111": "visa", "341111xxxxx0005": "amex", "37xx": "amex", "5105xxxx": "mastercard",
		"2221xxxx": "mastercard", "2720xxxx": "mastercard", "2721xxxx": "", "6011xxxx": "discover",
		"65xxxx": "discover", "644xxxx": "discover", "3530xxxx": "jcb", "36xxxx": "diners", "3005xxxx": "diners",
		"": "", "xxxx1111": "", "9xxx": "", " 4111": "visa",
	} {
		require.Equal(t, want, CardBrandFromMaskedPAN(masked), masked)
	}
}

func TestV5CursorDecodesStringNumberAndNull(t *testing.T) {
	for raw, want := range map[string]V5Cursor{`"7"`: "7", `7`: "7", `null`: "", `""`: ""} {
		var page PlanPage
		require.NoError(t, json.Unmarshal([]byte(`{"next_cursor":`+raw+`}`), &page), raw)
		require.Equal(t, want, page.NextCursor, raw)
	}
	var page PlanPage
	require.Error(t, json.Unmarshal([]byte(`{"next_cursor":{}}`), &page))
}

func TestClientConstructionBindsPostureAndEndpoints(t *testing.T) {
	sandbox, err := newClient("nmi", &config.NMIProviderSettings{SecurityKey: "k"}, true)
	require.NoError(t, err)
	require.Equal(t, []string{SandboxDirectPostURL, SandboxQueryAPIURL, SandboxV5BaseURL}, []string{sandbox.DirectPostURL, sandbox.QueryURL, sandbox.V5BaseURL})
	live, err := newClient("nmi", &config.NMIProviderSettings{SecurityKey: "k"}, false)
	require.NoError(t, err)
	require.Equal(t, []string{DefaultDirectPostURL, DefaultQueryAPIURL, DefaultV5BaseURL}, []string{live.DirectPostURL, live.QueryURL, live.V5BaseURL})
	gateway, err := newClient("nmi", &config.NMIProviderSettings{SecurityKey: "k", EndpointDeployment: config.NMIEndpointGateway}, true)
	require.NoError(t, err)
	require.Equal(t, DefaultV5BaseURL, gateway.V5BaseURL)

	_, err = newClient("nmi", &config.NMIProviderSettings{}, false)
	require.ErrorContains(t, err, "security key is required")
	_, err = newClient("nmi", &config.NMIProviderSettings{SecurityKey: "k", EndpointDeployment: config.NMIEndpointSandbox}, false)
	require.Error(t, err, "sandbox endpoint requires test posture")
	_, err = newClient("nmi", &config.NMIProviderSettings{SecurityKey: "k", EndpointDeployment: "staging"}, true)
	require.Error(t, err)

	_, err = NewAccountClient(uuid.Nil, uuid.New(), "nmi", &config.NMIProviderSettings{SecurityKey: "k"}, true)
	require.Error(t, err)
	_, err = NewAccountClient(uuid.New(), uuid.New(), "nmi", &config.NMIProviderSettings{SecurityKey: " "}, true)
	require.Error(t, err)
	merchant, psp := uuid.New(), uuid.New()
	bound, err := NewAccountClient(merchant, psp, "nmi", &config.NMIProviderSettings{SecurityKey: " k "}, true)
	require.NoError(t, err)
	gotMerchant, gotPSP := bound.AccountIdentity()
	require.Equal(t, []uuid.UUID{merchant, psp}, []uuid.UUID{gotMerchant, gotPSP})
	require.Equal(t, "k", bound.SecurityKey)
	require.Equal(t, "k", bound.accountSecurityKey, "the account-scoped key is trimmed at load too")
}

// A whitespace-padded account key must not break the native recurring
// preflight (it compares both keys) or reach the gateway untrimmed.
func TestPaddedAccountKeyIsTrimmedOnTheWire(t *testing.T) {
	f := newNMIFake(t, reply(""))
	c, err := NewAccountClient(uuid.New(), uuid.New(), "nmi", &config.NMIProviderSettings{SecurityKey: " padded-key\n"}, true)
	require.NoError(t, err)
	c.DirectPostURL, c.QueryURL, c.V5BaseURL = f.URL+"/transact", f.URL+"/query", f.URL
	c.LoopbackFixture = true
	err = c.PrepareRecurringSale(context.Background(), "vault-1", "billing-1")
	if err != nil {
		require.NotContains(t, err.Error(), "immutable account credentials")
	}
	calls := f.Calls()
	require.NotEmpty(t, calls)
	for _, call := range calls {
		if key := call.Form.Get("security_key"); key != "" {
			require.Equal(t, "padded-key", key)
		}
	}
}

// Reads get the tight bound, mutations the generous one; a shorter caller
// deadline always wins.
func TestPerRequestDeadlineBounds(t *testing.T) {
	client, err := newClient("nmi", &config.NMIProviderSettings{SecurityKey: "k"}, true)
	require.NoError(t, err)
	for mutating, want := range map[bool]time.Duration{false: nmiReadTimeout, true: nmiMutationTimeout} {
		req, cancel, err := client.newRequest(context.Background(), http.MethodPost, "http://127.0.0.1:1/x", nil, mutating)
		require.NoError(t, err)
		deadline, ok := req.Context().Deadline()
		cancel()
		require.True(t, ok)
		require.InDelta(t, want.Seconds(), time.Until(deadline).Seconds(), 1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, rcancel, err := client.newRequest(ctx, http.MethodPost, "http://127.0.0.1:1/x", nil, true)
	require.NoError(t, err)
	defer rcancel()
	deadline, _ := req.Context().Deadline()
	require.LessOrEqual(t, time.Until(deadline), 2*time.Second)
}
