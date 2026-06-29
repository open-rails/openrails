package riverjobs

import (
	"testing"

	"github.com/open-rails/openrails/internal/db/models"
)

// #635: provider-auto-billed classification gates dunning — CCBill always,
// NMI/mobius only when vault-less; an NMI sub with a vault is our-rebill.
func TestSubscriptionProviderAutoBilled(t *testing.T) {
	withVault := &models.PaymentMethod{RailMethodRef: "vault-123", RailCustomerRef: "cust-1"}
	noVault := &models.PaymentMethod{}

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
		{"mobius vault-less → auto-billed", "mobius", nil, true},
		{"nmi with vault → our-rebill", "nmi", withVault, false},
		{"mobius with vault → our-rebill", "mobius", withVault, false},
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
