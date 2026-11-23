package catalog

import "fmt"

// Ownership is how many of a product one customer may hold at once.
type Ownership string

const (
	// OwnershipUnique: one live holding per customer (per tier group when the
	// product has one). A recurring product is then one subscription, with
	// seats as its quantity.
	OwnershipUnique Ownership = "unique"
	// OwnershipConsumable: purchases stack; quantity counts units.
	OwnershipConsumable Ownership = "consumable"
	// OwnershipExtend: a later purchase starts when the current window ends.
	OwnershipExtend Ownership = "extend"
)

// Validate refuses an unknown rule; "" is undeclared.
func (o Ownership) Validate() error {
	switch o {
	case "", OwnershipUnique, OwnershipConsumable, OwnershipExtend:
		return nil
	}
	return fmt.Errorf("ownership %q is not unique, consumable or extend", o)
}
