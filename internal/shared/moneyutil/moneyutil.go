package moneyutil

import (
	"fmt"
	"strconv"
	"strings"
)

const (
	MicrosPerMajorUnit = int64(1_000_000)
	CentsPerMajorUnit  = int64(100)
	MicrosPerCent      = MicrosPerMajorUnit / CentsPerMajorUnit
)

// Micros is an amount in millionths of a major currency unit, the internal
// money unit. A defined type, so mixing it with Cents is a compile error.
type Micros int64

// Cents is an amount in hundredths of a major currency unit — the minor unit
// most card rails (NMI, Stripe) charge in for 2-decimal currencies.
type Cents int64

// Internal->rail conversions are currency-aware (NativeToRailMinor); there is
// deliberately no currency-blind micros->cents converter. NativeToRailMinor
// takes int64, not Micros: its input is at the currency's registered scale,
// which is not always micros.

// CentsToMicros widens a fiat card-rail minor amount into native units. The
// registered fiat currencies share this shift; crypto uses native atomic units
// directly and must go through RailMinorToNative instead.
func CentsToMicros(cents Cents) Micros {
	return Micros(int64(cents) * MicrosPerCent)
}

func FormatMicrosDecimal(micros Micros) string {
	return formatDecimal(int64(micros), MicrosPerMajorUnit, 6)
}

func FormatCentsDecimal(cents Cents) string {
	return formatDecimal(int64(cents), CentsPerMajorUnit, 2)
}

func formatDecimal(amount, scale int64, width int) string {
	// Never negate amount: -MinInt64 overflows. |amount%scale| < scale always
	// fits, and FormatInt renders the quotient's magnitude exactly.
	whole := strings.TrimPrefix(strconv.FormatInt(amount/scale, 10), "-")
	minor := amount % scale
	sign := ""
	if amount < 0 {
		sign = "-"
		minor = -minor
	}
	if width == 0 {
		return sign + whole
	}
	return fmt.Sprintf("%s%s.%0*d", sign, whole, width, minor)
}

func FormatUSD(micros Micros) string {
	amount := FormatMicrosDecimal(micros)
	if strings.HasPrefix(amount, "-") {
		return "-$" + strings.TrimPrefix(amount, "-")
	}
	return "$" + amount
}
