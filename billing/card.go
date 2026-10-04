package billing

import "github.com/open-rails/openrails/internal/cardguard"

// Card is a payment card as its holder typed it, accepted only for a PSP
// whose card_entry is server. It decodes from
// {"number","exp_month","exp_year","cvc"} and prints, logs and re-encodes as
// a redaction, so it reaches OpenRails only in the request that first carried
// it; OpenRails wipes it once the gateway has vaulted it.
type Card = cardguard.Card

// NewCard builds a Card from its parts; year may be two or four digits.
func NewCard(number string, expMonth, expYear int, cvc string) (*Card, error) {
	return cardguard.NewCard(number, expMonth, expYear, cvc)
}
