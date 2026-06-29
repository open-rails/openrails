package service

import (
	"testing"

	"github.com/open-rails/openrails/internal/db/models"
)

func TestProductBenefitFingerprintStableAndSensitive(t *testing.T) {
	product := &models.Product{
		EntitlementsSpec: map[string]*int{"pro": nil, "export": nil},
		CreditsSpec: models.CreditsSpec{
			"monthly": {Unit: "usd", Amount: 10_000_000, Cadence: models.CreditGrantCadencePerRenewal},
		},
	}
	same := &models.Product{
		EntitlementsSpec: map[string]*int{"export": nil, "pro": nil},
		CreditsSpec: models.CreditsSpec{
			"monthly": {Unit: "usd", Amount: 10_000_000, Cadence: models.CreditGrantCadencePerRenewal},
		},
	}
	changed := &models.Product{
		EntitlementsSpec: map[string]*int{"export": nil, "pro": nil},
		CreditsSpec: models.CreditsSpec{
			"monthly": {Unit: "usd", Amount: 20_000_000, Cadence: models.CreditGrantCadencePerRenewal},
		},
	}

	got := productBenefitFingerprint(product)
	if got == "" {
		t.Fatal("fingerprint should be populated")
	}
	if got != productBenefitFingerprint(same) {
		t.Fatal("fingerprint should be stable across map iteration order")
	}
	if got == productBenefitFingerprint(changed) {
		t.Fatal("fingerprint should change when benefit payload changes")
	}
}
