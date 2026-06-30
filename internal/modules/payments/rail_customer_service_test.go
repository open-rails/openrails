package payments

import "testing"

// #635: rail_customers is materialized only for rails with a real card-independent
// remote customer object (Stripe cus_*, NMI customer_vault_id).
func TestRailHasRemoteCustomer(t *testing.T) {
	cases := map[string]bool{
		"stripe": true, "nmi": true, " stripe ": true,
		// #630: mobius is a provider-account name on rail nmi, not a rail.
		"mobius": false, "ccbill": false, "solana": false, "paypal": false, "admin": false, "manual": false, "": false,
	}
	for rail, want := range cases {
		if got := railHasRemoteCustomer(rail); got != want {
			t.Errorf("railHasRemoteCustomer(%q) = %v, want %v", rail, got, want)
		}
	}
}
