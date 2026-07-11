package embed

import (
	"strings"
	"testing"
)

// #711: ParseMerchantConfig is strict — a typo'd key must fail loudly, never
// silently provision a merchant with no rails.
func TestParseMerchantConfigStrict(t *testing.T) {
	valid := []byte(`
display_name: Doujins
psps:
  mobius:
    nmi:
      account_id: "579145"
      secrets:
        security_key: sk
        webhook_signing_secret: whs
`)
	m, err := ParseMerchantConfig(valid)
	if err != nil {
		t.Fatalf("valid config: %v", err)
	}
	if len(m.PSPs) != 1 {
		t.Fatalf("accounts = %d, want 1", len(m.PSPs))
	}

	typo := []byte(`
display_name: Doujins
acounts:
  mobius:
    nmi:
      account_id: "579145"
`)
	if _, err := ParseMerchantConfig(typo); err == nil {
		t.Fatal("typo'd 'acounts:' must be rejected, not silently dropped")
	}

	renamed := []byte(`
display_name: Doujins
rail_merchant_accounts:
  mobius:
    nmi:
      account_id: "579145"
`)
	_, err = ParseMerchantConfig(renamed)
	if err == nil || !strings.Contains(err.Error(), "renamed to psps") {
		t.Fatalf("retired key must get a rename pointer, got: %v", err)
	}
}
