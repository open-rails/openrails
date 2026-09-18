package checkout

import (
	"fmt"

	"github.com/open-rails/openrails/internal/cardguard"
)

// PAN firewall (#795 B5, SAQ A): custodian-held-card checkout accepts ONLY the BT
// token-intent handle. A raw card number reaching OpenRails — pasted into any
// request field — would silently escalate the PCI posture (SAQ A -> SAQ D), so
// card-number-shaped values are rejected LOUDLY, never stored or forwarded.
//
// Every field is scanned unconditionally. Identifiers are kept out of the
// refusals by the detector's grouping rule (internal/cardguard), never by a
// per-field exemption: an exemption is a hole an attacker can aim at, and it
// only ever covered the fields someone had already been burned by.

// RejectPANShapedFields errors when any string field of the checkout request
// contains a card number.
func RejectPANShapedFields(req *CheckoutRequest) error {
	if req == nil {
		return nil
	}
	fields := map[string]string{
		"payment_token":      req.PaymentToken,
		"bt_token_intent_id": req.BTTokenIntentID,
		"payment_method_id":  req.PaymentMethodID,
		"email":              req.Email,
		"name_on_card":       req.NameOnCard,
		"first_name":         req.FirstName,
		"last_name":          req.LastName,
		"address1":           req.Address1,
		"city":               req.City,
		"state":              req.State,
		"zip":                req.Zip,
		"country":            req.Country,
		"last_four":          req.LastFour,
		"expiry_date":        req.ExpiryDate,
		"card_type":          req.CardType,
	}
	for key, value := range req.Metadata {
		if cardguard.ContainsPAN(key) {
			return fmt.Errorf("metadata key contains a card-number-shaped value: raw PANs must never reach OpenRails (SAQ A) — collect cards via the vault's browser SDK")
		}
		fields["metadata."+key] = value
	}
	for name, value := range fields {
		if cardguard.ContainsPAN(value) {
			return fmt.Errorf("field %q contains a card-number-shaped value: raw PANs must never reach OpenRails (SAQ A) — collect cards via the vault's browser SDK", name)
		}
	}
	return nil
}
