package riverjobs

import (
	"testing"

	"github.com/open-rails/openrails/internal/db/models"
)

// #635/#682: provider-auto-billed classification gates dunning — CCBill
// always; NMI by the EXPLICIT RebillDriver mode (#682), no longer inferred
// from method-ref emptiness.
func TestSubscriptionProviderAutoBilled(t *testing.T) {
	withVault := &models.PaymentMethod{RailMethodRef: "vault-123", RailCustomerRef: "cust-1", RebillDriver: models.RebillDriverOpenRails}
	noVault := &models.PaymentMethod{RebillDriver: models.RebillDriverProvider}

	cases := []struct {
		name string
		rail string
		pm   *models.PaymentMethod
		want bool
	}{
		{"ccbill always auto-billed", "ccbill", nil, true},
		{"ccbill auto-billed even with pm", "ccbill", withVault, true},
		{"nmi vault-less → auto-billed", "nmi", nil, true},
		{"nmi empty pm → auto-billed", "nmi", noVault, true},
		{"nmi with vault → our-rebill", "nmi", withVault, false},
		// #682: the mode column decides — a billing id WITHOUT the openrails
		// mode stays provider-billed (native vaults may capture billing ids
		// without flipping lanes), and the mode alone flips it.
		{"nmi ref without openrails mode → auto-billed", "nmi", &models.PaymentMethod{RailMethodRef: "vault-123", RebillDriver: models.RebillDriverProvider}, true},
		{"nmi openrails mode without ref → our-rebill", "nmi", &models.PaymentMethod{RebillDriver: models.RebillDriverOpenRails}, false},
		{"stripe → not this path", "stripe", nil, false},
		{"unknown rail → false", "paypal", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := subscriptionProviderAutoBilled(c.rail, c.pm); got != c.want {
				t.Errorf("subscriptionProviderAutoBilled(%q, %v) = %v, want %v", c.rail, c.pm, got, c.want)
			}
		})
	}
}
