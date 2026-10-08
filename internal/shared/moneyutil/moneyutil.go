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

// Micros is an amount in millionths of a major currency unit — the system-wide
// internal money unit. Defined type so passing micros where a cents/dollars
// parameter is expected is a compile error (#671).
type Micros int64

// Cents is an amount in hundredths of a major currency unit — the minor unit
// most card rails (NMI, Stripe) charge in for 2-decimal currencies.
type Cents int64

// GAP-12 / or#863. Unit changes are typed, so handing cents to a micros
// parameter (or the reverse) is a compile error rather than a mischarge.
//
// The internal->rail direction lives in currency.go and is currency-aware
// (NativeToRailMinor / NativeToRailMinorExact). The currency-BLIND
// MicrosToCentsCeil/MicrosToCentsExact that 14 provider boundaries used to
// call are DELETED, not deprecated: an exported converter that cannot see the
// currency is a converter that cannot refuse an amount whose currency nobody
// established, and it is not reintroducible if it does not exist.
//
// Deliberately NOT converted: DB columns are
// still bare bigint, most struct fields are still int64, and
// NativeToRailMinor still takes int64 because its input is "internal units at
// the CURRENCY's registered scale" (JPY is 10^4), which is not always micros —
// typing it Micros would be a lie.

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
