package openrails

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/open-rails/openrails/internal/configdocument"
)

const MaxMerchantConfigurationBytes = 1 << 20

// ParseMerchantConfigurationYAML accepts one bounded document without YAML
// aliases, anchors, tags, duplicate fields or unknown schema fields.
func ParseMerchantConfigurationYAML(raw []byte) (*MerchantConfigurationApplyParams, error) {
	body, err := configdocument.YAMLToJSON(raw, MaxMerchantConfigurationBytes)
	if err != nil {
		return nil, err
	}
	return ParseMerchantConfigurationJSON(body)
}

func ParseMerchantConfigurationJSON(raw []byte) (*MerchantConfigurationApplyParams, error) {
	if err := configdocument.GuardJSON(raw, MaxMerchantConfigurationBytes); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var params MerchantConfigurationApplyParams
	if err := decoder.Decode(&params); err != nil {
		return nil, err
	}
	if strings.TrimSpace(params.ApplicationID) == "" || len(params.ApplicationID) > 128 || params.ExpectedRevision == nil || strings.TrimSpace(*params.ExpectedRevision) == "" {
		return nil, fmt.Errorf("application_id and expected_revision are required")
	}
	return &params, nil
}
