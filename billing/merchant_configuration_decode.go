package billing

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/open-rails/openrails/internal/configdocument"
)

// MaxMerchantConfigurationBytes bounds a merchant configuration document.
const MaxMerchantConfigurationBytes = 1 << 20

// ParseMerchantConfigurationYAML reads one UpdateMerchantConfiguration document,
// YAML or JSON, bounded and without aliases, anchors, tags, duplicate fields
// or unknown fields.
func ParseMerchantConfigurationYAML(raw []byte) (*UpdateMerchantConfigurationParams, error) {
	body, err := configdocument.YAMLToJSON(raw, MaxMerchantConfigurationBytes)
	if err != nil {
		return nil, err
	}
	return parseMerchantConfigurationJSON(body)
}

func parseMerchantConfigurationJSON(raw []byte) (*UpdateMerchantConfigurationParams, error) {
	if err := configdocument.GuardJSON(raw, MaxMerchantConfigurationBytes); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var params UpdateMerchantConfigurationParams
	if err := decoder.Decode(&params); err != nil {
		return nil, err
	}
	if params.ExpectedRevision == nil || strings.TrimSpace(*params.ExpectedRevision) == "" {
		return nil, fmt.Errorf("expected_revision is required")
	}
	return &params, nil
}
