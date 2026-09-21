package charge

import (
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
)

func TestEngineExecutionBindings(t *testing.T) {
	provider := FrozenInstrument{PSPID: uuid.New(), Custodian: models.CustodianPSP,
		RailCustomerRef: "customer", RailMethodRef: "method", StoredCredentialRecurringRef: "initial-payment"}
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
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateEngineInstrument(tc.rail, tc.instrument, tc.binding, true); err != nil {
				t.Fatal(err)
			}
			noAgreement := tc.instrument
			noAgreement.StoredCredentialRecurringRef = ""
			if err := ValidateEngineInstrument(tc.rail, noAgreement, tc.binding, true); err == nil {
				t.Fatal("renewal accepted an unestablished recurring agreement")
			}
			if err := ValidateEngineInstrument(tc.rail, noAgreement, tc.binding, false); err != nil {
				t.Fatalf("initial customer payment cannot establish agreement: %v", err)
			}
			missingMethod := tc.instrument
			missingMethod.RailMethodRef = ""
			if err := ValidateEngineInstrument(tc.rail, missingMethod, tc.binding, false); err == nil {
				t.Fatal("engine accepted provider default method instead of exact instrument")
			}
		})
	}
	for _, tc := range []struct {
		name, rail string
		instrument FrozenInstrument
		binding    *HyperSwitchBinding
	}{
		{"ccbill_has_no_engine_adapter", "ccbill", provider, nil},
		{"stripe_cannot_use_nmi_proxy", "stripe", proxied, binding},
		{"native_cannot_carry_proxy_binding", "nmi", provider, binding},
		{"proxy_requires_frozen_profile", "nmi", proxied, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateEngineInstrument(tc.rail, tc.instrument, tc.binding, true); err == nil {
				t.Fatal("unsupported execution binding accepted")
			}
		})
	}
}
