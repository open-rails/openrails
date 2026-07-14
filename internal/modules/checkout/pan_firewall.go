package checkout

import (
	"fmt"
)

// PAN firewall (#795 B5, SAQ A): the vaulted_card checkout accepts ONLY the BT
// token-intent handle. A raw card number reaching OpenRails — pasted into any
// request field — would silently escalate the PCI posture (SAQ A -> SAQ D), so
// card-number-shaped values are rejected LOUDLY, never stored or forwarded.

// RejectPANShapedFields errors when any string field of the checkout request
// contains a 13-19 digit Luhn-passing sequence (spaces/dashes tolerated).
func RejectPANShapedFields(req *CheckoutRequest) error {
	if req == nil {
		return nil
	}
	fields := map[string]string{
		"payment_token":      req.PaymentToken,
		"bt_token_intent_id": req.BTTokenIntentID,
		"payment_method_id":  req.PaymentMethodID,
		"email":              req.Email,
		"first_name":         req.FirstName,
		"last_name":          req.LastName,
		"address1":           req.Address1,
		"city":               req.City,
		"state":              req.State,
		"zip":                req.Zip,
		"country":            req.Country,
		"expiry_date":        req.ExpiryDate,
		"card_type":          req.CardType,
	}
	for key, value := range req.Metadata {
		fields["metadata."+key] = value
	}
	for name, value := range fields {
		if looksLikePAN(value) {
			return fmt.Errorf("field %q contains a card-number-shaped value: raw PANs must never reach OpenRails (SAQ A) — collect cards via the vault's browser SDK", name)
		}
	}
	return nil
}

// looksLikePAN reports whether s contains a 13-19 digit Luhn-valid run.
func looksLikePAN(s string) bool {
	if s == "" {
		return false
	}
	var digits []byte
	flush := func() bool {
		defer func() { digits = digits[:0] }()
		return len(digits) >= 13 && len(digits) <= 19 && luhnValid(digits)
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			digits = append(digits, c-'0')
		case c == ' ' || c == '-':
			// separators inside a formatted PAN
		default:
			if flush() {
				return true
			}
		}
	}
	return flush()
}

func luhnValid(digits []byte) bool {
	sum := 0
	double := false
	for i := len(digits) - 1; i >= 0; i-- {
		d := int(digits[i])
		if double {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		double = !double
	}
	return sum%10 == 0
}
