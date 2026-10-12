package billing

// PSPRoutingRule is one processor-preference rule. Rules are evaluated in
// declaration order and the first whose Match accepts the routing inputs wins.
// Prefer is that rule's ranked candidate list and its whitelist: a PSP the
// winning rule does not name is not eligible, so a rule can constrain a
// product to one rail.
type PSPRoutingRule struct {
	Match PSPRoutingMatch `json:"match,omitempty"`
	// Prefer are purchase selectors in preference order: PSP keys ("mobius"),
	// or a rail kind, which resolves to that rail's one armed PSP.
	Prefer []string `json:"prefer"`
}

// PSPRoutingMatch is a rule's condition. Every SET field must match the
// routing inputs; an all-empty match accepts everything (the catch-all rule).
type PSPRoutingMatch struct {
	Currency string `json:"currency,omitempty"` // Registered currency code, uppercase (matched case-insensitively)
	Product  string `json:"product,omitempty"`  // product key
	Price    string `json:"price,omitempty"`    // price key
	Mode     string `json:"mode,omitempty"`     // one_off | subscription
	Country  string `json:"country,omitempty"`  // unsupported; nonempty values are rejected
}

// IsCatchAll reports a match with no conditions — it accepts every input, so
// no later rule can ever be reached.
func (m PSPRoutingMatch) IsCatchAll() bool {
	return m.Currency == "" && m.Product == "" && m.Price == "" && m.Mode == "" && m.Country == ""
}

// MerchantSettings.ProviderRefundAccess values.
const (
	ProviderRefundRevokeOnFull = "revoke_on_full"
	ProviderRefundRevokeOnAny  = "revoke_on_any"
	ProviderRefundKeep         = "keep"
)
