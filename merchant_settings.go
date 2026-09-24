package openrails

// CheckoutRoutingRule is one processor-preference rule (or#288). Rules are
// evaluated in declaration order and the FIRST whose Match accepts the routing
// inputs wins — no scoring, no second pass. Prefer is that rule's ranked
// candidate list AND its whitelist: a PSP the winning rule does not name is not
// eligible, so a rule can constrain a product to one rail.
type CheckoutRoutingRule struct {
	Match CheckoutRoutingMatch `json:"match,omitempty"`
	// Prefer are checkout selectors in preference order — PSP keys ("mobius"),
	// or a rail kind where the #848 wire accepts one (exactly one armed PSP).
	Prefer []string `json:"prefer"`
}

// CheckoutRoutingMatch is a rule's condition. Every SET field must match the
// routing inputs; an all-empty match accepts everything (the catch-all rule).
type CheckoutRoutingMatch struct {
	Currency string `json:"currency,omitempty"` // ISO-4217, uppercase (matched case-insensitively)
	Product  string `json:"product,omitempty"`  // product key
	Price    string `json:"price,omitempty"`    // price key
	Mode     string `json:"mode,omitempty"`     // one_off | subscription
	Country  string `json:"country,omitempty"`  // payer country, ISO-3166-1 alpha-2
}

// IsCatchAll reports a match with no conditions — it accepts every input, so
// no later rule can ever be reached.
func (m CheckoutRoutingMatch) IsCatchAll() bool {
	return m.Currency == "" && m.Product == "" && m.Price == "" && m.Mode == "" && m.Country == ""
}

// MerchantSettings.ProviderRefundAccess values.
const (
	ProviderRefundRevokeOnFull = "revoke_on_full"
	ProviderRefundRevokeOnAny  = "revoke_on_any"
	ProviderRefundKeep         = "keep"
)
