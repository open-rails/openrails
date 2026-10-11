package billing

import (
	"encoding/json"
)

// UpdateMerchantConfigurationParams merges into the merchant's configuration:
// omitted fields keep their values. ExpectedRevision, when set, is the
// revision the caller read; a configuration changed since is refused with
// revision_mismatch. Credentials and PSPs use the PSP methods.
type UpdateMerchantConfigurationParams struct {
	ExpectedRevision *int64            `json:"expected_revision"`
	Settings         *MerchantSettings `json:"settings,omitempty"`
	DisplayName      *string           `json:"display_name,omitempty"`
}

// MarshalJSON preserves explicit empty policy lists across both Client
// transports. MerchantSettings omitempty tags otherwise turn a clear into
// omission, which means keep.
func (p UpdateMerchantConfigurationParams) MarshalJSON() ([]byte, error) {
	type plain UpdateMerchantConfigurationParams
	body, err := json.Marshal(plain(p))
	if err != nil || p.Settings == nil {
		return body, err
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(body, &document); err != nil {
		return nil, err
	}
	var settings map[string]json.RawMessage
	if err := json.Unmarshal(document["settings"], &settings); err != nil {
		return nil, err
	}
	for name, present := range map[string]bool{
		"billing_policies":                      p.Settings.BillingPolicies != nil,
		"billing_policy_bindings":               p.Settings.BillingPolicyBindings != nil,
		"delegated_invoker_wasted_spend_limits": p.Settings.DelegatedInvokerWastedSpendLimits != nil,
	} {
		if _, exists := settings[name]; present && !exists {
			settings[name] = json.RawMessage("[]")
		}
	}
	document["settings"], err = json.Marshal(settings)
	if err != nil {
		return nil, err
	}
	return json.Marshal(document)
}

// MerchantConfigurationState is the merchant's configuration, never its
// credentials. Revision advances with every change; an update may name it.
type MerchantConfigurationState struct {
	Revision    int64            `json:"revision"`
	DisplayName string           `json:"display_name"`
	Settings    MerchantSettings `json:"settings"`
}

// Capabilities is what this deployment serves: each optional route group
// (admin, catalog, merchant_config, metrics, app), on or off, and the optional
// features its configuration enables.
type Capabilities struct {
	RouteGroups map[string]bool `json:"route_groups"`
	Features    map[string]bool `json:"features"`
}
