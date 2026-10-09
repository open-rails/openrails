package subscriptions

import (
	"errors"
	"math"
)

// ErrSeatAmountOverflow refuses a unit price times quantity beyond int64.
var ErrSeatAmountOverflow = errors.New("seat amount exceeds int64")

// SeatAmount is unit × quantity, refusing a quantity below 1 and overflow.
func SeatAmount(unit int64, quantity int) (int64, error) {
	if quantity < 1 || unit < 0 {
		return 0, errors.New("seat amount needs a nonnegative unit and at least one seat")
	}
	if unit > 0 && int64(quantity) > math.MaxInt64/unit {
		return 0, ErrSeatAmountOverflow
	}
	return unit * int64(quantity), nil
}
