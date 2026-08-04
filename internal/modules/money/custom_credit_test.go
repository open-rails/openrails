package money

import (
	"context"
	"errors"
	"testing"

	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

func TestRequireBillingCurrencyRejectsCustom(t *testing.T) {
	if err := RequireBillingCurrency("usd"); err != nil {
		t.Errorf("built-in usd must pass billing: %v", err)
	}
	if err := RequireBillingCurrency(""); err == nil {
		t.Error("empty currency must be rejected")
	}
	err := RequireBillingCurrency("tensorhub/gold")
	if !errors.Is(err, ErrBillingUnitRequired) {
		t.Errorf("qualified custom unit must be rejected at billing, got %v", err)
	}
}

func TestCurrencyRegistryRejectsBlank(t *testing.T) {
	if got := NormalizeCurrency(""); got != "" {
		t.Fatalf("NormalizeCurrency(\"\") = %q, want empty", got)
	}
	if err := moneyutil.ValidateCurrency(""); err == nil {
		t.Fatal("moneyutil.ValidateCurrency(\"\") must reject blank")
	}
	if _, ok := moneyutil.CurrencyScale(""); ok {
		t.Fatal("moneyutil.CurrencyScale(\"\") must reject blank")
	}
}

func TestResolveUnitBuiltin(t *testing.T) {
	s := &MoneyService{}
	d, builtin, err := s.ResolveUnit(context.Background(), "usd")
	if err != nil || !builtin || d != 6 {
		t.Errorf("usd => (6,true,nil), got (%d,%v,%v)", d, builtin, err)
	}
	if _, _, err := s.ResolveUnit(context.Background(), "doge"); err == nil {
		t.Error("unknown built-in must error")
	}
}
