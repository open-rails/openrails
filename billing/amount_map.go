package openrails

import (
	"encoding/json"
	"fmt"
	"strconv"
)

// AmountMap contains native monetary units. Each JSON value is a signed decimal
// string so JSON consumers retain the full int64 range without float rounding.
type AmountMap map[string]int64

func (m AmountMap) MarshalJSON() ([]byte, error) {
	if m == nil {
		return []byte("null"), nil
	}
	out := make(map[string]string, len(m))
	for key, value := range m {
		out[key] = strconv.FormatInt(value, 10)
	}
	return json.Marshal(out)
}

func (m *AmountMap) UnmarshalJSON(raw []byte) error {
	var values map[string]string
	if err := json.Unmarshal(raw, &values); err != nil {
		return err
	}
	if values == nil {
		*m = nil
		return nil
	}
	out := make(AmountMap, len(values))
	for key, encoded := range values {
		value, err := strconv.ParseInt(encoded, 10, 64)
		if err != nil {
			return fmt.Errorf("amount %q: %w", key, err)
		}
		out[key] = value
	}
	*m = out
	return nil
}
