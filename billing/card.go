package billing

import (
	"fmt"
	"io"
	"log/slog"

	"github.com/open-rails/openrails/internal/cardguard"
)

// Card is a payment card as its holder typed it, accepted only for a PSP
// whose card_entry is server. It decodes from
// {"number","exp_month","exp_year","cvc"} and prints, logs and re-encodes as
// a redaction, so it reaches OpenRails only in the request that first carried
// it; OpenRails wipes it once the gateway has vaulted it. Nothing outside
// OpenRails reads its number or security code.
type Card cardguard.Card

// NewCard builds a Card from its parts; year may be two or four digits.
func NewCard(number string, expMonth, expYear int, cvc string) (*Card, error) {
	c, err := cardguard.NewCard(number, expMonth, expYear, cvc)
	return (*Card)(c), err
}

// UnmarshalJSON decodes {"number","exp_month","exp_year","cvc"}. Errors name
// the field and never echo the input.
func (c *Card) UnmarshalJSON(data []byte) error { return (*cardguard.Card)(c).UnmarshalJSON(data) }

// Every formatter and encoder renders the redaction.

func (Card) String() string                 { return cardguard.Redacted }
func (Card) GoString() string               { return cardguard.Redacted }
func (Card) Format(f fmt.State, _ rune)     { _, _ = io.WriteString(f, cardguard.Redacted) }
func (Card) MarshalJSON() ([]byte, error)   { return []byte(`"` + cardguard.Redacted + `"`), nil }
func (Card) MarshalText() ([]byte, error)   { return []byte(cardguard.Redacted), nil }
func (Card) MarshalBinary() ([]byte, error) { return []byte(cardguard.Redacted), nil }
func (Card) LogValue() slog.Value           { return slog.StringValue(cardguard.Redacted) }

// Brand, BIN, LastFour and Expiry are the card's display facts: what a saved
// card is labelled with. They survive Zero.

func (c *Card) Brand() string { return (*cardguard.Card)(c).Brand() }

// BIN is the card number's first six digits.
func (c *Card) BIN() string { return (*cardguard.Card)(c).BIN() }

func (c *Card) LastFour() string { return (*cardguard.Card)(c).LastFour() }

// ExpiryMonthYear is the expiry month (1-12) and four-digit year.
func (c *Card) ExpiryMonthYear() (month, year int) {
	return (*cardguard.Card)(c).ExpiryMonthYear()
}

// Expiry is MM/YY.
func (c *Card) Expiry() string { return (*cardguard.Card)(c).Expiry() }

// Zero wipes the number and security code. Safe to repeat and on nil.
func (c *Card) Zero() { (*cardguard.Card)(c).Zero() }
