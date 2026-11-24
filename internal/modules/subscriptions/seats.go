package subscriptions

import (
	"errors"
	"fmt"
	"math"

	"github.com/open-rails/openrails/catalog"
)

// ErrSeatAmountOverflow refuses a unit price times quantity beyond int64.
var ErrSeatAmountOverflow = errors.New("seat amount exceeds int64")

// SeatAmount is unit × quantity, or the unit for a price without seats,
// refusing a quantity outside 1 to catalog.MaxQuantity and overflow.
func SeatAmount(unit int64, quantity *int) (int64, error) {
	if quantity == nil {
		return unit, nil
	}
	return Seats(unit, *quantity)
}

// Seats is unit × n, refusing n outside 1 to catalog.MaxQuantity and
// overflow.
func Seats(unit int64, n int) (int64, error) {
	if n < 1 || n > catalog.MaxQuantity || unit < 0 {
		return 0, fmt.Errorf("seat amount needs a nonnegative unit and 1 to %d seats", catalog.MaxQuantity)
	}
	if unit > 0 && int64(n) > math.MaxInt64/unit {
		return 0, ErrSeatAmountOverflow
	}
	return unit * int64(n), nil
}

// SeatCount is a quantity's seats, one for a price without seats.
func SeatCount(quantity *int) int {
	if quantity == nil {
		return 1
	}
	return *quantity
}

// SameQuantity compares two optional quantities by value.
func SameQuantity(a, b *int) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// CloneQuantity copies an optional quantity.
func CloneQuantity(q *int) *int {
	if q == nil {
		return nil
	}
	v := *q
	return &v
}
