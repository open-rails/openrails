package service

import (
	"testing"

	"github.com/open-rails/openrails/internal/db/models"
)

func TestProductBenefitFingerprintStableAndSensitive(t *testing.T) {
	product := &models.Product{
		EntitlementsSpec: map[string]*int{"pro": nil, "export": nil},
	}
	same := &models.Product{
		EntitlementsSpec: map[string]*int{"export": nil, "pro": nil},
	}
	changed := &models.Product{
		EntitlementsSpec: map[string]*int{"export": nil},
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
