package checkout

import (
	"fmt"

	"github.com/open-rails/openrails/internal/cardguard"
	"github.com/open-rails/openrails/internal/config"
)

// PAN firewall (SAQ A): custodian-held-card checkout accepts only the BT
// token-intent handle. A raw card number in any request field would escalate
// the PCI posture (SAQ A to SAQ D), so card-number-shaped values are refused,
// never stored or forwarded.
//
// Every field is scanned. Identifiers pass by the detector's grouping rule
// (internal/cardguard), never by a per-field exemption, which would be a hole
// to aim at.
//
// The one card OpenRails takes is the typed `card` field (cardguard.Card),
// only for a PSP that declares card_entry: server (cardEntryFor). It is not a
// string field, so nothing here is relaxed for it.

// cardEntryFor is where the PSP a request routed to takes new cards.
func cardEntryFor(target railTarget) string {
	if target.Scope == nil {
		return config.CardEntryBrowser
	}
	entry, err := config.CardEntry(target.Scope.Rail, target.Scope.Settings, target.Scope.CustodianID != nil)
	if err != nil {
		return config.CardEntryBrowser
	}
	return entry
}

// describeCard stamps a request's card onto the display fields it names
// (nothing the caller says about the card is used) and returns its wipe.
func describeCard(req *CheckoutAttemptCreateRequest) func() {
	if req == nil || req.Payment.Card == nil {
		return func() {}
	}
	card := req.Payment.Card
	req.Payment.LastFour, req.Payment.CardType, req.Payment.ExpiryDate = card.LastFour(), card.Brand(), card.Expiry()
	return card.Zero
}

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
