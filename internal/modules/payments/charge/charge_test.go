package charge

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
)

func TestEngineExecutionBindings(t *testing.T) {
	t.Parallel()
	provider := FrozenInstrument{PSPID: uuid.New(), Custodian: models.CustodianPSP, RailCustomerRef: "customer", RailMethodRef: "method", Mandate: &Mandate{ID: uuid.New(), Kind: AgreementRecurring, InitialTransactionID: "initial-payment"}}
	custodyID := uuid.New()
	proxied := provider
	proxied.Custodian, proxied.CustodianID = models.CustodianHyperSwitch, &custodyID
	binding := &HyperSwitchBinding{AccountID: "account", ProfileID: "profile", APIBaseURL: "https://vault.example"}

	for _, tc := range []struct {
		name, rail string
		instrument FrozenInstrument
		binding    *HyperSwitchBinding
	}{
		{"native_nmi", "nmi", provider, nil},
		{"stripe", "stripe", provider, nil},
		{"proxied_nmi", "nmi", proxied, binding},
	} {
		require.NoError(t, ValidateEngineInstrument(tc.rail, tc.instrument, tc.binding), tc.name)
		require.NoError(t, ValidateRenewalMandate(tc.instrument), tc.name)
		noAgreement := tc.instrument
		noAgreement.Mandate = nil
		require.ErrorIs(t, ValidateRenewalMandate(noAgreement), ErrNoRecurringMandate, "%s: renewal without its mandate", tc.name)
		require.NoError(t, ValidateEngineInstrument(tc.rail, noAgreement, tc.binding), "%s: the initial payment establishes it", tc.name)
		onFile := tc.instrument
		onFile.Mandate = &Mandate{ID: uuid.New(), Kind: AgreementCardOnFile, InitialTransactionID: "save"}
		require.ErrorIs(t, ValidateRenewalMandate(onFile), ErrNoRecurringMandate, "%s: card-on-file consent never renews", tc.name)
		noMethod := tc.instrument
		noMethod.RailMethodRef = " "
		require.Error(t, ValidateEngineInstrument(tc.rail, noMethod, tc.binding), "%s: provider default method is never charged", tc.name)
	}

	basisID := uuid.New()
	basis := provider
	basis.Custodian, basis.CustodianID = models.CustodianBasisTheory, &basisID
	badBinding := *binding
	badBinding.APIBaseURL = "https://vault.example/"
	for _, tc := range []struct {
		name, rail string
		instrument FrozenInstrument
		binding    *HyperSwitchBinding
	}{
		{"ccbill_has_no_engine_adapter", "ccbill", provider, nil},
		{"stripe_cannot_use_nmi_proxy", "stripe", proxied, binding},
		{"native_cannot_carry_proxy_binding", "nmi", provider, binding},
		{"proxy_requires_frozen_profile", "nmi", proxied, nil},
		{"proxy_binding_must_be_canonical", "nmi", proxied, &badBinding},
		{"basis_theory_has_no_engine_transport", "nmi", basis, nil},
		{"no_provider_account", "nmi", FrozenInstrument{Custodian: models.CustodianPSP, RailCustomerRef: "c", RailMethodRef: "m"}, nil},
	} {
		require.Error(t, ValidateEngineInstrument(tc.rail, tc.instrument, tc.binding), tc.name)
	}
}

func TestFrozenInstrumentCustodyAndMatching(t *testing.T) {
	t.Parallel()
	psp, custodian := uuid.New(), uuid.New()
	for _, tc := range []struct {
		name string
		in   FrozenInstrument
		ok   bool
	}{
		{"psp held", FrozenInstrument{PSPID: psp, Custodian: models.CustodianPSP}, true},
		{"custodian held", FrozenInstrument{PSPID: psp, Custodian: models.CustodianBasisTheory, CustodianID: &custodian}, true},
		{"psp with custodian id", FrozenInstrument{PSPID: psp, Custodian: models.CustodianPSP, CustodianID: &custodian}, false},
		{"custodian without id", FrozenInstrument{PSPID: psp, Custodian: models.CustodianHyperSwitch}, false},
		{"unknown custodian", FrozenInstrument{PSPID: psp, Custodian: "vault_co"}, false},
		{"no account", FrozenInstrument{Custodian: models.CustodianPSP}, false},
	} {
		require.Equal(t, tc.ok, tc.in.Validate() == nil, tc.name)
	}

	method := gen.BillingPaymentMethod{ID: uuid.New(), PspID: &psp, Custodian: models.CustodianPSP, RailCustomerRef: new(" vault "), RailMethodRef: new("billing")}
	frozen := FreezeInstrument(method, psp)
	require.Equal(t, "vault", frozen.RailCustomerRef)
	require.Nil(t, frozen.Mandate, "a card's agreements live on its mandates")
	require.False(t, frozen.CustodianHeld())
	require.NoError(t, frozen.Matches(method))

	for name, mutate := range map[string]func(*gen.BillingPaymentMethod){
		"account":  func(m *gen.BillingPaymentMethod) { other := uuid.New(); m.PspID = &other },
		"customer": func(m *gen.BillingPaymentMethod) { m.RailCustomerRef = new("vault2") },
		"method":   func(m *gen.BillingPaymentMethod) { m.RailMethodRef = new("billing2") },
		"custody": func(m *gen.BillingPaymentMethod) {
			m.Custodian, m.CustodianID = models.CustodianBasisTheory, &custodian
		},
	} {
		m := method
		mutate(&m)
		require.True(t, errors.Is(frozen.Matches(m), ErrInstrumentChanged), name)
	}

	// A card a custodian holds names no PSP: any PSP routing picks charges it,
	// and a PSP-held card only through its own PSP.
	custodial := method
	custodial.PspID, custodial.Custodian, custodial.CustodianID = nil, models.CustodianBasisTheory, &custodian
	require.True(t, ChargeableOn(custodial, uuid.New()))
	require.NoError(t, FreezeInstrument(custodial, psp).Matches(custodial))
	require.True(t, ChargeableOn(method, psp))
	require.False(t, ChargeableOn(method, uuid.New()))
	require.ErrorIs(t, FreezeInstrument(method, uuid.New()).Matches(method), ErrInstrumentChanged)
}

func TestHyperSwitchBindingIsCanonical(t *testing.T) {
	t.Parallel()
	for raw, want := range map[string]string{"https://vault.example": "https://vault.example", "https://vault.example/": "https://vault.example", "http://localhost:8080/api": "http://localhost:8080/api"} {
		got, err := CanonicalHyperSwitchDeployment(raw)
		require.NoError(t, err, raw)
		require.Equal(t, want, got, raw)
	}
	for _, raw := range []string{"", "vault.example", "ftp://vault.example", "https://user:pw@vault.example", "https://vault.example?x=1", "https://vault.example#f", " https://vault.example"} {
		_, err := CanonicalHyperSwitchDeployment(raw)
		require.Error(t, err, raw)
	}

	good := HyperSwitchBinding{AccountID: "acct", ProfileID: "prof", APIBaseURL: "https://vault.example"}
	require.NoError(t, good.Validate())
	for name, b := range map[string]HyperSwitchBinding{
		"no account":        {ProfileID: "prof", APIBaseURL: good.APIBaseURL},
		"padded profile":    {AccountID: "acct", ProfileID: " prof", APIBaseURL: good.APIBaseURL},
		"long account":      {AccountID: strings.Repeat("a", 257), ProfileID: "prof", APIBaseURL: good.APIBaseURL},
		"non-canonical url": {AccountID: "acct", ProfileID: "prof", APIBaseURL: "https://vault.example/"},
	} {
		require.Error(t, b.Validate(), name)
	}
}

func TestCustomerPaymentKeyIsScoped(t *testing.T) {
	t.Parallel()
	payer, other := uuid.New(), uuid.New()
	key := CustomerPaymentKey("checkout", payer, "client-key")
	require.True(t, CustomerPaymentKeyValid("checkout", payer, key))
	require.Equal(t, key, CustomerPaymentKey("checkout", payer, "client-key"))
	require.NotEqual(t, key, CustomerPaymentKey("checkout", payer, "client-key-2"))
	require.False(t, CustomerPaymentKeyValid("checkout", other, key), "another payer")
	require.False(t, CustomerPaymentKeyValid("upgrade", payer, key), "another command")
	require.False(t, CustomerPaymentKeyValid("checkout", payer, strings.ToUpper(key)))
	require.False(t, CustomerPaymentKeyValid("checkout", payer, "checkout:customer:"+payer.String()+":abcd"), "short digest")
	require.False(t, CustomerPaymentKeyValid("checkout", payer, "client-key"), "an unscoped raw key")
}
