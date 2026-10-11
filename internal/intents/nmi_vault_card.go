package intents

import (
	"errors"
	"strings"
	"unicode"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
)

type nmiCard struct {
	LastFour   string `json:"last_four"`
	CardType   string `json:"card_type"`
	ExpiryDate string `json:"expiry_date"`
}

// complete: last four and expiry identify a masked card. NMI may omit the
// brand, so it is optional.
func (c nmiCard) complete() bool {
	return c.LastFour != "" && c.ExpiryDate != ""
}

// model is the card as a stored method's display facts.
func (c nmiCard) model() models.Card { return models.ParseCard(c.CardType, c.LastFour, c.ExpiryDate) }

// errNMICardDataGap is a stored card NMI reports without its last four or
// expiry: deterministic, so retrying cannot resolve it.
var errNMICardDataGap = errors.New("NMI billing entry returned incomplete masked card metadata")

// billingEntry finds a vault billing entry by id.
func billingEntry(customer nmi.V5Customer, billingID string) (nmi.V5CustomerBilling, bool) {
	ref := strings.TrimSpace(billingID)
	for _, b := range customer.Billing {
		if strings.TrimSpace(b.ID) == ref {
			return b, true
		}
	}
	return nmi.V5CustomerBilling{}, false
}

var errNMIBillingEntryAbsent = errors.New("billing entry absent")

// entryCard is the masked card of one billing entry ("" = the primary).
func entryCard(customer nmi.V5Customer, billingID string) (nmiCard, error) {
	if strings.TrimSpace(billingID) == "" {
		if primary := customer.PrimaryBilling(); primary != nil {
			return billingCard(*primary)
		}
		return nmiCard{}, errNMIBillingEntryAbsent
	}
	b, ok := billingEntry(customer, billingID)
	if !ok {
		return nmiCard{}, errNMIBillingEntryAbsent
	}
	return billingCard(b)
}

// NMIVaultCard is the masked card of one billing entry of an NMI vault ("" =
// the primary).
func NMIVaultCard(customer nmi.V5Customer, billingID string) (models.Card, error) {
	card, err := entryCard(customer, billingID)
	if err != nil {
		return models.Card{}, err
	}
	return card.model(), nil
}

func billingCard(b nmi.V5CustomerBilling) (nmiCard, error) {
	card := nmiCard{
		LastFour:   maskedLastFour(b.PaymentDetails.CardNumber),
		CardType:   strings.TrimSpace(b.PaymentDetails.CardType),
		ExpiryDate: normalizeNMIExpiry(b.PaymentDetails.CardExp),
	}
	if card.CardType == "" {
		card.CardType = nmi.CardBrandFromMaskedPAN(b.PaymentDetails.CardNumber)
	}
	if !card.complete() {
		return nmiCard{}, errNMICardDataGap
	}
	return card, nil
}

func maskedLastFour(value string) string {
	var digits strings.Builder
	for _, r := range value {
		if unicode.IsDigit(r) {
			digits.WriteRune(r)
		}
	}
	out := digits.String()
	if len(out) < 4 {
		return ""
	}
	return out[len(out)-4:]
}

func normalizeNMIExpiry(value string) string {
	var digits strings.Builder
	for _, r := range value {
		if unicode.IsDigit(r) {
			digits.WriteRune(r)
		}
	}
	raw := digits.String()
	switch len(raw) {
	case 4:
		return raw[:2] + "/" + raw[2:]
	case 6:
		return raw[:2] + "/" + raw[4:]
	default:
		return ""
	}
}
