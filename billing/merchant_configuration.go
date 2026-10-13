package billing

import (
	"encoding/json"
)

// UpdateMerchantConfigurationParams merges into the merchant's configuration:
// omitted fields keep their values. ExpectedRevision, when set, is the
// revision the caller read; a configuration changed since is refused with
// revision_mismatch. Credentials and PSPs use the PSP methods.
type UpdateMerchantConfigurationParams struct {
	ExpectedRevision *int64  `json:"expected_revision"`
	DisplayName      *string `json:"display_name,omitempty"`
	// Sandbox makes the merchant a sandbox, or live; refused while a PSP it
	// serves would disagree.
	Sandbox *bool `json:"sandbox,omitempty"`
	// MerchantSettings are the settings to merge, beside display_name; nil
	// keeps them all.
	*MerchantSettings
}

// MarshalJSON preserves explicit empty policy lists across both Client
// transports. MerchantSettings omitempty tags otherwise turn a clear into
// omission, which means keep.
func (p UpdateMerchantConfigurationParams) MarshalJSON() ([]byte, error) {
	type plain UpdateMerchantConfigurationParams
	body, err := json.Marshal(plain(p))
	if err != nil || p.MerchantSettings == nil {
		return body, err
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(body, &document); err != nil {
		return nil, err
	}
	for name, present := range map[string]bool{
		"billing_policies":                      p.BillingPolicies != nil,
		"billing_policy_bindings":               p.BillingPolicyBindings != nil,
		"delegated_invoker_wasted_spend_limits": p.DelegatedInvokerWastedSpendLimits != nil,
	} {
		if _, exists := document[name]; present && !exists {
			document[name] = json.RawMessage("[]")
		}
	}
	return json.Marshal(document)
}

// MerchantConfigurationState is the merchant's configuration, never its
// credentials. Revision advances with every change; an update may name it.
type MerchantConfigurationState struct {
	Revision    int64  `json:"revision"`
	DisplayName string `json:"display_name"`
	// Sandbox is whether the merchant is a sandbox: its PSPs move no money.
	Sandbox bool `json:"sandbox"`
	MerchantSettings
	// PlatformVault is whether card_custody platform can be chosen.
	PlatformVault PlatformVaultState `json:"platform_vault"`
}

// PlatformVaultState is the deployment's platform vault as a merchant sees
// it: Available when one is configured for the deployment's posture.
type PlatformVaultState struct {
	Available bool `json:"available"`
}

// Capabilities is what this deployment serves: each optional route group
// (admin, catalog, merchant_config, metrics, app), on or off, and the optional
// features its configuration enables.
type Capabilities struct {
	RouteGroups map[string]bool `json:"route_groups"`
	Features    map[string]bool `json:"features"`
}
