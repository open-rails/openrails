package merchantbootstrap

import (
	"testing"

	"github.com/open-rails/openrails/config"
)

func TestValidateMerchantDeclarationStripePublishableKey(t *testing.T) {
	declare := func(key string) MerchantConfig {
		return MerchantConfig{PSPs: map[string]PSPConfig{"stripe": {"stripe": {AccountID: "acct_1", Settings: map[string]any{"publishable_key": key}}}}}
	}
	sandbox := &config.Config{TestMode: config.CredentialPostureSandbox}
	if err := ValidateMerchantDeclaration(sandbox, declare("pk_test_abc")); err != nil {
		t.Fatalf("pk_test_ under sandbox: %v", err)
	}
	if err := ValidateMerchantDeclaration(sandbox, declare("pk_live_abc")); err == nil {
		t.Fatal("pk_live_ under sandbox must be refused")
	}
	if err := ValidateMerchantDeclaration(&config.Config{TestMode: config.CredentialPostureLive}, declare("pk_test_abc")); err == nil {
		t.Fatal("pk_test_ under live must be refused")
	}
}
