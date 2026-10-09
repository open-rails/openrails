package grants

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
)

// DecodeAcceptedEntitlements reads the keys an operation admitted before
// product access, retained only to reproduce its accepted fingerprint. New
// operations admit no keys: settlement grants the product.
func DecodeAcceptedEntitlements(raw json.RawMessage, retained map[string]*int) ([]string, map[string]*int, error) {
	raw = bytes.TrimSpace(raw)
	var names []string
	if len(raw) == 0 || string(raw) == "null" {
		return nil, retained, nil
	}
	if raw[0] == '{' {
		if err := json.Unmarshal(raw, &retained); err != nil {
			return nil, nil, err
		}
		names = make([]string, 0, len(retained))
		for name := range retained {
			names = append(names, name)
		}
		slices.Sort(names)
	} else if err := json.Unmarshal(raw, &names); err != nil {
		return nil, nil, err
	}
	if retained != nil {
		if len(names) != len(retained) {
			return nil, nil, errors.New("historical entitlement snapshot does not match its names")
		}
		for _, name := range names {
			if _, ok := retained[name]; !ok {
				return nil, nil, errors.New("historical entitlement snapshot does not match its names")
			}
		}
	}
	return names, retained, nil
}

// AcceptedEntitlementValue is used only to reproduce an admitted provider or
// quote fingerprint. Current catalog terms always select the list shape.
func AcceptedEntitlementValue(names []string, historical map[string]*int) any {
	if historical != nil {
		return historical
	}
	return names
}

// AcceptedEntitlementsValue reproduces the keys an operation admitted before
// product access, in the shape its fingerprint hashed. nil: the operation
// admitted none, and its fingerprint omits them.
func AcceptedEntitlementsValue(raw json.RawMessage, legacy map[string]*int) (any, error) {
	if len(bytes.TrimSpace(raw)) == 0 && legacy == nil {
		return nil, nil
	}
	names, historical, err := DecodeAcceptedEntitlements(raw, legacy)
	if err != nil {
		return nil, err
	}
	if historical != nil {
		return historical, nil
	}
	if names == nil {
		names = []string{}
	}
	return names, nil
}
