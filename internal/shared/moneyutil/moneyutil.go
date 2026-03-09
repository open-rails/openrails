package moneyutil

import (
	"fmt"
	"math/big"
	"strings"
)

func ParseDecimalToCents(value string) (int64, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return 0, fmt.Errorf("amount is empty")
	}

	parsed, ok := new(big.Rat).SetString(trimmed)
	if !ok {
		return 0, fmt.Errorf("invalid decimal amount %q", trimmed)
	}

	scaled := new(big.Rat).Mul(parsed, big.NewRat(100, 1))
	return roundHalfAwayFromZero(scaled)
}

func CentsToMajorUnits(cents int64) float64 {
	return float64(cents) / 100.0
}

func FormatCentsDecimal(cents int64) string {
	negative := cents < 0
	abs := uint64(cents)
	if negative {
		abs = uint64(-(cents + 1)) + 1
	}

	major := abs / 100
	minor := abs % 100
	if cents < 0 {
		return fmt.Sprintf("-%d.%02d", major, minor)
	}
	return fmt.Sprintf("%d.%02d", major, minor)
}

func FormatDisplay(cents int64, currency string) string {
	code := strings.ToUpper(strings.TrimSpace(currency))
	amount := FormatCentsDecimal(cents)
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

func FormatUSD(cents int64) string {
	amount := FormatCentsDecimal(cents)
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
