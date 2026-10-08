package billing

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// UnmarshalJSON distinguishes omitted entitlements from an invalid null list.
// An explicit empty list declares a product with no entitlements.
func (p *CreateProductParams) UnmarshalJSON(raw []byte) error {
	type product CreateProductParams
	var value product
	decoded := struct {
		*product
		Entitlements json.RawMessage `json:"entitlements"`
	}{product: &value}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		return err
	}
	if decoded.Entitlements != nil {
		if bytes.Equal(bytes.TrimSpace(decoded.Entitlements), []byte("null")) {
			return fmt.Errorf("entitlements must be a string list, not null; use [] for none")
		}
		if err := json.Unmarshal(decoded.Entitlements, &value.Entitlements); err != nil {
			return fmt.Errorf("entitlements: %w", err)
		}
	}
	*p = CreateProductParams(value)
	return nil
}
