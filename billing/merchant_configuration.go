package openrails

import "encoding/json"

// MerchantConfigurationApplyParams is an explicit, replayable metadata update.
// Omitted fields preserve stored values. Credentials and provider lifecycle
// changes use PaymentProviders and their separate publication receipts.
type MerchantConfigurationApplyParams struct {
	ApplicationID    string            `json:"application_id"`
	ExpectedRevision *string           `json:"expected_revision"`
	Settings         *MerchantSettings `json:"settings,omitempty"`
	DisplayName      *string           `json:"display_name,omitempty"`
	APIHost          *string           `json:"api_host,omitempty"`
}

// MarshalJSON preserves explicit empty policy lists across both Client
// transports. MerchantSettings omitempty tags otherwise turn a clear into
// omission, which means preserve for a metadata application.
func (p MerchantConfigurationApplyParams) MarshalJSON() ([]byte, error) {
	type plain MerchantConfigurationApplyParams
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

// MerchantConfigurationReceipt records a committed metadata application. Replay
// returns the original revision even when later operations changed metadata.
type MerchantConfigurationReceipt struct {
	ApplicationID string `json:"application_id"`
	Revision      string `json:"revision"`
	Replayed      bool   `json:"replayed"`
}

// MerchantConfigurationState contains current non-secret metadata and an opaque
// revision for optimistic concurrency. Merchant selection does not grant access.
type MerchantConfigurationState struct {
	Revision    string           `json:"revision"`
	DisplayName string           `json:"display_name"`
	APIHost     string           `json:"api_host"`
	Settings    MerchantSettings `json:"settings"`
}
