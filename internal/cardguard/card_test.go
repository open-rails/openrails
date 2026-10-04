package cardguard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
)

// Gateway test card (NMI's documented Visa), never a real one.
const (
	testNumber = "4111111111111111"
	testCVC    = "999"
)

func testCard(t *testing.T) *Card {
	t.Helper()
	var card Card
	require.NoError(t, json.Unmarshal([]byte(`{"number":"4111 1111 1111 1111","exp_month":10,"exp_year":2027,"cvc":"999"}`), &card))
	return &card
}

type probeKey struct{}

type cardHolder struct {
	Card    *Card
	Value   Card
	private Card
	pointer *Card
}

// A card never appears in a formatter, encoder or logger, however it is held.
func TestCardRedactsItself(t *testing.T) {
	card := testCard(t)
	holder := cardHolder{Card: card, Value: *card, private: *card, pointer: card}
	var logs bytes.Buffer
	text := log.New()
	text.SetOutput(&logs)
	text.WithField("card", card).WithField("holder", holder).WithError(fmt.Errorf("wrap %v", card)).Info("card")
	structured := log.New()
	structured.SetOutput(&logs)
	structured.SetFormatter(&log.JSONFormatter{})
	structured.WithField("card", card).WithField("holder", holder).Info("card")
	slog.New(slog.NewJSONHandler(&logs, nil)).Info("card", "card", card, "value", *card, "holder", holder)
	slog.New(slog.NewTextHandler(&logs, nil)).Info("card", "card", card, "value", *card, "holder", holder)

	encoded, err := json.Marshal(map[string]any{"card": card, "value": *card, "holder": holder})
	require.NoError(t, err)
	require.JSONEq(t, `{"card":"[card]","value":"[card]","holder":{"Card":"[card]","Value":"[card]"}}`, string(encoded))
	textual, err := card.MarshalText()
	require.NoError(t, err)

	rendered := []string{logs.String(), string(encoded), string(textual), card.String(), card.GoString(),
		fmt.Sprint(context.WithValue(WithCard(context.Background(), card), probeKey{}, "x"))}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%d", "%x", "%X", "%T%v"} {
		rendered = append(rendered, fmt.Sprintf(verb, card), fmt.Sprintf(verb, *card), fmt.Sprintf(verb, holder), fmt.Sprintf(verb, &holder),
			fmt.Sprintf(verb, []*Card{card}), fmt.Sprintf(verb, map[string]Card{"k": *card}))
	}
	// Every way a printer could render the number or code had it reached the
	// bytes: text, quoted, byte lists (%v, %d, %#v) and hex. A holder's
	// unexported card prints as a pointer address, so bare digits are not
	// matched.
	for _, out := range rendered {
		for _, leak := range []string{testNumber, "4111 1111", "52 49 49 49", "0x34, 0x31, 0x31", "34313131",
			`"` + testCVC + `"`, "cvc:" + testCVC, "57 57 57", "0x39, 0x39, 0x39", "393939"} {
			require.NotContains(t, strings.ToLower(out), leak)
		}
	}
	require.Contains(t, fmt.Sprintf("%+v", card), Redacted)
}

func TestCardDecodesAndWipes(t *testing.T) {
	card := testCard(t)
	require.Equal(t, []string{"visa", "1111", "10/27"}, []string{card.Brand(), card.LastFour(), card.Expiry()})

	var number, cvc []byte
	require.True(t, Unseal(card, func(n, c []byte, month, year int) {
		number, cvc = n, c
		require.Equal(t, []any{testNumber, testCVC, 10, 2027}, []any{string(n), string(c), month, year})
	}))
	card.Zero()
	card.Zero()
	require.Equal(t, make([]byte, 16), number, "the number's own bytes are wiped")
	require.Equal(t, make([]byte, 3), cvc)
	require.False(t, Unseal(card, func([]byte, []byte, int, int) { t.Fatal("a wiped card is not read") }))
	require.Equal(t, []string{"visa", "1111", "10/27"}, []string{card.Brand(), card.LastFour(), card.Expiry()})

	var none *Card
	none.Zero()
	require.False(t, Unseal(none, nil))
	require.Empty(t, none.LastFour()+none.Brand()+none.Expiry())
	require.Nil(t, CardFrom(WithCard(context.Background(), nil)))
	require.Same(t, card, CardFrom(WithCard(context.Background(), card)))

	two, err := NewCard("5431-1111-1111-1111", 1, 30, "0123")
	require.NoError(t, err)
	require.Equal(t, []string{"mastercard", "1111", "01/30"}, []string{two.Brand(), two.LastFour(), two.Expiry()})
}

// A refused card names the field and never echoes what was sent.
func TestCardRefusalsNeverEchoInput(t *testing.T) {
	for body, field := range map[string]string{
		`{"number":"4111111111111112","exp_month":10,"exp_year":2027,"cvc":"999"}`:           "number", // Luhn
		`{"number":"4111","exp_month":10,"exp_year":2027,"cvc":"999"}`:                       "number",
		`{"number":"4111x11111111111","exp_month":10,"exp_year":2027,"cvc":"999"}`:           "number",
		`{"number":"41111111111111111","exp_month":10,"exp_year":2027,"cvc":"999"}`:          "number", // escapes
		`{"number":4111111111111111,"exp_month":10,"exp_year":2027,"cvc":"999"}`:             "number",
		`{"number":"4111111111111111","exp_month":13,"exp_year":2027,"cvc":"999"}`:           "exp_month",
		`{"number":"4111111111111111","exp_month":10,"exp_year":1999,"cvc":"999"}`:           "exp_year",
		`{"number":"4111111111111111","exp_month":10,"exp_year":2027,"cvc":"99"}`:            "cvc",
		`{"number":"4111111111111111","exp_month":10,"exp_year":2027,"cvc":999}`:             "cvc",
		`{"number":"4111111111111111","exp_month":10,"exp_year":2027}`:                       "cvc",
		`{"number":"4111111111111111","exp_month":10,"exp_year":2027,"cvc":"999","zip":"1"}`: "",
		`"4111111111111111"`: "",
	} {
		var card Card
		err := json.Unmarshal([]byte(body), &card)
		var refused *CardError
		require.ErrorAs(t, err, &refused, body)
		require.Equal(t, field, refused.Field, body)
		require.NotContains(t, err.Error(), "4111")
		require.NotContains(t, refused.ClientSafeBindMessage(), "999")
		require.False(t, Unseal(&card, func([]byte, []byte, int, int) {}), "a refused card holds nothing")
	}

	// Years may be two digits; month and year may be numeric strings.
	var card Card
	require.NoError(t, json.Unmarshal([]byte(`{"number":"4111111111111111","exp_month":"03","exp_year":"29","cvc":"999"}`), &card))
	require.Equal(t, "03/29", card.Expiry())

	// A card re-encoded by anything arrives as its redaction and is refused as such.
	relayed, err := json.Marshal(struct {
		Card *Card `json:"card"`
	}{testCard(t)})
	require.NoError(t, err)
	var back struct {
		Card *Card `json:"card"`
	}
	require.ErrorIs(t, json.Unmarshal(relayed, &back), ErrCardRedacted)
	require.True(t, errors.Is(ErrCardRedacted, ErrCardRedacted))
}

// Only the gateway integration reads a card's number and security code.
func TestOnlyTheGatewayIntegrationUnsealsCards(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)
	allowed := []string{"internal/cardguard/", "internal/integrations/nmi/"}
	require.NoError(t, filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == "node_modules" || name == "testdata" || (strings.HasPrefix(name, ".") && path != root) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		for _, prefix := range allowed {
			if strings.HasPrefix(rel, prefix) {
				return nil
			}
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		ast.Inspect(file, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "Unseal" {
				t.Errorf("%s reads a card with Unseal; only internal/integrations/nmi may", rel)
			}
			return true
		})
		return nil
	}))
}
