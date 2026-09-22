package openrails

// Path identifiers are validated at the SDK boundary without requiring callers
// to construct transport-specific ID wrappers. Domain IDs remain typed internally.
func requireCustomerID(value string) (string, error) {
	id, err := ParseCustomerID(value)
	if err != nil {
		return "", invalidErr("invalid customer_id")
	}
	return requireTypedID("customer_id", id)
}

func requirePriceID(value string) (string, error) {
	id, err := ParsePriceID(value)
	if err != nil {
		return "", invalidErr("invalid price_id")
	}
	return requireTypedID("price_id", id)
}
