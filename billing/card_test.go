package billing_test

import (
	"bytes"
	"encoding"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/open-rails/openrails/billing"
)

// Card is billing's own type: its formatter, LogValuer and marshalers are
// declared there, so every rendering of it is the redaction.
func TestCardRedacts(t *testing.T) {
	var (
		_ fmt.Formatter            = billing.Card{}
		_ fmt.Stringer             = billing.Card{}
		_ fmt.GoStringer           = billing.Card{}
		_ slog.LogValuer           = billing.Card{}
		_ json.Marshaler           = billing.Card{}
		_ encoding.TextMarshaler   = billing.Card{}
		_ encoding.BinaryMarshaler = billing.Card{}
		_ json.Unmarshaler         = (*billing.Card)(nil)
	)
	var card billing.Card
	if err := json.Unmarshal([]byte(`{"number":"4111 1111 1111 1111","exp_month":"07","exp_year":31,"cvc":"987"}`), &card); err != nil {
		t.Fatal(err)
	}
	if card.Brand() != "visa" || card.BIN() != "411111" || card.LastFour() != "1111" || card.Expiry() != "07/31" {
		t.Fatalf("display facts = %s %s %s %s", card.Brand(), card.BIN(), card.LastFour(), card.Expiry())
	}
	wire, err := json.Marshal(struct{ Card *billing.Card }{&card})
	if err != nil {
		t.Fatal(err)
	}
	text, _ := card.MarshalText()
	binary, _ := card.MarshalBinary()
	var logged bytes.Buffer
	slog.New(slog.NewJSONHandler(&logged, nil)).Info("card", "card", card, "pointer", &card)
	for _, out := range []string{
		fmt.Sprint(card), fmt.Sprintf("%+v %#v %s %q %x %d", card, card, &card, card, card, card),
		string(wire), string(text), string(binary), logged.String(),
	} {
		if strings.Contains(out, "4111") || strings.Contains(out, "987") || !strings.Contains(out, "[card]") {
			t.Errorf("rendered %q", out)
		}
	}
	if err := json.Unmarshal([]byte(`"[card]"`), new(billing.Card)); err == nil {
		t.Error("a redaction decoded as a card")
	}
	card.Zero()
	if card.LastFour() != "1111" {
		t.Error("Zero dropped the display facts")
	}
}
