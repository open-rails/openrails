package catalog

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// MaxQuantity bounds the seats of a per-seat price.
const MaxQuantity = 10000

// Quantity makes a recurring price per seat: its unit amount is one seat's,
// and a subscription holds Min to Max seats (inclusive). A price without it
// has no quantity.
type Quantity struct {
	Min int `json:"min"`
	Max int `json:"max"`
}

// Validate refuses bounds outside 1 to MaxQuantity or out of order.
func (q Quantity) Validate() error {
	if q.Min < 1 || q.Max < q.Min || q.Max > MaxQuantity {
		return fmt.Errorf("quantity needs 1 <= min <= max <= %d", MaxQuantity)
	}
	return nil
}

// Allows reports whether n seats are within the bounds.
func (q Quantity) Allows(n int) bool { return n >= q.Min && n <= q.Max }

func (q *Quantity) UnmarshalJSON(raw []byte) error {
	var value struct {
		Min int `json:"min"`
		Max int `json:"max"`
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&value); err != nil {
		return err
	}
	*q = Quantity(value)
	return nil
}
