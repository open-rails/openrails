package cardguard

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
)

// Redacted is what a Card prints, logs and marshals as.
const Redacted = "[card]"

// Card is a payment card as its holder typed it, for a PSP whose card_entry is
// server (#1129). It lives only between the request that carried it and the
// gateway call that vaults it: every formatter and encoder renders Redacted,
// and Zero wipes the number and security code. Only the gateway integration
// reads them (Unseal).
//
// The value holds one closure, so a reflective printer that cannot call its
// methods (an unexported field under %+v, a test differ) shows an address,
// never the card.
type Card struct{ open func() *cardSecret }

func (c *Card) secret() *cardSecret {
	if c == nil || c.open == nil {
		return nil
	}
	return c.open()
}

type cardSecret struct {
	number, cvc  []byte // ASCII digits
	month, year  int
	brand, last4 string
}

// CardError names the card field a request got wrong. It never carries the
// value.
type CardError struct {
	Field    string
	redacted bool
}

func (e *CardError) Error() string {
	switch {
	case e.redacted:
		return "card was redacted in transit: a card reaches OpenRails only in the request that first carried it"
	case e.Field == "":
		return "card is invalid"
	}
	return "card " + e.Field + " is invalid"
}

// ClientSafeBindMessage lets the request binder return the message as is.
func (e *CardError) ClientSafeBindMessage() string { return e.Error() }

// ErrCardRedacted: the card arrived as its own redaction. A card cannot be
// re-encoded, so it reaches OpenRails only in the request that first carried it.
var ErrCardRedacted = &CardError{redacted: true}

// NewCard builds a Card from its parts; year may be two or four digits.
func NewCard(number string, month, year int, cvc string) (*Card, error) {
	digits, ok := cardDigits([]byte(number), true)
	if !ok {
		return nil, &CardError{Field: "number"}
	}
	code, ok := cardDigits([]byte(cvc), false)
	if !ok {
		clear(digits)
		return nil, &CardError{Field: "cvc"}
	}
	return newCard(digits, code, month, year)
}

func newCard(number, cvc []byte, month, year int) (*Card, error) {
	fail := func(field string) (*Card, error) {
		clear(number)
		clear(cvc)
		return nil, &CardError{Field: field}
	}
	if len(number) < 12 || len(number) > maxPANDigits {
		return fail("number")
	}
	values := make([]byte, len(number))
	for i, d := range number {
		values[i] = d - '0'
	}
	valid := luhnValid(values)
	clear(values)
	if !valid {
		return fail("number")
	}
	if month < 1 || month > 12 {
		return fail("exp_month")
	}
	if year >= 0 && year <= 99 {
		year += 2000
	}
	if year < 2000 || year > 2099 {
		return fail("exp_year")
	}
	if len(cvc) < 3 || len(cvc) > 4 {
		return fail("cvc")
	}
	secret := &cardSecret{
		number: number, cvc: cvc, month: month, year: year,
		brand: BrandFromLeadingDigits(string(number[:6])), last4: string(number[len(number)-4:]),
	}
	return &Card{open: func() *cardSecret { return secret }}, nil
}

// cardDigits copies the digits of a card field. Card formatting (spaces and
// dashes) is accepted in a number and nowhere else.
func cardDigits(raw []byte, formatted bool) ([]byte, bool) {
	out := make([]byte, 0, len(raw))
	for _, b := range raw {
		switch {
		case isDigit(b):
			out = append(out, b)
		case formatted && (b == ' ' || b == '-'):
		default:
			clear(out)
			return nil, false
		}
	}
	return out, true
}

// UnmarshalJSON decodes {"number","exp_month","exp_year","cvc"}. Errors name
// the field and never echo the input.
func (c *Card) UnmarshalJSON(data []byte) error {
	if string(data) == `"`+Redacted+`"` {
		return ErrCardRedacted
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return &CardError{}
	}
	defer func() {
		for _, raw := range fields {
			clear(raw)
		}
	}()
	for name := range fields {
		switch name {
		case "number", "exp_month", "exp_year", "cvc":
		default:
			return &CardError{}
		}
	}
	text := func(name string, formatted bool) ([]byte, bool) {
		raw := fields[name]
		if len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' {
			return nil, false
		}
		return cardDigits(raw[1:len(raw)-1], formatted)
	}
	integer := func(name string) (int, bool) {
		raw := fields[name]
		if len(raw) >= 2 && raw[0] == '"' && raw[len(raw)-1] == '"' {
			raw = raw[1 : len(raw)-1]
		}
		if len(raw) == 0 || len(raw) > 4 {
			return 0, false
		}
		n := 0
		for _, b := range raw {
			if !isDigit(b) {
				return 0, false
			}
			n = n*10 + int(b-'0')
		}
		return n, true
	}
	number, ok := text("number", true)
	if !ok {
		return &CardError{Field: "number"}
	}
	cvc, ok := text("cvc", false)
	if !ok {
		clear(number)
		return &CardError{Field: "cvc"}
	}
	month, ok := integer("exp_month")
	if !ok {
		clear(number)
		clear(cvc)
		return &CardError{Field: "exp_month"}
	}
	year, ok := integer("exp_year")
	if !ok {
		clear(number)
		clear(cvc)
		return &CardError{Field: "exp_year"}
	}
	card, err := newCard(number, cvc, month, year)
	if err != nil {
		return err
	}
	*c = *card
	return nil
}

func (Card) String() string                 { return Redacted }
func (Card) GoString() string               { return Redacted }
func (Card) Format(f fmt.State, _ rune)     { _, _ = io.WriteString(f, Redacted) }
func (Card) MarshalJSON() ([]byte, error)   { return []byte(`"` + Redacted + `"`), nil }
func (Card) MarshalText() ([]byte, error)   { return []byte(Redacted), nil }
func (Card) MarshalBinary() ([]byte, error) { return []byte(Redacted), nil }
func (Card) LogValue() slog.Value           { return slog.StringValue(Redacted) }

// Brand, LastFour and Expiry are the card's display facts: what a saved card
// is labelled with. They survive Zero.

func (c *Card) Brand() string {
	if s := c.secret(); s != nil {
		return s.brand
	}
	return ""
}

func (c *Card) LastFour() string {
	if s := c.secret(); s != nil {
		return s.last4
	}
	return ""
}

// Expiry is MM/YY.
func (c *Card) Expiry() string {
	if s := c.secret(); s != nil {
		return fmt.Sprintf("%02d/%02d", s.month, s.year%100)
	}
	return ""
}

// Unseal hands the number and security code to use, for the one gateway call
// that vaults the card. The slices are the card's own: use must not keep them.
// It reports false once the card was wiped.
func (c *Card) Unseal(use func(number, cvc []byte, month, year int)) bool {
	s := c.secret()
	if s == nil || len(s.number) == 0 {
		return false
	}
	use(s.number, s.cvc, s.month, s.year)
	return true
}

// Zero wipes the number and security code. Safe to repeat and on nil.
func (c *Card) Zero() {
	s := c.secret()
	if s == nil {
		return
	}
	clear(s.number)
	clear(s.cvc)
	s.number, s.cvc = nil, nil
}

type cardContextKey struct{}

// WithCard carries a card to the provider operation executing in this request.
// A later run of the same operation has none: the card is never stored.
func WithCard(ctx context.Context, c *Card) context.Context {
	if c == nil {
		return ctx
	}
	return context.WithValue(ctx, cardContextKey{}, c)
}

// CardFrom is the card this request carried, or nil.
func CardFrom(ctx context.Context) *Card {
	c, _ := ctx.Value(cardContextKey{}).(*Card)
	return c
}

// BrandFromLeadingDigits names the card network from a number's leading
// digits ("" when they do not identify one).
func BrandFromLeadingDigits(lead string) string {
	prefix := func(n int) int {
		if len(lead) < n {
			return -1
		}
		v := 0
		for i := 0; i < n; i++ {
			if !isDigit(lead[i]) {
				return -1
			}
			v = v*10 + int(lead[i]-'0')
		}
		return v
	}
	switch p1, p2, p3, p4 := prefix(1), prefix(2), prefix(3), prefix(4); {
	case p1 == 4:
		return "visa"
	case p2 == 34 || p2 == 37:
		return "amex"
	case p2 >= 51 && p2 <= 55, p4 >= 2221 && p4 <= 2720:
		return "mastercard"
	case p4 == 6011, p2 == 65, p3 >= 644 && p3 <= 649:
		return "discover"
	case p2 == 35:
		return "jcb"
	case p2 == 36 || p2 == 38 || (p3 >= 300 && p3 <= 305):
		return "diners"
	}
	return ""
}
