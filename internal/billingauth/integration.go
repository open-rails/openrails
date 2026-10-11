package billingauth

import "github.com/open-rails/openrails/billing"

// Target is the merchant a request acts on: the one its credential is bound
// to, resolved by the route gate once.
type Target struct {
	MerchantID billing.MerchantID
}
