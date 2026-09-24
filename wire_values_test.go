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
	requireFixture(t, "currencies.json", CurrencyRegistry{Object: "currencies", Currencies: Currencies()})
	usd, ok := LookupCurrency(" usd ")
	require.True(t, ok)
	require.Equal(t, CurrencyUnits{Code: "USD", Decimals: 6, MinorDecimals: 2}, usd)
	require.Equal(t, 4, usd.NativeShift())
	jpy, _ := LookupCurrency("JPY")
	require.Equal(t, 4, jpy.NativeShift(), "zero-decimal rails still scale into native units")
	_, ok = LookupCurrency("XYZ")
	require.False(t, ok, "a scale is never guessed")
}

func TestHostedCheckoutPlanStampsRegistryScale(t *testing.T) {
	hours := 720
	product := &Product{ID: ProductID(uuid.New()).String(), DisplayName: "Premium"}
	price := &Price{ID: PriceID(uuid.New()).String(), UnitAmount: math.MaxInt64, Currency: "jpy", AccessDurationHours: &hours, AutoRenew: true}
	plan, err := NewHostedCheckoutPlan(product, price)
	require.NoError(t, err)
	require.Equal(t, HostedCheckoutPlan{DisplayName: "Premium", UnitAmount: math.MaxInt64, Currency: "JPY", UnitDecimals: 4, PeriodHours: &hours, AutomaticallyRenews: true}, plan)
	raw, err := json.Marshal(plan)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"unit_amount":"9223372036854775807"`)
	for _, bad := range []struct {
		product *Product
		price   *Price
	}{{nil, price}, {product, nil}, {product, &Price{Currency: "XYZ"}}} {
		_, err := NewHostedCheckoutPlan(bad.product, bad.price)
		require.ErrorIs(t, err, ErrInvalid)
	}
	for rail, want := range map[string]string{"nmi": "collect_js", " NMI ": "collect_js", "stripe": "redirect", "ccbill": "redirect", "solana": "solana_pay"} {
		got, ok := HostedCheckoutDriver(rail)
		require.True(t, ok, rail)
		require.Equal(t, want, got, rail)
	}
	_, ok := HostedCheckoutDriver("basis_theory")
	require.False(t, ok, "a rail the browser cannot execute is never offered")
}

func TestInvoiceMoneyWireIsLossless(t *testing.T) {
	for _, amount := range []int64{math.MinInt64, -9007199254740993, 0, 9007199254740993, math.MaxInt64} {
		invoice := InvoiceDTO{AmountDue: amount, TotalAmount: amount, MoneyMovements: AmountMap{"deposit": amount}, LineItems: []InvoiceLineItemDTO{{Amount: amount}}}
		raw, err := json.Marshal(invoice)
		require.NoError(t, err)
		var browser map[string]any
		require.NoError(t, json.Unmarshal(raw, &browser))
		require.IsType(t, "", browser["amount_due"])
		var read InvoiceDTO
		require.NoError(t, json.Unmarshal(raw, &read))
		require.Equal(t, invoice.AmountDue, read.AmountDue)
		require.Equal(t, invoice.TotalAmount, read.TotalAmount)
		require.Equal(t, invoice.MoneyMovements, read.MoneyMovements)
		require.Equal(t, amount, read.LineItems[0].Amount)
	}
	for _, raw := range []string{`{"deposit":9007199254740993}`, `{"deposit":"9223372036854775808"}`, `{"deposit":"1.5"}`} {
		var out AmountMap
		require.Error(t, json.Unmarshal([]byte(raw), &out), raw)
	}
}

func TestProviderBillingQualificationWireContract(t *testing.T) {
	when := time.Date(2026, 9, 16, 12, 0, 0, 123456000, time.UTC)
	cost, rated := int64(math.MaxInt64), int64(math.MaxInt64)
	body := []byte(`{"contract":"openrails/pass-through-provider-cost"}`)
	digest := SHA256(sha256.Sum256(body))
	merchantID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	value := ProviderBillingQualification{
		OperationID: "rental/create", MerchantID: merchantID,
		Lifecycle: ProviderBillingLifecycleEvidence{
			Provider: "runpod", ProviderResourceID: "pod-1",
			ProviderLifetimeStart: when.Add(-2 * time.Hour), ProviderLifetimeEnd: when.Add(-time.Hour),
			ProviderAbsentAt: when, ProviderAbsenceReference: "absence:1", BillingStopReference: "stop:1",
			WindowsClosedAt: when, WindowsClosedReference: "windows:1", LifecycleEvidenceBody: []byte(`{}`),
		},
		LifecycleEvidenceSHA256: SHA256(sha256.Sum256([]byte(`{}`))),
		QuiescenceSeconds:       86400,
		State:                   ProviderBillingQualificationEligible,
		Reason:                  ProviderBillingEligible,
		BaselineObservationID:   "obs-1", QualifiedObservationID: "obs-2",
		QualifiedProviderCostUSDMicros: &cost,
		QualifiedAt:                    &when,
		Authorization: OperationAuthorization{
			OperationID: "rental/create", MerchantID: merchantID,
			Payer: CustomerID(uuid.MustParse("22222222-2222-2222-2222-222222222222")), RecordOwner: "user:1",
			AuthorizedUSDMicros: math.MaxInt64, ClaimReference: "claim:1", AuthorizationBody: []byte(`{"op":1}`),
			AuthorizationBodySHA256: SHA256(sha256.Sum256([]byte(`{"op":1}`))), State: OperationAuthorizationSettled,
			TerminalReference: "sha256:" + digest.String(), SettlementProviderCostUSDMicros: &cost,
			SettlementRatedUSDMicros: &rated, SettlementBody: body, SettlementBodySHA256: &digest,
			CreatedAt: when, SettledAt: &when,
		},
		CreatedAt: when, UpdatedAt: when,
	}
	raw := requireFixture(t, "provider_billing_qualification.json", value)
	var got ProviderBillingQualification
	require.NoError(t, json.Unmarshal(raw, &got))
	require.True(t, reflect.DeepEqual(value, got), "qualification lost precision or null semantics: %#v", got)

	open := OperationAuthorization{State: OperationAuthorizationOpen, CreatedAt: when}
	raw, err := json.Marshal(open)
	require.NoError(t, err)
	for _, field := range []string{`"settlement_rated_usd_micros":null`, `"settlement_body":null`, `"settlement_body_sha256":null`} {
		require.Contains(t, string(raw), field, "unsettled authorization must encode explicit nulls")
	}
	var openGot OperationAuthorization
	require.NoError(t, json.Unmarshal(raw, &openGot))
	require.True(t, reflect.DeepEqual(open, openGot))

	var d SHA256
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
	for _, request := range []any{OperationAuthorizationRequest{}, ReleaseOperationAuthorizationRequest{}, ProviderBillingObservationRequest{}} {
		walk(reflect.TypeOf(request), reflect.TypeOf(request).Name())
	}
}

func TestProviderOperationPathIsOneSegment(t *testing.T) {
	path, err := providerOperationPath("rental/1?#%/create")
	require.NoError(t, err)
	require.Equal(t, "/v1/merchant/provider-operations/rental%2F1%3F%23%25%2Fcreate", path)
	for _, id := range []string{"", " ", ".", "..", " x"} {
		_, err := providerOperationPath(id)
		require.ErrorIs(t, err, ErrInvalid, "%q", id)
	}
}

func TestMerchantConfigurationDocument(t *testing.T) {
	valid := "application_id: initial\nexpected_revision: revision\ndisplay_name: Shop\nsettings:\n  profile:\n    support_url: https://help.example.test\n"
	params, err := ParseMerchantConfigurationYAML([]byte(valid))
	require.NoError(t, err)
	require.Equal(t, "https://help.example.test", params.Settings.Profile.SupportURL)
	for _, document := range []string{
		valid + "display_name: Duplicate\n",
		valid + "unexpected: true\n",
		valid + "other: &anchor value\n",
		valid + "---\napplication_id: extra\n",
		"application_id: missing-revision\n",
		strings.Repeat(" ", MaxMerchantConfigurationBytes+1),
		`{"application_id":"a","application_id":"b","expected_revision":"r"}`,
		`{"application_id":"a","expected_revision":"r","settings":{"profile":{"unknown":1}}}`,
	} {
		_, err := ParseMerchantConfigurationYAML([]byte(document))
		require.Error(t, err, document)
	}
	_, err = ParseMerchantConfigurationJSON([]byte(`{"application_id":"a","application_id":"b","expected_revision":"r"}`))
	require.Error(t, err)
}

// Explicit empty lists mean "clear"; absent lists mean "unchanged".
func TestMerchantConfigurationEmptyListsSurviveTransport(t *testing.T) {
	revision, amount := "before", int64(9007199254740993)
	params := MerchantConfigurationApplyParams{ApplicationID: "clear", ExpectedRevision: &revision, Settings: &MerchantSettings{
		InvoiceCollectionThreshold: &amount,
		BillingPolicies:            []BillingPolicyInput{}, BillingPolicyBindings: []BillingPolicyBindingInput{}, DelegatedInvokerWastedSpendLimits: []BudgetWindowInput{},
	}}
	body, err := json.Marshal(params)
	require.NoError(t, err)
	require.Contains(t, string(body), `"collection_threshold":"9007199254740993"`)
	decoded, err := ParseMerchantConfigurationJSON(body)
	require.NoError(t, err)
	require.Equal(t, amount, *decoded.Settings.InvoiceCollectionThreshold)
	require.NotNil(t, decoded.Settings.BillingPolicies)
	require.NotNil(t, decoded.Settings.BillingPolicyBindings)
	require.NotNil(t, decoded.Settings.DelegatedInvokerWastedSpendLimits)

	params.Settings = &MerchantSettings{}
	body, err = json.Marshal(params)
	require.NoError(t, err)
	decoded, err = ParseMerchantConfigurationJSON(body)
	require.NoError(t, err)
	require.Nil(t, decoded.Settings.BillingPolicies)
}
