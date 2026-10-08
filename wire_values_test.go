package openrails

import (
	"crypto/sha256"
	"encoding/json"
	"math"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/modules/checkoutsession"
)

func requireFixture(t *testing.T, name string, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	require.NoError(t, err)
	fixture, err := os.ReadFile("testdata/wire/" + name)
	require.NoError(t, err)
	require.Equal(t, strings.TrimSpace(string(fixture)), string(raw), "%s drifted from the Go contract", name)
	return raw
}

func TestCurrencyRegistry(t *testing.T) {
	requireFixture(t, "currencies.json", billing.CurrencyRegistry{Currencies: billing.Currencies()})
	usd, ok := billing.LookupCurrency(" usd ")
	require.True(t, ok)
	require.Equal(t, billing.CurrencyUnits{Code: "USD", Decimals: 6, MinorDecimals: 2}, usd)
	require.Equal(t, 4, usd.NativeShift())
	jpy, _ := billing.LookupCurrency("JPY")
	require.Equal(t, 4, jpy.NativeShift(), "zero-decimal rails still scale into native units")
	_, ok = billing.LookupCurrency("XYZ")
	require.False(t, ok, "a scale is never guessed")
}

func TestCheckoutSessionPlanStampsRegistryScale(t *testing.T) {
	hours := 720
	plan, err := checkoutsession.NewPlan("Premium", math.MaxInt64, "jpy", &hours, &hours)
	require.NoError(t, err)
	require.Equal(t, checkoutsession.CheckoutSessionPlan{DisplayName: "Premium", UnitAmount: math.MaxInt64, Currency: "JPY", UnitDecimals: 4, BillingIntervalHours: &hours, AccessDurationHours: &hours}, plan)
	raw, err := json.Marshal(plan)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"unit_amount":"9223372036854775807"`)
	_, err = checkoutsession.NewPlan("Premium", 1, "XYZ", nil, nil)
	require.ErrorIs(t, err, billing.ErrInvalid, "a scale is never guessed")
	// OpenRails advertises the browser driver per option (#1078); an option
	// no browser can drive carries none.
	psp := billing.PSPID(uuid.MustParse("0198f3a4-6f1e-7c2b-9d4e-1f2a3b4c5d6e"))
	option := billing.CheckoutOption{PSP: "solana", PSPID: psp, Rail: "solana", Mode: "subscription", Driver: "solana_pay", PublicConfig: map[string]string{"token_symbol": "DUSD"}}
	raw, err = json.Marshal(option)
	require.NoError(t, err)
	require.JSONEq(t, `{"psp":"solana","psp_id":"psp_0198f3a4-6f1e-7c2b-9d4e-1f2a3b4c5d6e","rail":"solana","mode":"subscription","driver":"solana_pay","public_config":{"token_symbol":"DUSD"}}`, string(raw))
	raw, err = json.Marshal(billing.CheckoutOption{PSP: "stripe", PSPID: psp, Rail: "stripe", Mode: "subscription"})
	require.NoError(t, err)
	require.NotContains(t, string(raw), "driver")
}

func TestInvoiceMoneyWireIsLossless(t *testing.T) {
	for _, amount := range []int64{math.MinInt64, -9007199254740993, 0, 9007199254740993, math.MaxInt64} {
		invoice := billing.Invoice{AmountDue: amount, TotalAmount: amount, MoneyMovements: billing.AmountMap{"deposit": amount}, LineItems: []billing.InvoiceLineItem{{Amount: amount}}}
		raw, err := json.Marshal(invoice)
		require.NoError(t, err)
		var browser map[string]any
		require.NoError(t, json.Unmarshal(raw, &browser))
		require.IsType(t, "", browser["amount_due"])
		var read billing.Invoice
		require.NoError(t, json.Unmarshal(raw, &read))
		require.Equal(t, invoice.AmountDue, read.AmountDue)
		require.Equal(t, invoice.TotalAmount, read.TotalAmount)
		require.Equal(t, invoice.MoneyMovements, read.MoneyMovements)
		require.Equal(t, amount, read.LineItems[0].Amount)
	}
	for _, raw := range []string{`{"deposit":9007199254740993}`, `{"deposit":"9223372036854775808"}`, `{"deposit":"1.5"}`} {
		var out billing.AmountMap
		require.Error(t, json.Unmarshal([]byte(raw), &out), raw)
	}
}

func TestProviderBillingQualificationWireContract(t *testing.T) {
	when := time.Date(2026, 9, 16, 12, 0, 0, 123456000, time.UTC)
	cost, rated := int64(math.MaxInt64), int64(math.MaxInt64)
	body := []byte(`{"contract":"openrails/pass-through-provider-cost"}`)
	digest := billing.SHA256(sha256.Sum256(body))
	merchantID := billing.MerchantID(uuid.MustParse("11111111-1111-1111-1111-111111111111"))
	value := billing.ProviderBillingQualification{
		OperationID: "rental/create", MerchantID: merchantID,
		Lifecycle: billing.ProviderBillingLifecycleEvidence{
			Provider: "runpod", ProviderResourceID: "pod-1",
			ProviderLifetimeStartsAt: when.Add(-2 * time.Hour), ProviderLifetimeEndsAt: when.Add(-time.Hour),
			ProviderAbsentAt: when, ProviderAbsenceReference: "absence:1", BillingStopReference: "stop:1",
			WindowsClosedAt: when, WindowsClosedReference: "windows:1", LifecycleEvidenceBody: []byte(`{}`),
		},
		LifecycleEvidenceSHA256: billing.SHA256(sha256.Sum256([]byte(`{}`))),
		QuiescenceSeconds:       86400,
		State:                   billing.ProviderBillingQualificationEligible,
		Reason:                  billing.ProviderBillingEligible,
		BaselineObservationID:   "obs-1", QualifiedObservationID: "obs-2",
		QualifiedCostAmount: &cost,
		QualifiedAt:         &when,
		Authorization: billing.OperationAuthorization{
			OperationID: "rental/create", MerchantID: merchantID,
			CustomerID: billing.CustomerID(uuid.MustParse("22222222-2222-2222-2222-222222222222")), RecordOwner: "user:1",
			Currency: "USD", Amount: math.MaxInt64, ClaimReference: "claim:1", AuthorizationBody: []byte(`{"op":1}`),
			AuthorizationBodySHA256: billing.SHA256(sha256.Sum256([]byte(`{"op":1}`))), State: billing.OperationAuthorizationSettled,
			TerminalReference: "sha256:" + digest.String(), SettlementCostAmount: &cost,
			SettlementAmount: &rated, SettlementBody: body, SettlementBodySHA256: &digest,
			CreatedAt: when, SettledAt: &when,
		},
		CreatedAt: when, UpdatedAt: when,
	}
	raw := requireFixture(t, "provider_billing_qualification.json", value)
	var got billing.ProviderBillingQualification
	require.NoError(t, json.Unmarshal(raw, &got))
	require.True(t, reflect.DeepEqual(value, got), "qualification lost precision or null semantics: %#v", got)

	open := billing.OperationAuthorization{State: billing.OperationAuthorizationOpen, CreatedAt: when}
	raw, err := json.Marshal(open)
	require.NoError(t, err)
	for _, field := range []string{`"settlement_amount":null`, `"settlement_body":null`, `"settlement_body_sha256":null`} {
		require.Contains(t, string(raw), field, "unsettled authorization must encode explicit nulls")
	}
	var openGot billing.OperationAuthorization
	require.NoError(t, json.Unmarshal(raw, &openGot))
	require.True(t, reflect.DeepEqual(open, openGot))

	var d billing.SHA256
	valid := strings.Repeat("ab", sha256.Size)
	require.NoError(t, d.UnmarshalText([]byte(valid)))
	require.Equal(t, valid, d.String())
	for _, invalid := range []string{strings.ToUpper(valid), valid[:62], valid + "00", strings.Repeat("zz", sha256.Size), ""} {
		require.Error(t, d.UnmarshalText([]byte(invalid)), invalid)
	}
}

// Provider obligation commands carry facts; the engine rates and settles.
func TestProviderObligationRequestsCarryNoRatedAmount(t *testing.T) {
	pkg := reflect.TypeFor[Client]().PkgPath()
	var walk func(reflect.Type, string)
	walk = func(typ reflect.Type, path string) {
		for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice {
			typ = typ.Elem()
		}
		if typ.Kind() != reflect.Struct || typ.PkgPath() != pkg {
			return
		}
		for i := range typ.NumField() {
			field := typ.Field(i)
			name := strings.ToLower(field.Name + " " + field.Tag.Get("json"))
			for _, banned := range []string{"rated", "settle", "charge"} {
				if strings.Contains(name, banned) {
					t.Errorf("%s.%s lets a caller supply a settlement amount", path, field.Name)
				}
			}
			walk(field.Type, path+"."+field.Name)
		}
	}
	for _, request := range []any{billing.OpenOperationAuthorizationParams{}, billing.ReleaseOperationAuthorizationParams{}, billing.RecordProviderBillingObservationParams{}} {
		walk(reflect.TypeOf(request), reflect.TypeOf(request).Name())
	}
}

func TestProviderOperationPathIsOneSegment(t *testing.T) {
	path, err := providerOperationPath("rental/1?#%/create")
	require.NoError(t, err)
	require.Equal(t, "/v1/merchant/provider-operations/rental%2F1%3F%23%25%2Fcreate", path)
	for _, id := range []string{"", " ", ".", "..", " x"} {
		_, err := providerOperationPath(id)
		require.ErrorIs(t, err, billing.ErrInvalid, "%q", id)
	}
}

func TestMerchantConfigurationDocument(t *testing.T) {
	valid := "application_id: initial\nexpected_revision: revision\ndisplay_name: Shop\nsettings:\n  profile:\n    support_url: https://help.example.test\n"
	params, err := billing.ParseMerchantConfigurationYAML([]byte(valid))
	require.NoError(t, err)
	require.Equal(t, "https://help.example.test", params.Settings.Profile.SupportURL)
	for _, document := range []string{
		valid + "display_name: Duplicate\n",
		valid + "unexpected: true\n",
		valid + "other: &anchor value\n",
		valid + "---\napplication_id: extra\n",
		"application_id: missing-revision\n",
		strings.Repeat(" ", billing.MaxMerchantConfigurationBytes+1),
		`{"application_id":"a","application_id":"b","expected_revision":"r"}`,
		`{"application_id":"a","expected_revision":"r","settings":{"profile":{"unknown":1}}}`,
	} {
		_, err := billing.ParseMerchantConfigurationYAML([]byte(document))
		require.Error(t, err, document)
	}
}

// Explicit empty lists mean "clear"; absent lists mean "unchanged".
func TestMerchantConfigurationEmptyListsSurviveTransport(t *testing.T) {
	revision, amount := "before", int64(9007199254740993)
	params := billing.ApplyMerchantConfigurationParams{ApplicationID: "clear", ExpectedRevision: &revision, Settings: &billing.MerchantSettings{
		InvoiceCollectionThreshold: &amount,
		BillingPolicies:            []billing.BillingPolicy{}, BillingPolicyBindings: []billing.BillingPolicyBinding{}, DelegatedInvokerWastedSpendLimits: []billing.BudgetWindow{},
	}}
	body, err := json.Marshal(params)
	require.NoError(t, err)
	require.Contains(t, string(body), `"collection_threshold":"9007199254740993"`)
	decoded, err := billing.ParseMerchantConfigurationYAML(body)
	require.NoError(t, err)
	require.Equal(t, amount, *decoded.Settings.InvoiceCollectionThreshold)
	require.NotNil(t, decoded.Settings.BillingPolicies)
	require.NotNil(t, decoded.Settings.BillingPolicyBindings)
	require.NotNil(t, decoded.Settings.DelegatedInvokerWastedSpendLimits)

	params.Settings = &billing.MerchantSettings{}
	body, err = json.Marshal(params)
	require.NoError(t, err)
	decoded, err = billing.ParseMerchantConfigurationYAML(body)
	require.NoError(t, err)
	require.Nil(t, decoded.Settings.BillingPolicies)
}
