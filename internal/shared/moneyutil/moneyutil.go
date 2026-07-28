package moneyutil

import (
	"fmt"
	"math/big"
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

func ParseDecimalToCents(value string) (int64, error) {
	return parseDecimalScaled(value, CentsPerMajorUnit)
}

func parseDecimalScaled(value string, scale int64) (int64, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return 0, fmt.Errorf("amount is empty")
	}

	parsed, ok := new(big.Rat).SetString(trimmed)
	if !ok {
		return 0, fmt.Errorf("invalid decimal amount %q", trimmed)
	}

	scaled := new(big.Rat).Mul(parsed, big.NewRat(scale, 1))
	return roundHalfAwayFromZero(scaled)
}

func CentsToMicros(cents int64) int64 {
	return cents * MicrosPerCent
}

func MicrosToCentsCeil(micros int64) int64 {
	if micros <= 0 {
		return 0
	}
	return (micros + MicrosPerCent - 1) / MicrosPerCent
}

func MicrosToCentsExact(micros int64) (int64, error) {
	if micros%MicrosPerCent != 0 {
		return 0, fmt.Errorf("amount %d micros is not representable in whole cents", micros)
	}
	return micros / MicrosPerCent, nil
}

func FormatMicrosDecimal(micros int64) string {
	return formatDecimal(micros, MicrosPerMajorUnit, 6)
}

func FormatCentsDecimal(cents int64) string {
	return formatDecimal(cents, CentsPerMajorUnit, 2)
}

func formatDecimal(amount, scale int64, width int) string {
	negative := amount < 0
	// Work entirely in int64. |MinInt64/100| and |MinInt64%100| both fit in int64,
	// so negating the quotient/remainder to their magnitudes never overflows
	// (unlike negating amount itself at math.MinInt64) — and there is no int64↔uint64
	// conversion for gosec G115 to flag.
	major := amount / scale
	minor := amount % scale
	if major < 0 {
		major = -major
	}
	if minor < 0 {
		minor = -minor
	}
	if negative {
		return fmt.Sprintf("-%d.%0*d", major, width, minor)
	}
	return fmt.Sprintf("%d.%0*d", major, width, minor)
}

func FormatDisplay(micros int64, currency string) string {
	code := strings.ToUpper(strings.TrimSpace(currency))
	amount := FormatMicrosDecimal(micros)
	if code == "" {
		return amount
	}
	if code == "USD" {
		if strings.HasPrefix(amount, "-") {
			return fmt.Sprintf("-$%s %s", strings.TrimPrefix(amount, "-"), code)
		}
		return fmt.Sprintf("$%s %s", amount, code)
	}
	return fmt.Sprintf("%s %s", amount, code)
}

func FormatUSD(micros int64) string {
	amount := FormatMicrosDecimal(micros)
	if strings.HasPrefix(amount, "-") {
		return "-$" + strings.TrimPrefix(amount, "-")
	}
	return "$" + amount
}

func roundHalfAwayFromZero(value *big.Rat) (int64, error) {
	if value == nil {
		return 0, fmt.Errorf("value is nil")
	}
	if value.Sign() == 0 {
		return 0, nil
	}

	sign := value.Sign()
	num := new(big.Int).Abs(value.Num())
	den := new(big.Int).Set(value.Denom())

	quotient, remainder := new(big.Int), new(big.Int)
	quotient.QuoRem(num, den, remainder)

	twiceRemainder := new(big.Int).Lsh(remainder, 1)
	if twiceRemainder.Cmp(den) >= 0 {
		quotient.Add(quotient, big.NewInt(1))
	}

	if !quotient.IsInt64() {
		return 0, fmt.Errorf("amount is out of int64 range")
	}

	rounded := quotient.Int64()
	if sign < 0 {
		rounded = -rounded
	}
	return rounded, nil
}
