package currency

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// KindUnit is a merchant's own unit (credits, tokens, an in-game currency):
// a balance, never money.
const KindUnit = "unit"

// Bounds of a merchant's units.
const (
	MaxUnitScale      = 6
	MaxUnits          = 32
	maxUnitNameRunes  = 64
	maxUnitSymbolRune = 8
)

// unitCode is a unit's code: four to twelve upper-case letters or digits,
// starting with a letter. Three-letter codes are ISO 4217's.
var unitCode = regexp.MustCompile(`^[A-Z][A-Z0-9]{3,11}$`)

// Refusals of a code outside OpenRails' registry.
var (
	ErrUnknown      = errors.New("unknown currency")
	ErrMerchantUnit = errors.New("a merchant's own unit, not money")
)

// Unit validates a merchant's unit. Its amounts are integers of 10^-scale
// units, so scale 0 counts whole units; the symbol defaults to the code.
func Unit(code, name, symbol string, scale int) (Units, error) {
	if !unitCode.MatchString(code) {
		return Units{}, fmt.Errorf("code %q must be 4 to 12 upper-case letters or digits starting with a letter", code)
	}
	if _, ok := registry[code]; ok {
		return Units{}, fmt.Errorf("code %s is a currency in OpenRails' registry", code)
	}
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > maxUnitNameRunes || !displayable(name) {
		return Units{}, fmt.Errorf("%s: name must be 1 to %d printable characters", code, maxUnitNameRunes)
	}
	if symbol = strings.TrimSpace(symbol); symbol == "" {
		symbol = code
	}
	if utf8.RuneCountInString(symbol) > maxUnitSymbolRune || !displayable(symbol) {
		return Units{}, fmt.Errorf("%s: symbol must be at most %d printable characters", code, maxUnitSymbolRune)
	}
	if strings.ContainsFunc(symbol, func(r rune) bool { return unicode.Is(unicode.Sc, r) }) {
		return Units{}, fmt.Errorf("%s: symbol must not contain a currency sign: a unit is not money", code)
	}
	if scale < 0 || scale > MaxUnitScale {
		return Units{}, fmt.Errorf("%s: scale must be 0 to %d", code, MaxUnitScale)
	}
	return Units{Code: code, Decimals: scale, MinorDecimals: scale, Kind: KindUnit, Name: name, Symbol: symbol}, nil
}

func displayable(s string) bool {
	return utf8.ValidString(s) && !strings.ContainsFunc(s, func(r rune) bool {
		return unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r)
	})
}

// Registry is the currencies one merchant names: OpenRails' registry and the
// merchant's own units. The zero Registry is OpenRails' alone.
type Registry struct {
	units map[string]Units
}

// NewRegistry is OpenRails' registry with a merchant's units, each from Unit.
func NewRegistry(units []Units) (Registry, error) {
	if len(units) > MaxUnits {
		return Registry{}, fmt.Errorf("at most %d units", MaxUnits)
	}
	out := Registry{units: make(map[string]Units, len(units))}
	for _, u := range units {
		if u.Kind != KindUnit {
			return Registry{}, fmt.Errorf("%s is not a merchant unit", u.Code)
		}
		if _, dup := out.units[u.Code]; dup {
			return Registry{}, fmt.Errorf("unit %s is declared twice", u.Code)
		}
		out.units[u.Code] = u
	}
	return out, nil
}

// Lookup is a registry currency or one of the merchant's units,
// case-insensitively.
func (r Registry) Lookup(code string) (Units, bool) {
	if u, ok := Lookup(code); ok {
		return u, true
	}
	u, ok := r.units[strings.ToUpper(strings.TrimSpace(code))]
	return u, ok
}

// Money is the registry currency code names: one of the merchant's units is
// ErrMerchantUnit, any other code ErrUnknown.
func (r Registry) Money(code string) (Units, error) {
	u, ok := r.Lookup(code)
	switch {
	case !ok:
		return Units{}, fmt.Errorf("%w %q", ErrUnknown, code)
	case u.Kind == KindUnit:
		return Units{}, fmt.Errorf("%s is %w", u.Code, ErrMerchantUnit)
	}
	return u, nil
}

// Units lists the merchant's units in code order.
func (r Registry) Units() []Units {
	out := make([]Units, 0, len(r.units))
	for _, u := range r.units {
		out = append(out, u)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Code < out[j].Code })
	return out
}
